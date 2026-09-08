#!/usr/bin/env python3
"""Coral Edge TPU sidecar for the offload harness (ADR 0024 accelerator lane, Coral design D7).

Wire contract — the Hailo sidecar's, verbatim, so the harness's accelclient speaks to both:

    GET  /health                -> 200 {enabled, device, status, temp_c, loaded, models_missing, runtime}
    POST /v1/<tool>  (JSON)     -> 200 result dict          | 200 {"error": ...} structured failure
                                   404 {"error":"unknown_tool"} | 400 {"error":"bad_request", ...}
                                   500 {"error":"internal", ...}
    anything else               -> 404 {"error":"not_found"}

Tools (D5): classify · object_detect · semantic_segment · embed.

Runtime facts this file encodes (SETUP.md §1, §5): ai_edge_litert's Interpreter with
load_delegate("libedgetpu.so.1"); one Interpreter per model kept resident after first use
(the TPU holds one model's weights at a time — switching re-uploads, ~10–60 ms at these
sizes); ONE lock, one in-flight inference; the sidecar READS /sys/class/apex/apex_0/{status,temp}
and never writes sysfs — the thermal knobs stay the operator's (SETUP.md §6.4).

Refusals: binds loopback only (a non-loopback CORAL_BIND is refused at startup); serves only
files listed in models.json whose sha256 matches (an unlisted or mismatched file is a
structured error, never a load); exits itself after CORAL_IDLE_SEC without a call.

CORAL_ENABLED=0 runs the whole HTTP contract with the TPU path stubbed (for test_server.py
on a box without the device); every inference then answers {"error":"tpu_disabled"}.
"""
from __future__ import annotations

import hashlib
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
BIND = os.environ.get("CORAL_BIND", "127.0.0.1")
PORT = int(os.environ.get("CORAL_PORT", "18814"))
IDLE_SEC = int(os.environ.get("CORAL_IDLE_SEC", "300"))
MODELS_DIR = os.environ.get("CORAL_MODELS_DIR", os.path.join(os.path.dirname(HERE), "models"))
MANIFEST = os.environ.get("CORAL_MANIFEST", os.path.join(HERE, "models.json"))
ENABLED = os.environ.get("CORAL_ENABLED", "1") != "0"
DELEGATE_LIB = os.environ.get("CORAL_DELEGATE", "libedgetpu.so.1")
DEVICE = "/dev/apex_0"
SYSFS = "/sys/class/apex/apex_0"

LOOPBACK = {"127.0.0.1", "::1", "localhost"}
if BIND not in LOOPBACK:
    print(f"coral sidecar: refusing to bind non-loopback address {BIND!r}", file=sys.stderr)
    sys.exit(2)

# ---------------------------------------------------------------- manifest

with open(MANIFEST, encoding="utf-8") as fh:
    _MAN = json.load(fh)
MODELS: dict[str, dict] = _MAN["models"]  # key -> {file, sha256, task, input, labels?}

_lock = threading.Lock()  # one in-flight inference, one model switch at a time
_interp: dict[str, object] = {}  # model key -> resident Interpreter
_labels: dict[str, list[str]] = {}
_last_call = time.monotonic()
_started = time.time()


def _sysfs(name: str) -> str | None:
    try:
        with open(os.path.join(SYSFS, name), encoding="utf-8") as fh:
            return fh.read().strip()
    except OSError:
        return None


def _sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _model_path(key: str) -> str:
    """Resolve a manifest key to a verified file; raise ValueError with a structured reason."""
    spec = MODELS.get(key)
    if spec is None:
        raise ValueError({"error": "unknown_model", "model": key, "known": sorted(MODELS)})
    path = os.path.join(MODELS_DIR, spec["file"])
    if not os.path.isfile(path):
        raise ValueError({"error": "model_missing", "model": key, "file": spec["file"],
                          "hint": "run fetch-models.sh"})
    digest = _sha256(path)
    if digest != spec["sha256"]:
        raise ValueError({"error": "model_sha_mismatch", "model": key, "file": spec["file"],
                          "expected": spec["sha256"], "got": digest})
    return path


