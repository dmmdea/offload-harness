#!/usr/bin/env python3
"""Rockchip RKNPU sidecar for the offload harness (ADR 0024 accelerator lane; RK3588 NPU, three cores).

Wire contract — the Coral sidecar's, verbatim, so the harness's accelclient speaks to both:

    GET  /health                -> 200 {enabled, device, runtime, temp_c, npu_load, loaded, models_missing, tools, ...}
    POST /v1/<tool>  (JSON)     -> 200 result dict          | 200 {"error": ...} structured failure
                                   404 {"error":"unknown_tool"} | 400 {"error":"bad_request", ...}
                                   500 {"error":"internal", ...}
    anything else               -> 404 {"error":"not_found"}

Tools: classify · object_detect · embed. Arguments and results have the shapes of the Coral tools of
the same name: classify {image_path, domain?, top_k?}; object_detect {image_path, score_threshold?};
embed {image_path}.

Runtime facts this file encodes (measured on an Orange Pi 5, RK3588S, under the mainline 7.0 kernel with the
rknpu 0.9.8 out-of-tree driver; the vendor 6.1 kernel's built-in driver runs the same runtime library, and
every sysfs/debugfs read below degrades to null where a node is absent):
  * RKNN-Toolkit-Lite2 2.3.2's RKNNLite over librknnrt.so 2.3.2, models compiled for rk3588, core mask
    NPU_CORE_0_1_2 (all three cores). RKNNLite reports failure by return value, never by raising:
    load_rknn / init_runtime return non-zero and inference returns None, so every call is checked.
  * Inputs are ALWAYS 4-D NHWC uint8 (a 3-D array is refused by the runtime); normalisation is compiled
    into the model. Outputs arrive as float32 whatever the model's quantisation.
  * ONE model resident, released before the next one loads and again at the idle exit, so the footprint
    is one model and the box's own workload keeps its memory (process RSS 113 MB with yolov8n resident,
    259 MB with CLIP and 471 MB at its peak while CLIP loads; a model switch costs about 0.3 s for resnet18
    and yolov8n and 1.2-1.8 s for CLIP). ONE lock, one in-flight NPU call. load_rknn also keeps the whole
    file in Python memory; it is dropped once init_runtime has copied it into the context (CLIP: 179 MB).
  * The wheel does not honour LD_LIBRARY_PATH for its own runtime: RKNNRuntime._get_rknn_api_lib_path
    probes os.path.exists on exactly /usr/lib/librknn_runtime.so, /usr/lib64/librknn_runtime.so and
    /usr/lib/librknnrt.so, then calls CDLL on the path it found, and otherwise fails init_runtime with
    "Can not find dynamic library" (measured: LD_LIBRARY_PATH set, or the module's LIBRKNNRT_PATH constant
    changed, makes no difference). RKNPU_RUNTIME_LIB (the launcher sets it to <home>/lib/librknnrt.so) is
    honoured by _runtime_lib_shim: around init_runtime only, that one path is reported present and the
    wheel's CDLL is redirected to the private copy, so nothing has to be installed under /usr/lib.
  * The sidecar READS sysfs (the npu-thermal zone, /sys/module/rknpu/version) and debugfs
    (/sys/kernel/debug/rknpu/load: readable by any user on the reference board, usually root-only elsewhere,
    and reported as null when unreadable); it never writes either.

Refusals: binds loopback only (a non-loopback RKNPU_BIND is refused at startup); serves only files
listed in models.json whose sha256 matches — model files and their label files alike (an unlisted or
mismatched file is a structured error, never a load); exits itself after RKNPU_IDLE_SEC without a tool
call. /health never counts as a call, so a poller cannot keep a model resident.

RKNPU_ENABLED=0 runs the whole HTTP contract with the NPU path stubbed (for test_server.py on a box
without the device); every inference then answers {"error":"npu_disabled"}.
"""
from __future__ import annotations