def _load_labels(key: str) -> list[str]:
    if key in _labels:
        return _labels[key]
    spec = MODELS[key]
    out: list[str] = []
    lf = spec.get("labels")
    if lf:
        with open(os.path.join(MODELS_DIR, lf), encoding="utf-8") as fh:
            for line in fh:
                line = line.rstrip("\n")
                if not line:
                    continue
                # Coral label files are either "id  label" or bare "label" per line.
                parts = line.split(maxsplit=1)
                if len(parts) == 2 and parts[0].isdigit():
                    idx = int(parts[0])
                    while len(out) <= idx:
                        out.append("")
                    out[idx] = parts[1]
                else:
                    out.append(line)
    _labels[key] = out
    return out


# ---------------------------------------------------------------- TPU

def _interpreter(key: str):
    """Resident Interpreter for key, built on first use (holds the lock)."""
    if key in _interp:
        return _interp[key]
    if not ENABLED:
        raise ValueError({"error": "tpu_disabled"})
    path = _model_path(key)
    from ai_edge_litert.interpreter import Interpreter, load_delegate  # imported lazily: test mode has no litert

    it = Interpreter(model_path=path, experimental_delegates=[load_delegate(DELEGATE_LIB)])
    it.allocate_tensors()
    _interp[key] = it
    return it


def _read_image(path: str, size: tuple[int, int]):
    import numpy as np
    from PIL import Image

    if not os.path.isfile(path):
        raise ValueError({"error": "image_missing", "image_path": path})
    img = Image.open(path).convert("RGB")
    w, h = img.size
    arr = np.asarray(img.resize(size, Image.BILINEAR), dtype=np.uint8)
    return arr, (w, h)


def _set_input(it, arr):
    import numpy as np

    d = it.get_input_details()[0]
    x = np.expand_dims(arr, 0)
    if d["dtype"] != np.uint8:
        scale, zero = d["quantization"]
        x = ((x.astype(np.float32) / 255.0) / (scale or 1.0) + zero).astype(d["dtype"])
    it.set_tensor(d["index"], x)


def _out(it, i: int):
    d = it.get_output_details()[i]
    return it.get_tensor(d["index"]), d


def _dequant(t, d):
    import numpy as np

    scale, zero = d["quantization"]
    if scale == 0:
        return t.astype(np.float32)
    return (t.astype(np.float32) - zero) * scale


# ---------------------------------------------------------------- tools

CLASSIFY_DOMAINS = {"imagenet": "efficientnet-s", "birds": "inat-birds", "insects": "inat-insects", "plants": "inat-plants"}
DETECT_SIZES = {"lite0": "efficientdet-lite0", "lite1": "efficientdet-lite1", "lite2": "efficientdet-lite2"}


def tool_classify(args: dict) -> dict:
    image = args.get("image_path")
    if not isinstance(image, str) or not image:
        raise ValueError({"error": "bad_request", "detail": "image_path required"})
    domain = args.get("domain", "imagenet")
    key = CLASSIFY_DOMAINS.get(domain)
    if key is None:
        raise ValueError({"error": "bad_request", "detail": f"domain must be one of {sorted(CLASSIFY_DOMAINS)}"})
    top_k = int(args.get("top_k", 5))
    it = _interpreter(key)
    d = it.get_input_details()[0]
    arr, _ = _read_image(image, (d["shape"][2], d["shape"][1]))
    _set_input(it, arr)
    it.invoke()
    t, od = _out(it, 0)
    scores = _dequant(t[0], od)
    labels = _load_labels(key)
    # softmax when the head is logits (EfficientNet); iNat models already emit probabilities
    if scores.max() > 1.0 or scores.min() < 0.0:
        import numpy as np

        e = np.exp(scores - scores.max())
        scores = e / e.sum()
    order = scores.argsort()[::-1][:top_k]
    results = [{"label": labels[i] if i < len(labels) else str(i), "score": float(scores[i])} for i in order]
    return {"results": results, "best": results[0] if results else None, "model": MODELS[key]["file"], "domain": domain}


def tool_object_detect(args: dict) -> dict:
    image = args.get("image_path")
    if not isinstance(image, str) or not image:
        raise ValueError({"error": "bad_request", "detail": "image_path required"})
    size = args.get("size", "lite0")
    key = DETECT_SIZES.get(size)
    if key is None:
        raise ValueError({"error": "bad_request", "detail": f"size must be one of {sorted(DETECT_SIZES)}"})
    thr = float(args.get("score_threshold", 0.3))
    it = _interpreter(key)
    d = it.get_input_details()[0]
    arr, (w, h) = _read_image(image, (d["shape"][2], d["shape"][1]))
    _set_input(it, arr)
    it.invoke()
    # EfficientDet-Lite (TFLite detection postprocess): boxes [1,N,4] (ymin,xmin,ymax,xmax normalised),
    # classes [1,N], scores [1,N], count [1]. Output order varies by export — match by shape.
    outs = [(_dequant(*_out(it, i)), _out(it, i)[1]["shape"]) for i in range(len(it.get_output_details()))]
    boxes = classes = scores = None
    count = None
    for t, shape in outs:
        if len(shape) == 3 and shape[-1] == 4:
            boxes = t[0]
        elif len(shape) == 2:
            if scores is None and t.dtype.kind == "f" and t.max() <= 1.0001 and t.min() >= -0.0001 and classes is not None:
                scores = t[0]
            elif classes is None and (t.max() > 1.0001 or (t == t.round()).all()):
                classes = t[0]
            else:
                scores = t[0]
        elif len(shape) == 1:
            count = int(t[0])
    if boxes is None or classes is None or scores is None:
        raise ValueError({"error": "internal", "detail": "unexpected detector output layout", "shapes": [list(map(int, s)) for _, s in outs]})
    labels = _load_labels(key)
    n = count if count is not None else len(scores)
    objects = []
    for i in range(min(n, len(scores))):
        s = float(scores[i])
        if s < thr:
            continue
        ymin, xmin, ymax, xmax = [float(v) for v in boxes[i]]
        cid = int(classes[i])
        objects.append({"label": labels[cid] if cid < len(labels) and labels[cid] else str(cid), "class_id": cid,
                        "x": max(0.0, xmin * w), "y": max(0.0, ymin * h),
                        "w": max(0.0, (xmax - xmin) * w), "h": max(0.0, (ymax - ymin) * h), "score": s})
    objects.sort(key=lambda o: -o["score"])
    return {"objects": objects, "count": len(objects), "model": MODELS[key]["file"], "image_width": w, "image_height": h}


def tool_semantic_segment(args: dict) -> dict:
    image = args.get("image_path")
    if not isinstance(image, str) or not image:
        raise ValueError({"error": "bad_request", "detail": "image_path required"})
    key = "deeplab-pascal"
    it = _interpreter(key)
    d = it.get_input_details()[0]
    arr, (w, h) = _read_image(image, (d["shape"][2], d["shape"][1]))
    _set_input(it, arr)
    it.invoke()
    import numpy as np
    from PIL import Image

    t, od = _out(it, 0)
    t = t[0]
    mask = t.argmax(-1).astype(np.uint8) if t.ndim == 3 else t.astype(np.uint8)
    labels = _load_labels(key)
    out_path = args.get("out_path") or (os.path.splitext(image)[0] + ".segmask.png")
    Image.fromarray(mask, mode="L").resize((w, h), Image.NEAREST).save(out_path)
    ids, counts = np.unique(mask, return_counts=True)
    classes = [{"class_id": int(i), "label": labels[int(i)] if int(i) < len(labels) else str(int(i)), "pixels": int(c)}
               for i, c in zip(ids, counts)]
    return {"mask_path": out_path, "classes": classes, "width": w, "height": h, "model": MODELS[key]["file"]}


def tool_embed(args: dict) -> dict:
    image = args.get("image_path")
    if not isinstance(image, str) or not image:
        raise ValueError({"error": "bad_request", "detail": "image_path required"})
    key = "efficientnet-s-embed"
    it = _interpreter(key)
    d = it.get_input_details()[0]
    arr, _ = _read_image(image, (d["shape"][2], d["shape"][1]))
    _set_input(it, arr)
    it.invoke()
    t, od = _out(it, 0)
    vec = _dequant(t, od).reshape(-1)
    return {"embedding": [float(v) for v in vec], "dim": int(vec.shape[0]), "space": "efficientnet-edgetpu-s",
            "model": MODELS[key]["file"]}