import contextlib
import hashlib
import importlib.metadata
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
BIND = os.environ.get("RKNPU_BIND", "127.0.0.1")
PORT = int(os.environ.get("RKNPU_PORT", "18815"))
IDLE_SEC = int(os.environ.get("RKNPU_IDLE_SEC", "300"))
HOME = os.environ.get("RKNPU_HOME", HERE)
MODELS_DIR = os.environ.get("RKNPU_MODELS_DIR", os.path.join(HOME, "models"))
MANIFEST = os.environ.get("RKNPU_MANIFEST", os.path.join(HERE, "models.json"))
ENABLED = os.environ.get("RKNPU_ENABLED", "1") != "0"
RUNTIME_LIB = os.environ.get("RKNPU_RUNTIME_LIB", "")
SYSFS = os.environ.get("RKNPU_SYSFS", "/sys")  # a test hook: a fake tree stands in for the board's /sys
DEVICE = "rknpu"
# The three paths RKNNRuntime probes for its library, in probe order. The last is the one that exists on
# a stock install; the shim reports it present and redirects a CDLL of any of the three.
_VENDOR_LIBS = ("/usr/lib/librknn_runtime.so", "/usr/lib64/librknn_runtime.so", "/usr/lib/librknnrt.so")

LOOPBACK = {"127.0.0.1", "::1", "localhost"}
if BIND not in LOOPBACK:
    print(f"rknpu sidecar: refusing to bind non-loopback address {BIND!r}", file=sys.stderr)
    sys.exit(2)

# ---------------------------------------------------------------- manifest

with open(MANIFEST, encoding="utf-8") as fh:
    _MAN = json.load(fh)
MODELS: dict[str, dict] = _MAN["models"]  # key -> {file, sha256, task, input, labels?, convert?}
LABELS: dict[str, dict] = _MAN.get("labels", {})  # file -> {sha256, url}


class Refusal(Exception):
    """A structured failure: the dict is the response body. 400 for bad_request, 200 for the rest."""

    def __init__(self, body: dict):
        super().__init__(body)
        self.body = body


_lock = threading.Lock()  # one in-flight NPU call, one model switch at a time
_resident: tuple[str, object] | None = None  # (manifest key, RKNNLite) — one model at a time
_labels: dict[str, list[str]] = {}
_last_call = time.monotonic()
_started = time.time()
_runtime_cache: dict | None = None


def _touch() -> None:
    global _last_call
    _last_call = time.monotonic()


def _read(path: str) -> str | None:
    try:
        with open(path, encoding="utf-8") as fh:
            return fh.read().strip()
    except OSError:
        return None


def _sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _hint(key: str) -> str:
    if "convert" in MODELS.get(key, {}):
        return "no public download: convert it on an x86_64 host (fetch-models.sh --convert), then copy the file here"
    return "run fetch-models.sh"


def _artifact_path(key: str, name: str, sha256: str) -> str:
    """Resolve a manifest artifact to a verified file; raise Refusal with a structured reason."""
    path = os.path.join(MODELS_DIR, name)
    if not os.path.isfile(path):
        raise Refusal({"error": "model_missing", "model": key, "file": name, "hint": _hint(key)})
    digest = _sha256(path)
    if digest != sha256:
        raise Refusal({"error": "model_sha_mismatch", "model": key, "file": name, "expected": sha256, "got": digest})
    return path


def _model_path(key: str) -> str:
    spec = MODELS.get(key)
    if spec is None:
        raise Refusal({"error": "unknown_model", "model": key, "known": sorted(MODELS)})
    return _artifact_path(key, spec["file"], spec["sha256"])


_WNID = re.compile(r"^n\d{8}\s+")


def _load_labels(key: str) -> list[str]:
    if key in _labels:
        return _labels[key]
    out: list[str] = []
    name = MODELS[key].get("labels")
    if name:
        with open(_artifact_path(key, name, LABELS[name]["sha256"]), encoding="utf-8") as fh:
            for line in fh:
                line = line.rstrip("\r\n")
                if line:
                    # ImageNet synset lines ("n01440764 tench, Tinca tinca") lose the wnid; bare labels stay.
                    out.append(_WNID.sub("", line))
    _labels[key] = out
    return out