TOOLS = {"classify": tool_classify, "object_detect": tool_object_detect,
         "semantic_segment": tool_semantic_segment, "embed": tool_embed}


# ---------------------------------------------------------------- http

def _health() -> dict:
    missing = []
    for key, spec in MODELS.items():
        if not os.path.isfile(os.path.join(MODELS_DIR, spec["file"])):
            missing.append(spec["file"])
    temp_raw = _sysfs("temp")
    temp_c = None
    if temp_raw and temp_raw.lstrip("-").isdigit():
        temp_c = int(temp_raw) / 1000.0
    runtime = {}
    try:
        import ai_edge_litert  # type: ignore

        runtime["litert"] = getattr(ai_edge_litert, "__version__", "?")
    except Exception:  # noqa: BLE001 - test mode has no litert
        runtime["litert"] = None
    runtime["libedgetpu"] = DELEGATE_LIB
    return {"enabled": ENABLED, "device": DEVICE, "status": _sysfs("status"), "temp_c": temp_c,
            "loaded": sorted(_interp), "models_missing": missing, "runtime": runtime,
            "uptime_sec": int(time.time() - _started), "idle_sec": IDLE_SEC, "tools": sorted(TOOLS)}


class Handler(BaseHTTPRequestHandler):
    server_version = "coral-sidecar/1"

    def log_message(self, fmt, *args):  # quiet by default; CORAL_LOG=1 to see requests
        if os.environ.get("CORAL_LOG") == "1":
            super().log_message(fmt, *args)

    def _send(self, code: int, obj: dict):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        global _last_call
        _last_call = time.monotonic()
        if urlparse(self.path).path == "/health":
            return self._send(200, _health())
        return self._send(404, {"error": "not_found"})

    def do_POST(self):
        global _last_call
        _last_call = time.monotonic()
        path = urlparse(self.path).path
        if not path.startswith("/v1/"):
            return self._send(404, {"error": "not_found"})
        tool = path[len("/v1/"):]
        fn = TOOLS.get(tool)
        if fn is None:
            return self._send(404, {"error": "unknown_tool", "tool": tool, "known": sorted(TOOLS)})
        try:
            n = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(n) if n else b"{}"
            args = json.loads(raw or b"{}")
            if not isinstance(args, dict):
                raise ValueError({"error": "bad_request", "detail": "body must be a JSON object"})
        except ValueError as e:
            obj = e.args[0] if e.args and isinstance(e.args[0], dict) else {"error": "bad_request", "detail": str(e)}
            return self._send(400, obj)
        except Exception as e:  # noqa: BLE001
            return self._send(400, {"error": "bad_request", "detail": str(e)})
        try:
            with _lock:
                result = fn(args)
            return self._send(200, result)
        except ValueError as e:
            obj = e.args[0] if e.args and isinstance(e.args[0], dict) else {"error": "bad_request", "detail": str(e)}
            code = 400 if obj.get("error") == "bad_request" else 200
            return self._send(code, obj)
        except Exception as e:  # noqa: BLE001 - the 500 guard: never a stack trace on the wire
            return self._send(500, {"error": "internal", "detail": f"{type(e).__name__}: {e}"})


def _idle_watchdog(server: ThreadingHTTPServer):
    while True:
        time.sleep(5)
        if IDLE_SEC > 0 and time.monotonic() - _last_call > IDLE_SEC:
            print(f"coral sidecar: idle {IDLE_SEC}s, exiting", file=sys.stderr)
            threading.Thread(target=server.shutdown, daemon=True).start()
            return


def main():
    srv = ThreadingHTTPServer((BIND, PORT), Handler)
    threading.Thread(target=_idle_watchdog, args=(srv,), daemon=True).start()
    print(f"coral sidecar: listening on http://{BIND}:{PORT} enabled={ENABLED} models_dir={MODELS_DIR} idle={IDLE_SEC}s",
          file=sys.stderr)
    try:
        srv.serve_forever()
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