# ---------------------------------------------------------------- NPU

@contextlib.contextmanager
def _runtime_lib_shim():
    """Point RKNNLite at RKNPU_RUNTIME_LIB for the duration of init_runtime (see the module docstring).

    Scoped because os.path.exists is process-global: the window is one init_runtime call under the
    NPU lock, and the predicate answers for one exact path, so no other caller can see a difference.
    """
    lib = RUNTIME_LIB
    if not lib or not os.path.isfile(lib):
        yield
        return
    import rknnlite.api.rknn_runtime as rr

    real_exists, real_cdll = os.path.exists, rr.CDLL

    class _CDLL(real_cdll):
        def __init__(self, name, *args, **kwargs):
            super().__init__(lib if name in _VENDOR_LIBS else name, *args, **kwargs)

    os.path.exists = lambda p: True if p == _VENDOR_LIBS[-1] else real_exists(p)
    rr.CDLL = _CDLL
    try:
        yield
    finally:
        os.path.exists = real_exists
        rr.CDLL = real_cdll


def _release() -> None:
    """Give the NPU context back (caller holds the lock)."""
    global _resident
    if _resident is not None:
        try:
            _resident[1].release()
        except Exception as e:  # noqa: BLE001 - a failing release must not block the next load or the exit
            print(f"rknpu sidecar: release of {_resident[0]} failed: {e}", file=sys.stderr)
        _resident = None


def _model(key: str):
    """Resident RKNNLite for key, built on first use (caller holds the lock)."""
    global _resident
    if _resident is not None and _resident[0] == key:
        return _resident[1]
    path = _model_path(key)  # verified before the resident model is touched
    try:
        from rknnlite.api import RKNNLite
    except ImportError as e:
        raise Refusal({"error": "runtime_missing", "detail": f"rknn-toolkit-lite2 is not importable: {e}"}) from None
    _release()
    rt = RKNNLite()
    if rt.load_rknn(path) != 0:
        raise Refusal({"error": "model_load_failed", "model": key, "file": MODELS[key]["file"]})
    with _runtime_lib_shim():
        ret = rt.init_runtime(core_mask=RKNNLite.NPU_CORE_0_1_2)
    if ret != 0:
        rt.release()
        raise Refusal({"error": "npu_unavailable", "model": key,
                       "detail": "init_runtime failed: the NPU needs root or the render group, and librknnrt.so "
                                 "(see RKNPU_RUNTIME_LIB); the sidecar log carries the runtime's own message"})
    rt.rknn_data = None  # load_rknn keeps the whole file in Python memory; the context has its own copy now
    _resident = (key, rt)
    return rt


def _infer(key: str, arr):
    outs = _model(key).inference(inputs=[arr])
    if outs is None:
        raise Refusal({"error": "inference_failed", "model": key,
                       "detail": "RKNNLite.inference returned None; the sidecar log carries the runtime's message"})
    return outs


# ---------------------------------------------------------------- images

def _require_npu() -> None:
    """Called by every tool after its arguments validate and before it touches an image or numpy, so the
    RKNPU_ENABLED=0 stub answers the whole contract on a box with neither the device nor the libraries."""
    if not ENABLED:
        raise Refusal({"error": "npu_disabled"})


def _image_arg(args: dict) -> str:
    image = args.get("image_path")
    if not isinstance(image, str) or not image:
        raise Refusal({"error": "bad_request", "detail": "image_path required"})
    return image


def _int_arg(args: dict, name: str, default: int, lo: int, hi: int) -> int:
    v = args.get(name, default)
    if isinstance(v, bool) or not isinstance(v, int) or not lo <= v <= hi:
        raise Refusal({"error": "bad_request", "detail": f"{name} must be an integer in [{lo}, {hi}]"})
    return v


def _float_arg(args: dict, name: str, default: float, lo: float, hi: float) -> float:
    v = args.get(name, default)
    if isinstance(v, bool) or not isinstance(v, (int, float)) or not lo <= v <= hi:
        raise Refusal({"error": "bad_request", "detail": f"{name} must be a number in [{lo}, {hi}]"})
    return float(v)


def _open_image(path: str):
    """Open path lazily (header only): a missing or unreadable file is refused before a model is touched."""
    from PIL import Image

    if not os.path.isfile(path):
        raise Refusal({"error": "image_missing", "image_path": path})
    try:
        return Image.open(path)
    except (OSError, Image.DecompressionBombError) as e:
        raise Refusal({"error": "image_unreadable", "image_path": path, "detail": str(e)}) from None


def _decode(img, path: str, want: tuple[int, int]):
    """Decode to RGB. A JPEG decodes at 1/2, 1/4 or 1/8 scale when want (the size the caller resizes to)
    allows it, which keeps the CPU cost of a 12-megapixel photo on an A55 core small."""
    from PIL import Image

    try:
        img.draft("RGB", want)
        return img.convert("RGB")
    except (OSError, Image.DecompressionBombError) as e:
        raise Refusal({"error": "image_unreadable", "image_path": path, "detail": str(e)}) from None


def _nhwc(img):
    import numpy as np

    return np.asarray(img, dtype=np.uint8)[None]


# ---------------------------------------------------------------- tools

CLASSIFY_DOMAINS = {"imagenet": "resnet18"}
DETECT_KEY = "yolov8n"
EMBED_KEY = "clip-vit-b32-image"
NMS_IOU = 0.45          # the rknn_model_zoo yolov8 recipe's NMS_THRESH
DETECT_SCORE = 0.25     # ... and its OBJ_THRESH, the default score_threshold
MAX_CANDIDATES = 2000   # boxes past the threshold that reach NMS (a threshold near 0 must not stall an A55 core)
MAX_DETECTIONS = 100


def tool_classify(args: dict) -> dict:
    image = _image_arg(args)
    domain = args.get("domain", "imagenet")
    key = CLASSIFY_DOMAINS.get(domain) if isinstance(domain, str) else None
    if key is None:
        raise Refusal({"error": "bad_request", "detail": f"domain must be one of {sorted(CLASSIFY_DOMAINS)}"})
    top_k = _int_arg(args, "top_k", 5, 1, 100)
    _require_npu()
    from PIL import Image
    import numpy as np

    spec = MODELS[key]
    size = spec["input"]
    with _open_image(image) as img:  # closed on every path: a refusal below must not leave the file open
        labels = _load_labels(key)  # every artifact is verified before the NPU is touched
        arr = _nhwc(_decode(img, image, (size, size)).resize((size, size), Image.BILINEAR))
    outs = _infer(key, arr)
    logits = outs[0].reshape(-1).astype(np.float32)  # the fc layer: raw logits
    e = np.exp(logits - logits.max())
    scores = e / e.sum()
    order = scores.argsort()[::-1][:top_k]
    results = [{"label": labels[i] if i < len(labels) else str(i), "score": float(scores[i])} for i in order]
    return {"results": results, "best": results[0], "model": spec["file"], "domain": domain}


def _dfl(box):
    """Distribution Focal Loss decode: (n, 64) logits -> (n, 4) l,t,r,b distances in grid cells."""
    import numpy as np

    x = box.reshape(box.shape[0], 4, 16)
    e = np.exp(x - x.max(axis=2, keepdims=True))
    return ((e / e.sum(axis=2, keepdims=True)) * np.arange(16, dtype=np.float32)).sum(axis=2)


def _nms(xyxy, scores, iou_thr: float) -> list[int]:
    import numpy as np

    areas = (xyxy[:, 2] - xyxy[:, 0]) * (xyxy[:, 3] - xyxy[:, 1])
    order = scores.argsort()[::-1]
    keep: list[int] = []
    while order.size:
        i = order[0]
        keep.append(int(i))
        rest = order[1:]
        w = np.maximum(0.0, np.minimum(xyxy[i, 2], xyxy[rest, 2]) - np.maximum(xyxy[i, 0], xyxy[rest, 0]))
        h = np.maximum(0.0, np.minimum(xyxy[i, 3], xyxy[rest, 3]) - np.maximum(xyxy[i, 1], xyxy[rest, 1]))
        inter = w * h
        order = rest[inter / (areas[i] + areas[rest] - inter + 1e-9) <= iou_thr]
    return keep


def _decode_yolov8(outs, size: int, thr: float):
    """rknn_model_zoo yolov8 post-processing: three (box[1,64,h,w], class[1,80,h,w], score-sum[1,1,h,w])
    branches -> (xyxy in letterboxed pixels, class ids, scores), per-class NMS applied. The score-sum
    branch is the C demo's pre-filter and is ignored, as the recipe's Python demo does."""
    import numpy as np

    shapes = [list(map(int, o.shape)) for o in outs]
    per = len(outs) // 3
    if len(outs) < 3 or len(outs) % 3:
        raise Refusal({"error": "internal", "detail": "unexpected detector output layout", "shapes": shapes})
    box_l, cls_l, cell_l = [], [], []
    for b in range(3):
        box, cls = outs[per * b], outs[per * b + 1]
        if box.ndim != 4 or cls.ndim != 4 or box.shape[1] != 64 or box.shape[2:] != cls.shape[2:]:
            raise Refusal({"error": "internal", "detail": "unexpected detector output layout", "shapes": shapes})
        gh, gw = box.shape[2:]
        gy, gx = np.divmod(np.arange(gh * gw), gw)
        box_l.append(box[0].transpose(1, 2, 0).reshape(-1, 64))
        cls_l.append(cls[0].transpose(1, 2, 0).reshape(-1, cls.shape[1]))
        cell_l.append(np.stack([gx, gy, np.full(gh * gw, size // gh)], axis=1))
    box, cls, cell = np.concatenate(box_l), np.concatenate(cls_l), np.concatenate(cell_l)
    ids = cls.argmax(axis=1)
    scores = cls[np.arange(len(cls)), ids]
    keep = np.flatnonzero(scores >= thr)
    if keep.size > MAX_CANDIDATES:
        keep = keep[np.argsort(-scores[keep])[:MAX_CANDIDATES]]
    if not keep.size:
        return np.zeros((0, 4), np.float32), ids[:0], scores[:0]
    d = _dfl(box[keep].astype(np.float32))
    gx, gy, stride = cell[keep].T.astype(np.float32)
    xyxy = np.stack([(gx + 0.5 - d[:, 0]) * stride, (gy + 0.5 - d[:, 1]) * stride,
                     (gx + 0.5 + d[:, 2]) * stride, (gy + 0.5 + d[:, 3]) * stride], axis=1)
    ids, scores = ids[keep], scores[keep]
    # Per-class NMS in one pass: boxes of different classes are pushed apart so they never overlap.
    picked = _nms(xyxy + (ids * 2 * size)[:, None], scores, NMS_IOU)
    return xyxy[picked], ids[picked], scores[picked]


def tool_object_detect(args: dict) -> dict:
    image = _image_arg(args)
    thr = _float_arg(args, "score_threshold", DETECT_SCORE, 0.0, 1.0)
    _require_npu()
    from PIL import Image

    spec = MODELS[DETECT_KEY]
    size = spec["input"]
    with _open_image(image) as img:
        labels = _load_labels(DETECT_KEY)  # every artifact is verified before the NPU is touched
        w, h = img.size
        # Letterbox as the recipe does: fit inside size x size keeping the aspect, centred on black.
        r = min(size / w, size / h)
        nw, nh = max(1, round(w * r)), max(1, round(h * r))
        left, top = (size - nw) // 2, (size - nh) // 2
        canvas = Image.new("RGB", (size, size), (0, 0, 0))
        canvas.paste(_decode(img, image, (nw, nh)).resize((nw, nh), Image.BILINEAR), (left, top))
    outs = _infer(DETECT_KEY, _nhwc(canvas))
    xyxy, ids, scores = _decode_yolov8(outs, size, thr)
    objects = []
    for (x1, y1, x2, y2), cid, s in zip(xyxy, ids, scores):
        x1, x2 = min(max((x1 - left) / r, 0.0), w), min(max((x2 - left) / r, 0.0), w)
        y1, y2 = min(max((y1 - top) / r, 0.0), h), min(max((y2 - top) / r, 0.0), h)
        cid = int(cid)
        objects.append({"label": labels[cid] if cid < len(labels) else str(cid), "class_id": cid,
                        "x": float(x1), "y": float(y1), "w": float(x2 - x1), "h": float(y2 - y1), "score": float(s)})
    objects.sort(key=lambda o: -o["score"])
    objects = objects[:MAX_DETECTIONS]
    return {"objects": objects, "count": len(objects), "model": spec["file"], "image_width": w, "image_height": h}


def tool_embed(args: dict) -> dict:
    image = _image_arg(args)
    _require_npu()
    from PIL import Image

    spec = MODELS[EMBED_KEY]
    size = spec["input"]
    with _open_image(image) as img:
        w, h = img.size
        # CLIP's own preprocessing: shorter side to size (bicubic), then a centred size x size crop.
        s = size / min(w, h)
        nw, nh = max(size, round(w * s)), max(size, round(h * s))
        scaled = _decode(img, image, (nw, nh)).resize((nw, nh), Image.BICUBIC)
    left, top = (nw - size) // 2, (nh - size) // 2
    outs = _infer(EMBED_KEY, _nhwc(scaled.crop((left, top, left + size, top + size))))
    vec = outs[0].reshape(-1)
    return {"embedding": [float(v) for v in vec], "dim": int(vec.shape[0]), "space": spec["space"], "model": spec["file"]}


TOOLS = {"classify": tool_classify, "object_detect": tool_object_detect, "embed": tool_embed}


# ---------------------------------------------------------------- http

def _npu_temp() -> float | None:
    base = os.path.join(SYSFS, "class", "thermal")
    try:
        zones = sorted(os.listdir(base))
    except OSError:
        return None
    for zone in zones:
        if zone.startswith("thermal_zone") and _read(os.path.join(base, zone, "type")) == "npu-thermal":
            raw = _read(os.path.join(base, zone, "temp"))
            if raw and raw.lstrip("-").isdigit():
                return int(raw) / 1000.0
    return None


def _npu_load() -> list[int] | None:
    """Per-core load in percent from debugfs ("NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,"); root only."""
    raw = _read(os.path.join(SYSFS, "kernel", "debug", "rknpu", "load"))
    cores = re.findall(r"Core(\d+):\s*(\d+)%", raw or "")
    return [int(pct) for _, pct in sorted(cores, key=lambda c: int(c[0]))] or None


def _runtime_info() -> dict:
    """Versions, computed once: the toolkit from its package metadata, the runtime from the version string
    embedded in the library file (no NPU access), the driver from /sys/module (world-readable)."""
    global _runtime_cache
    if _runtime_cache is None:
        try:
            toolkit = importlib.metadata.version("rknn-toolkit-lite2")
        except importlib.metadata.PackageNotFoundError:
            toolkit = None
        lib = next((p for p in (RUNTIME_LIB, *_VENDOR_LIBS) if p and os.path.isfile(p)), None)
        version = None
        if lib:
            with open(lib, "rb") as fh:
                m = re.search(rb"librknnrt version: ([^\x00\r\n]+)", fh.read())
            version = m.group(1).decode("ascii", "replace") if m else None
        driver = _read(os.path.join(SYSFS, "module", "rknpu", "version"))
        _runtime_cache = {"rknn_toolkit_lite2": toolkit, "librknnrt": version, "librknnrt_path": lib, "rknpu_driver": driver}
    return _runtime_cache


def _health() -> dict:
    missing = set()
    for spec in MODELS.values():
        for name in (spec["file"], spec.get("labels")):
            if name and not os.path.isfile(os.path.join(MODELS_DIR, name)):
                missing.add(name)
    resident = _resident
    return {"enabled": ENABLED, "device": DEVICE, "runtime": _runtime_info(), "core_mask": "0_1_2",
            "temp_c": _npu_temp(), "npu_load": _npu_load(),
            "cpu_affinity": sorted(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
            "loaded": [resident[0]] if resident else [], "models_missing": sorted(missing), "tools": sorted(TOOLS),
            "uptime_sec": int(time.time() - _started), "idle_sec": IDLE_SEC}


class Handler(BaseHTTPRequestHandler):
    server_version = "rknpu-sidecar/1"

    def log_message(self, fmt, *args):  # quiet by default; RKNPU_LOG=1 to see requests
        if os.environ.get("RKNPU_LOG") == "1":
            super().log_message(fmt, *args)

    def _send(self, code: int, obj: dict):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # Deliberately no _touch(): /health is how the harness (and offload_status) asks whether the
        # sidecar is up, and that must never keep a model resident.
        if urlparse(self.path).path == "/health":
            return self._send(200, _health())
        return self._send(404, {"error": "not_found"})

    def do_POST(self):
        path = urlparse(self.path).path
        if not path.startswith("/v1/"):
            return self._send(404, {"error": "not_found"})
        tool = path[len("/v1/"):]
        fn = TOOLS.get(tool)
        if fn is None:
            return self._send(404, {"error": "unknown_tool", "tool": tool, "known": sorted(TOOLS)})
        _touch()
        try:
            n = int(self.headers.get("Content-Length") or 0)
            if n < 0:
                raise ValueError("negative Content-Length")
            args = json.loads((self.rfile.read(n) if n else b"") or b"{}")
        except ValueError as e:  # a bad Content-Length or JSON that does not parse
            return self._send(400, {"error": "bad_request", "detail": str(e)})
        if not isinstance(args, dict):
            return self._send(400, {"error": "bad_request", "detail": "body must be a JSON object"})
        try:
            with _lock:
                try:
                    result = fn(args)
                finally:
                    # Idle counts from the END of the call, and the watchdog only acts while holding this lock, so
                    # touching before the lock is released leaves no window (the response write below is outside
                    # it) in which a call longer than --idle-sec looks idle and ends the process mid-response.
                    _touch()
            return self._send(200, result)
        except Refusal as e:
            return self._send(400 if e.body.get("error") == "bad_request" else 200, e.body)
        except Exception as e:  # noqa: BLE001 - the 500 guard: never a stack trace on the wire
            return self._send(500, {"error": "internal", "detail": f"{type(e).__name__}: {e}"})


def _idle_watchdog(server: ThreadingHTTPServer):
    tick = min(5.0, IDLE_SEC / 4)
    while True:
        time.sleep(tick)
        if time.monotonic() - _last_call > IDLE_SEC and _lock.acquire(blocking=False):
            try:
                _release()  # the NPU context goes back before the process does
            finally:
                _lock.release()
            print(f"rknpu sidecar: idle {IDLE_SEC}s, exiting", file=sys.stderr)
            threading.Thread(target=server.shutdown, daemon=True).start()
            return


def main():
    srv = ThreadingHTTPServer((BIND, PORT), Handler)
    if IDLE_SEC > 0:
        threading.Thread(target=_idle_watchdog, args=(srv,), daemon=True).start()
    print(f"rknpu sidecar: listening on http://{BIND}:{PORT} enabled={ENABLED} models_dir={MODELS_DIR} idle={IDLE_SEC}s",
          file=sys.stderr)
    try:
        srv.serve_forever()
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
