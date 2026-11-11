#!/usr/bin/env python3
"""Axera AX650-class NPU sidecar for the offload harness (ADR 0024 accelerator lane; an M.2 card on the
host's PCIe, reached through the AXCL host runtime).

Wire contract: the Coral sidecar's, so the harness's accelclient speaks to every device the same way.

    GET  /health                -> 200 {enabled, device, runtime, loaded, cmm_resident_mib, models_missing,
                                        models_unverified, tools, perf, telemetry, uptime_sec, idle_sec}
    POST /v1/<tool>  (JSON)     -> 200 result dict          | 200 {"error": ...} structured failure
                                   404 {"error":"unknown_tool"} | 400 {"error":"bad_request", ...}
                                   500 {"error":"internal", ...}
    anything else               -> 404 {"error":"not_found"}

Tools and the models behind them (every model compiled for the AX650 NPU by its publisher; models.json):
    classify          {image_path, top_k?}                    SigLIP2 B/16-224 zero-shot over the 1000 ImageNet names
    object_detect     {image_path, score_threshold?}          RF-DETR small, 80 COCO classes
    semantic_segment  {image_path, out_path?}                 DeepLabV3+ MobileNetV2, 21 Pascal VOC classes
    segment           {image_path, everything?, score_threshold?, out_path?}
                                                              RF-DETR boxes refined by MobileSAM; everything = point grid
    embed             {image_path}                            SigLIP2 image tower, 768-d
    text_embed        {text}                                  SigLIP2 text tower, the same 768-d space
    zero_shot         {image_path, labels, template?}         both SigLIP2 towers
    depth             {image_path, out_path?}                 Depth Anything 3 small
    pose              {image_path, score_threshold?}          RF-DETR person boxes, then RTMPose-M (17 COCO keypoints)
    ocr               {image_path}                            PP-OCRv5 mobile: detection, orientation, recognition

Runtime facts this file encodes (measured on the card, 2026-11-10, AXCL host runtime over PCIe Gen2 x2):
  * pyaxengine's InferenceSession over libaxcl_rt; inputs must be C-contiguous numpy arrays of exactly the
    model's dtype and shape (uint8 NHWC for most vision models: normalisation is compiled into them).
  * Every model loads into the card's CMM pool (2944 MiB), never host RAM or VRAM. Measured CMM per model
    (MiB): RF-DETR-S 32, SigLIP2 image 98, text 466, DeepLabV3+ 51, DA3-small 41, RTMPose-M 17, MobileSAM
    encoder 33 + decoder 12, PP-OCRv5 det 33 + rec 9 + cls 1. Sessions stay resident (LRU under
    AXERA_CMM_BUDGET_MIB, default 1536, so an NPU LLM seat on the same card keeps its share) and a load
    that the card refuses evicts every other session and retries once.
  * One lock, one in-flight NPU call: the sessions are not documented as thread-safe and the card serialises
    anyway. Latency per call, NPU plus PCIe, ms: RF-DETR 40, SigLIP2 image 13, text 5, DeepLabV3+ 46,
    DA3 27, RTMPose 4, MobileSAM encoder 64 + decoder 21, PP-OCRv5 det 44 + rec 7 per line.
  * RF-DETR ships split: the NPU part returns logits, box features and reference boxes, and the publisher's
    box head (a three-layer MLP, 0.5 MB ONNX) turns them into boxes on the host through onnxruntime, as the
    publisher's own inference script does.
  * classify needs only the image tower at call time: the 1000 ImageNet text embeddings are computed once
    with the text tower and cached under <home>/cache, keyed by the text model's digest and the template.
  * Telemetry is read by axcl_telemetry.py in a CHILD process (never in this one, which holds live sessions),
    cached for 5 s and refreshed off the request path; /health returns the newest sample.
  * At start the card CPU is set to its top frequency through axcl_telemetry.py --apply-perf (the NPU and
    NN-bus clocks are already at their maxima and are reported, not written). The setting is volatile and
    idempotent, so every spawn applies it again.

Refusals: binds loopback only (a non-loopback AXERA_BIND is refused at startup); loads only files the
manifest names; a file whose bytes differ from the digest fetch_models.py recorded in models.installed.json
is a structured error, never a load (no record: served, and listed under models_unverified in /health);
exits itself after AXERA_IDLE_SEC (the launcher's --idle-sec) without a tool call, clamped to 1..300,
because a model that idles longer than five minutes is a violation. /health never counts as a call, so a
poller cannot keep a model resident. The process asks the OOM killer to take it before the host's own
stack (oom_score_adj 500).

AXERA_ENABLED=0 runs the HTTP contract with the NPU stubbed; every inference then answers
{"error":"npu_disabled"}.
"""
from __future__ import annotations

import collections
import hashlib
import importlib.metadata
import json
import math
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
BIND = os.environ.get("AXERA_BIND", "127.0.0.1")
PORT = int(os.environ.get("AXERA_PORT", "18816"))
MAX_IDLE_SEC = 300


def _clamp_idle(sec: int) -> int:
    """Every model idles out within five minutes: 0, a negative value and more than 300 mean 300."""
    if 1 <= sec <= MAX_IDLE_SEC:
        return sec
    print(f"axera sidecar: idle seconds {sec} is outside 1..{MAX_IDLE_SEC}, using {MAX_IDLE_SEC}", file=sys.stderr)
    return MAX_IDLE_SEC


IDLE_SEC = _clamp_idle(int(os.environ.get("AXERA_IDLE_SEC", "300")))
HOME = os.environ.get("AXERA_HOME", os.path.dirname(HERE))
MODELS_DIR = os.environ.get("AXERA_MODELS_DIR", os.path.join(HOME, "models"))
MANIFEST = os.environ.get("AXERA_MANIFEST", os.path.join(HERE, "models.json"))
INSTALLED = os.environ.get("AXERA_INSTALLED", os.path.join(MODELS_DIR, "models.installed.json"))
CACHE_DIR = os.environ.get("AXERA_CACHE_DIR", os.path.join(HOME, "cache"))
ENABLED = os.environ.get("AXERA_ENABLED", "1") != "0"
CMM_BUDGET_MIB = int(os.environ.get("AXERA_CMM_BUDGET_MIB", "1536"))
TELEMETRY = os.path.join(HERE, "axcl_telemetry.py")
TELEMETRY_TTL = 5.0
DEVICE = "axera-ax650"
OOM_SCORE_ADJ = "/proc/self/oom_score_adj"
OOM_SCORE = "500"
MAX_PIXELS = 64_000_000

LOOPBACK = {"127.0.0.1", "::1", "localhost"}
if BIND not in LOOPBACK:
    print(f"axera sidecar: refusing to bind non-loopback address {BIND!r}", file=sys.stderr)
    sys.exit(2)

with open(MANIFEST, encoding="utf-8") as fh:
    _MAN = json.load(fh)

# The files each tool loads: (manifest key, path inside the repository). A file outside this table is never
# opened, and every entry must be one the manifest names (checked at startup).
FILES = {
    "rfdetr": ("rfdetr-s", "models/rf-detr-small.axmodel"),
    "rfdetr_head": ("rfdetr-s", "models/rfdetr_small_512_b2_post.onnx"),
    "siglip_image": ("siglip2-b16-224", "ax650/siglip2-base-patch16-224_vision.axmodel"),
    "siglip_text": ("siglip2-b16-224", "ax650/siglip2-base-patch16-224_text.axmodel"),
    "siglip_tokenizer": ("siglip2-b16-224", "tokenizer/tokenizer.json"),
    "deeplab": ("deeplabv3plus", "models-ax650/deeplabv3plus_mobilenet_u16.axmodel"),
    "da3": ("depth-anything-3-s", "models-ax650/da3-small.axmodel"),
    "rtmpose": ("rtmpose-m", "AX650/rtmpose_m_npu3.axmodel"),
    "sam_encoder": ("mobilesam", "ax_model/mobile_sam_encoder_650.axmodel"),
    "sam_decoder": ("mobilesam", "ax_model/mobile_sam_decoder_650.axmodel"),
    "ocr_det": ("ppocr-v5", "ax650/det_npu3.axmodel"),
    "ocr_cls": ("ppocr-v5", "ax650/cls_npu3.axmodel"),
    "ocr_rec": ("ppocr-v5", "ax650/rec_npu3.axmodel"),
    "ocr_dict": ("ppocr-v5", "ppocrv5_dict.txt"),
}
# Measured CMM per loaded model (MiB), the LRU budget's estimate; an unlisted model counts 64.
CMM_MIB = {"rfdetr": 32, "siglip_image": 98, "siglip_text": 466, "deeplab": 51, "da3": 41, "rtmpose": 17,
           "sam_encoder": 33, "sam_decoder": 12, "ocr_det": 33, "ocr_cls": 1, "ocr_rec": 9}
TOOL_FILES = {
    "classify": ["siglip_image", "siglip_text", "siglip_tokenizer"],
    "object_detect": ["rfdetr", "rfdetr_head"],
    "semantic_segment": ["deeplab"],
    "segment": ["rfdetr", "rfdetr_head", "sam_encoder", "sam_decoder"],
    "embed": ["siglip_image"],
    "text_embed": ["siglip_text", "siglip_tokenizer"],
    "zero_shot": ["siglip_image", "siglip_text", "siglip_tokenizer"],
    "depth": ["da3"],
    "pose": ["rfdetr", "rfdetr_head", "rtmpose"],
    "ocr": ["ocr_det", "ocr_cls", "ocr_rec", "ocr_dict"],
}
for _k, (_mk, _p) in FILES.items():
    if _mk not in _MAN["models"] or _p not in _MAN["models"][_mk]["files"]:
        print(f"axera sidecar: {_k} -> {_mk}/{_p} is not in the manifest", file=sys.stderr)
        sys.exit(2)

SIGLIP_SPACE = "siglip2-base-p16-224"
SIGLIP_SCALE = math.exp(4.7244534)  # the checkpoint's logit_scale, exponentiated
SIGLIP_BIAS = -16.771725            # the checkpoint's logit_bias
SIGLIP_LEN = 64
CLASSIFY_TEMPLATE = "a photo of a {}."

# COCO category ids as RF-DETR's 91 logits index them (0 and the N/A ids are never real classes).
COCO = ["__background__", "person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck",
        "boat", "traffic light", "fire hydrant", "N/A", "stop sign", "parking meter", "bench", "bird", "cat",
        "dog", "horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "N/A", "backpack", "umbrella",
        "N/A", "N/A", "handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball", "kite",
        "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket", "bottle", "N/A",
        "wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
        "broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "couch", "potted plant", "bed",
        "N/A", "dining table", "N/A", "N/A", "toilet", "N/A", "tv", "laptop", "mouse", "remote", "keyboard",
        "cell phone", "microwave", "oven", "toaster", "sink", "refrigerator", "N/A", "book", "clock", "vase",
        "scissors", "teddy bear", "hair drier", "toothbrush"]
VOC = ["background", "aeroplane", "bicycle", "bird", "boat", "bottle", "bus", "car", "cat", "chair", "cow",
       "diningtable", "dog", "horse", "motorbike", "person", "pottedplant", "sheep", "sofa", "train", "tvmonitor"]
KEYPOINTS = ["nose", "left_eye", "right_eye", "left_ear", "right_ear", "left_shoulder", "right_shoulder",
             "left_elbow", "right_elbow", "left_wrist", "right_wrist", "left_hip", "right_hip", "left_knee",
             "right_knee", "left_ankle", "right_ankle"]


class Refusal(Exception):
    """A structured failure: the dict is the response body. 400 for bad_request, 200 for the rest."""

    def __init__(self, body: dict):
        super().__init__(body)
        self.body = body


_lock = threading.Lock()  # one in-flight NPU call, one load or eviction at a time
_sessions: "collections.OrderedDict[str, object]" = collections.OrderedDict()  # FILES key -> session, LRU order
_verified: dict[str, tuple[int, int]] = {}  # rel path -> (size, mtime_ns) whose digest matched its record
_text_cache: "collections.OrderedDict[str, object]" = collections.OrderedDict()
_aux: dict[str, object] = {}  # tokenizer, box head, OCR dictionary, ImageNet text matrix
_last_call = time.monotonic()
_started = time.time()
_perf: dict = {"applied": False}
_tel_lock = threading.Lock()
_tel: dict = {"doc": None, "at": 0.0, "running": False}


def _touch() -> None:
    global _last_call
    _last_call = time.monotonic()


# ---------------------------------------------------------------- files and integrity

def _rel(key: str) -> str:
    mk, p = FILES[key]
    return f"{mk}/{p}"


def _path(key: str) -> str:
    return os.path.join(MODELS_DIR, *_rel(key).split("/"))


def _record() -> dict:
    try:
        with open(INSTALLED, encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return {}


def _sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _checked(key: str) -> str:
    """The file's path once its bytes match the digest fetch_models.py recorded (hashed once per process)."""
    path, rel = _path(key), _rel(key)
    if not os.path.isfile(path):
        raise Refusal({"error": "model_missing", "file": rel,
                       "hint": f"fetch it: fetch-models.sh --only {FILES[key][0]}"})
    st = os.stat(path)
    if _verified.get(rel) == (st.st_size, st.st_mtime_ns):
        return path
    want = (_record().get(rel) or {}).get("digest")
    if want:
        got = "sha256:" + _sha256(path)
        if got != want:
            raise Refusal({"error": "model_digest_mismatch", "file": rel, "recorded": want, "found": got,
                           "hint": f"refetch it: fetch-models.sh --only {FILES[key][0]} --verify"})
    _verified[rel] = (st.st_size, st.st_mtime_ns)
    return path


# ---------------------------------------------------------------- sessions

def _drop(sess) -> None:
    """Free one session's card memory now. pyaxengine keeps every session in a module-level list, so dropping
    our reference never runs its __del__ (measured: an evicted session kept its CMM until the process
    exited), and its unload does not bind the AXCL context, which is per thread, so it fails from any thread
    but the creator's ("thread hasn't binded any context"). This binds the session's own context, then
    unloads; the emptied object stays in that list and the library's exit hook skips it (unload is
    idempotent)."""
    inner = getattr(sess, "_sess", None)
    if inner is None or not hasattr(inner, "_unload"):
        return
    try:
        from axengine import _axclrt

        ctx = getattr(inner, "_thread_context", None)
        if ctx is not None:
            _axclrt.axclrt_lib.axclrtSetCurrentContext(ctx[0])
        inner._unload()
    except Exception as e:  # noqa: BLE001 - a failed unload is logged, never fatal; process exit frees the rest
        print(f"axera sidecar: unload failed: {type(e).__name__}: {e}", file=sys.stderr)


def _evict(key: str) -> None:
    s = _sessions.pop(key, None)
    if s is not None:
        _drop(s)


def _release() -> None:
    for key in list(_sessions):
        _evict(key)
    _text_cache.clear()
    _aux.pop("rfdetr_head", None)
    import gc
    gc.collect()


def _resident_mib() -> int:
    return sum(CMM_MIB.get(k, 64) for k in _sessions)


def _session(key: str):
    """The resident session for key, loading it (and evicting least-recently-used ones) as needed."""
    if not ENABLED:
        raise Refusal({"error": "npu_disabled"})
    s = _sessions.get(key)
    if s is not None:
        _sessions.move_to_end(key)
        return s
    path = _checked(key)
    need = CMM_MIB.get(key, 64)
    while _sessions and _resident_mib() + need > CMM_BUDGET_MIB:
        _evict(next(iter(_sessions)))  # least recently used first
    import axengine

    try:
        s = axengine.InferenceSession(path)
    except Exception as first:  # noqa: BLE001 - the card refused: free every other session, retry once
        if not _sessions:
            raise Refusal({"error": "model_load_failed", "file": _rel(key), "detail": str(first)[:300]}) from None
        _release()
        try:
            s = axengine.InferenceSession(path)
        except Exception as e:  # noqa: BLE001
            raise Refusal({"error": "model_load_failed", "file": _rel(key), "detail": str(e)[:300],
                           "hint": "the card's CMM pool may be held by another process; see /health telemetry"}) from None
    _sessions[key] = s
    return s


def _run(key: str, feed: dict) -> list:
    import numpy as np

    s = _session(key)
    return s.run(None, {k: np.ascontiguousarray(v) for k, v in feed.items()})


def _box_head():
    h = _aux.get("rfdetr_head")
    if h is None:
        import onnxruntime as ort

        opts = ort.SessionOptions()
        opts.intra_op_num_threads = 1
        h = ort.InferenceSession(_checked("rfdetr_head"), sess_options=opts, providers=["CPUExecutionProvider"])
        _aux["rfdetr_head"] = h
    return h


def _tokenizer():
    t = _aux.get("tokenizer")
    if t is None:
        from tokenizers import Tokenizer

        t = Tokenizer.from_file(_checked("siglip_tokenizer"))
        t.enable_truncation(SIGLIP_LEN)
        t.enable_padding(length=SIGLIP_LEN, pad_id=0, pad_token="<pad>")
        _aux["tokenizer"] = t
    return t


# ---------------------------------------------------------------- arguments and images

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


def _bool_arg(args: dict, name: str, default: bool) -> bool:
    v = args.get(name, default)
    if not isinstance(v, bool):
        raise Refusal({"error": "bad_request", "detail": f"{name} must be true or false"})
    return v


def _rgb(path: str):
    """The image as an RGB uint8 HxWx3 array, EXIF orientation applied."""
    import numpy as np
    from PIL import Image, ImageOps

    if not os.path.isfile(path):
        raise Refusal({"error": "image_missing", "image_path": path})
    Image.MAX_IMAGE_PIXELS = MAX_PIXELS
    try:
        with Image.open(path) as im:
            im = ImageOps.exif_transpose(im)
            return np.asarray(im.convert("RGB"), dtype=np.uint8)
    except (OSError, ValueError, Image.DecompressionBombError) as e:
        raise Refusal({"error": "image_unreadable", "image_path": path, "detail": str(e)}) from None


def _resize(arr, w: int, h: int, nearest: bool = False):
    import cv2

    return cv2.resize(arr, (w, h), interpolation=cv2.INTER_NEAREST if nearest else cv2.INTER_LINEAR)


def _out_path(args: dict, image: str, suffix: str) -> str:
    out = args.get("out_path")
    if out is not None and (not isinstance(out, str) or not out):
        raise Refusal({"error": "bad_request", "detail": "out_path must be a non-empty string"})
    if not out:
        out = os.path.splitext(image)[0] + suffix
    d = os.path.dirname(os.path.abspath(out))
    if not os.path.isdir(d) or not os.access(d, os.W_OK):
        raise Refusal({"error": "out_path_unwritable", "out_path": out})
    return out


def _save_png(arr, out: str) -> None:
    from PIL import Image

    tmp = out + ".part.png"
    Image.fromarray(arr).save(tmp, format="PNG", optimize=False)
    os.replace(tmp, out)


# ---------------------------------------------------------------- SigLIP2

def _image_vec(img):
    import numpy as np

    x = _resize(img, 224, 224).astype(np.float32) * (2.0 / 255.0) - 1.0  # (x/255 - 0.5) / 0.5
    v = _run("siglip_image", {"image": x.transpose(2, 0, 1)[None]})[0][0].astype(np.float32)
    return v / max(float(np.linalg.norm(v)), 1e-12)


def _text_vec(text: str):
    import numpy as np

    key = text
    v = _text_cache.get(key)
    if v is not None:
        _text_cache.move_to_end(key)
        return v
    ids = np.asarray([_tokenizer().encode(text.lower()).ids], dtype=np.int32)
    v = _run("siglip_text", {"text": ids})[0][0].astype(np.float32)
    v = v / max(float(np.linalg.norm(v)), 1e-12)
    _text_cache[key] = v
    while len(_text_cache) > 4096:
        _text_cache.popitem(last=False)
    return v


def _imagenet_matrix():
    """The 1000 ImageNet class-name embeddings, computed once with the text tower and cached on disk."""
    import numpy as np

    m = _aux.get("imagenet")
    if m is not None:
        return m
    with open(os.path.join(HERE, "imagenet_classes.txt"), encoding="utf-8") as fh:
        names = [ln.strip() for ln in fh if ln.strip()]
    digest = ((_record().get(_rel("siglip_text")) or {}).get("digest") or f"size{os.path.getsize(_path('siglip_text'))}")
    tag = hashlib.sha256((digest + "|" + CLASSIFY_TEMPLATE + "|" + "\n".join(names)).encode()).hexdigest()[:16]
    cache = os.path.join(CACHE_DIR, f"siglip2-imagenet-{tag}.npy")
    mat = None
    if os.path.isfile(cache):
        try:
            mat = np.load(cache)
            if mat.shape != (len(names), 768):
                mat = None
        except (OSError, ValueError):
            mat = None
    if mat is None:
        mat = np.stack([_text_vec(CLASSIFY_TEMPLATE.format(n)) for n in names]).astype(np.float32)
        os.makedirs(CACHE_DIR, exist_ok=True)
        tmp = cache + ".part.npy"
        np.save(tmp, mat)
        os.replace(tmp, cache)
        _evict("siglip_text")  # classify never needs the 466 MiB text tower again
        for k in [k for k in _text_cache if k.startswith("a photo of a ")]:
            _text_cache.pop(k, None)
    _aux["imagenet"] = (names, mat)
    return _aux["imagenet"]


def tool_classify(args: dict) -> dict:
    import numpy as np

    image = _image_arg(args)
    top_k = _int_arg(args, "top_k", 5, 1, 50)
    domain = args.get("domain", "imagenet")
    if domain != "imagenet":
        raise Refusal({"error": "bad_request", "detail": "domain must be \"imagenet\" (use zero_shot for any other label set)"})
    img = _rgb(image)
    names, mat = _imagenet_matrix()
    logits = (mat @ _image_vec(img)) * SIGLIP_SCALE + SIGLIP_BIAS
    p = np.exp(logits - logits.max())
    p /= p.sum()
    order = np.argsort(-p)[:top_k]
    results = [{"label": names[i], "score": round(float(p[i]), 5)} for i in order]
    return {"results": results, "best": results[0], "model": "SigLIP2 B/16-224 (zero-shot over ImageNet-1k names)",
            "domain": "imagenet"}


def _space(v) -> dict:
    return {"embedding": [round(float(x), 6) for x in v], "dim": int(v.shape[0]), "space": SIGLIP_SPACE,
            "model": "SigLIP2 B/16-224"}


def tool_embed(args: dict) -> dict:
    return _space(_image_vec(_rgb(_image_arg(args))))


def tool_text_embed(args: dict) -> dict:
    text = args.get("text")
    if not isinstance(text, str) or not text.strip():
        raise Refusal({"error": "bad_request", "detail": "text required"})
    return _space(_text_vec(text))


def tool_zero_shot(args: dict) -> dict:
    image = _image_arg(args)
    labels = args.get("labels")
    if not isinstance(labels, list) or not labels or len(labels) > 256 or not all(isinstance(x, str) and x.strip() for x in labels):
        raise Refusal({"error": "bad_request", "detail": "labels must be 1..256 non-empty strings"})
    template = args.get("template", "a photo of {}.")
    if not isinstance(template, str) or "{}" not in template:
        raise Refusal({"error": "bad_request", "detail": "template must be a string containing {}"})
    iv = _image_vec(_rgb(image))
    out = []
    for lab in labels:
        sim = float(_text_vec(template.format(lab.strip())) @ iv)
        logit = sim * SIGLIP_SCALE + SIGLIP_BIAS
        out.append({"label": lab, "similarity": round(sim, 5), "prob": round(1.0 / (1.0 + math.exp(-logit)), 5)})
    out.sort(key=lambda r: -r["similarity"])
    return {"results": out, "best": out[0], "model": "SigLIP2 B/16-224", "template": template}


# ---------------------------------------------------------------- RF-DETR

def _sigmoid(x):
    import numpy as np

    return 0.5 * (1.0 + np.tanh(0.5 * x))  # overflow-free


def _nms(boxes, scores, thr: float) -> list[int]:
    import numpy as np

    order = list(np.argsort(-scores))
    keep = []
    while order:
        i = order.pop(0)
        keep.append(int(i))
        if not order:
            break
        rest = np.array(order)
        x1 = np.maximum(boxes[i, 0], boxes[rest, 0]); y1 = np.maximum(boxes[i, 1], boxes[rest, 1])
        x2 = np.minimum(boxes[i, 2], boxes[rest, 2]); y2 = np.minimum(boxes[i, 3], boxes[rest, 3])
        inter = np.clip(x2 - x1, 0, None) * np.clip(y2 - y1, 0, None)
        area = lambda b: (b[..., 2] - b[..., 0]) * (b[..., 3] - b[..., 1])  # noqa: E731
        iou = inter / np.maximum(area(boxes[i]) + area(boxes[rest]) - inter, 1e-9)
        order = [o for o, v in zip(order, iou) if v < thr]
    return keep


def _detect(img, thr: float, only_person: bool = False) -> list[dict]:
    import numpy as np

    h, w = img.shape[:2]
    logits, feats, refs = _run("rfdetr", {"pixel_values": _resize(img, 512, 512)[None]})
    head = _box_head()
    boxes = head.run(["pred_boxes"], {"bbox_features": feats.astype(np.float32), "reference_boxes": refs.astype(np.float32)})[0][0]
    scores = _sigmoid(logits[0].astype(np.float32))
    scores[:, 0] = 0.0
    for i, n in enumerate(COCO):
        if n == "N/A":
            scores[:, i] = 0.0
    cls = scores.argmax(axis=1)
    best = scores[np.arange(len(cls)), cls]
    sel = np.where((best >= thr) & ((cls == 1) if only_person else True))[0]
    if not len(sel):
        return []
    cx, cy, bw, bh = boxes[sel].T
    xyxy = np.stack([(cx - bw / 2) * w, (cy - bh / 2) * h, (cx + bw / 2) * w, (cy + bh / 2) * h], axis=1)
    xyxy = np.clip(xyxy, 0, [w, h, w, h])
    out = []
    for c in np.unique(cls[sel]):  # class-wise NMS: DETR rarely needs it, but argmax-per-query can repeat a box
        idx = np.where(cls[sel] == c)[0]
        for j in _nms(xyxy[idx], best[sel][idx], 0.7):
            k = idx[j]
            x1, y1, x2, y2 = (float(v) for v in xyxy[k])
            out.append({"label": COCO[int(c)], "class_id": int(c), "x": round(x1, 1), "y": round(y1, 1),
                        "w": round(x2 - x1, 1), "h": round(y2 - y1, 1), "score": round(float(best[sel][k]), 4)})
    out.sort(key=lambda o: -o["score"])
    return out


def tool_object_detect(args: dict) -> dict:
    image = _image_arg(args)
    thr = _float_arg(args, "score_threshold", 0.5, 0.0, 1.0)
    img = _rgb(image)
    objs = _detect(img, thr)
    return {"objects": objs, "count": len(objs), "width": int(img.shape[1]), "height": int(img.shape[0]),
            "model": "RF-DETR small (COCO)"}


# ---------------------------------------------------------------- DeepLabV3+

def tool_semantic_segment(args: dict) -> dict:
    import numpy as np

    image = _image_arg(args)
    out = _out_path(args, image, ".segmask.png")
    img = _rgb(image)
    h, w = img.shape[:2]
    logits = _run("deeplab", {"input": _resize(img, 513, 513)[None]})[0][0]
    cls = logits.argmax(axis=0).astype(np.uint8)
    mask = _resize(cls, w, h, nearest=True)
    _save_png(mask, out)
    ids, counts = np.unique(mask, return_counts=True)
    classes = sorted(({"class_id": int(i), "label": VOC[int(i)] if int(i) < len(VOC) else str(int(i)), "pixels": int(c)}
                      for i, c in zip(ids, counts)), key=lambda r: -r["pixels"])
    return {"mask_path": out, "classes": classes, "width": int(w), "height": int(h),
            "model": "DeepLabV3+ MobileNetV2 (Pascal VOC, 21 classes)"}


# ---------------------------------------------------------------- Depth Anything 3

def tool_depth(args: dict) -> dict:
    import numpy as np

    image = _image_arg(args)
    out = _out_path(args, image, ".depth.png")
    img = _rgb(image)
    h, w = img.shape[:2]
    d = _run("da3", {"img": _resize(img, 504, 280)[None]})[0][0, 0].astype(np.float32)
    d = _resize(d, w, h)
    inv = 1.0 / np.clip(d, 1e-6, None)  # bright = near: inverse depth spreads the near range like a disparity map
    lo, hi = float(inv.min()), float(inv.max())
    png = ((inv - lo) / max(hi - lo, 1e-12) * 255.0).astype(np.uint8)
    _save_png(png, out)
    return {"depth_path": out, "min": round(float(d.min()), 4), "max": round(float(d.max()), 4),
            "mean": round(float(d.mean()), 4), "units": "relative depth (larger = farther); PNG is inverse depth, bright = near",
            "width": int(w), "height": int(h), "model": "Depth Anything 3 small"}


# ---------------------------------------------------------------- RTMPose

def _pose_one(img_bgr, box: dict):
    import cv2
    import numpy as np

    in_w, in_h = 192, 256
    cx, cy = box["x"] + box["w"] / 2, box["y"] + box["h"] / 2
    sw, sh = box["w"] * 1.25, box["h"] * 1.25
    if sw > sh * in_w / in_h:
        sh = sw * in_h / in_w
    else:
        sw = sh * in_w / in_h
    # Axis-aligned affine (no rotation): crop box -> 192x256, the RTMPose top-down convention.
    m = np.array([[in_w / sw, 0, in_w / 2 - cx * in_w / sw], [0, in_h / sh, in_h / 2 - cy * in_h / sh]], dtype=np.float32)
    crop = cv2.warpAffine(img_bgr, m, (in_w, in_h), flags=cv2.INTER_LINEAR)
    sx, sy = _run("rtmpose", {"input": crop[None]})
    xl, yl = sx[0].argmax(axis=1), sy[0].argmax(axis=1)
    xv, yv = sx[0].max(axis=1), sy[0].max(axis=1)
    kx = xl / 2.0 / in_w * sw + cx - sw / 2  # SimCC split ratio 2
    ky = yl / 2.0 / in_h * sh + cy - sh / 2
    sc = np.minimum(xv, yv)
    return {name: {"x": round(float(kx[i]), 1), "y": round(float(ky[i]), 1), "score": round(float(sc[i]), 4)}
            for i, name in enumerate(KEYPOINTS)}


def tool_pose(args: dict) -> dict:
    image = _image_arg(args)
    thr = _float_arg(args, "score_threshold", 0.5, 0.0, 1.0)
    img = _rgb(image)
    bgr = img[:, :, ::-1].copy()  # the RTMPose model swaps BGR->RGB inside, as its publisher's script feeds it
    people = []
    for p in _detect(img, thr, only_person=True)[:20]:
        people.append({k: p[k] for k in ("x", "y", "w", "h", "score")} | {"keypoints": _pose_one(bgr, p)})
    return {"people": people, "count": len(people), "skeleton": "COCO-17", "model": "RF-DETR small + RTMPose-M"}


# ---------------------------------------------------------------- MobileSAM

def _sam_embed(img):
    import numpy as np

    h, w = img.shape[:2]
    s = 1024.0 / max(h, w)
    nh, nw = int(round(h * s)), int(round(w * s))
    pad = np.zeros((1024, 1024, 3), dtype=np.uint8)
    pad[:nh, :nw] = _resize(img, nw, nh)
    return _run("sam_encoder", {"input": pad[None]})[0].astype(np.float32), s, nh, nw


def _sam_decode(emb, pts, labels):
    import numpy as np

    coords = np.zeros((1, 5, 2), dtype=np.float32)
    labs = np.full((1, 5), -1.0, dtype=np.float32)  # -1 = padding point, ignored by the decoder
    coords[0, :len(pts)] = pts
    labs[0, :len(labels)] = labels
    iou, masks = _run("sam_decoder", {"image_embeddings": emb, "point_coords": coords, "point_labels": labs,
                                      "mask_input": np.zeros((1, 1, 256, 256), dtype=np.float32),
                                      "has_mask_input": np.zeros((1,), dtype=np.float32)})
    return iou[0], masks[0]


def _sam_full(lowres, nh: int, nw: int, h: int, w: int):
    """A 256x256 low-res mask logit map -> a boolean mask at the image's own resolution."""
    up = _resize(lowres, 1024, 1024)[:nh, :nw]
    return _resize(up, w, h) > 0.0


def tool_segment(args: dict) -> dict:
    import numpy as np

    image = _image_arg(args)
    everything = _bool_arg(args, "everything", False)
    thr = _float_arg(args, "score_threshold", 0.5, 0.0, 1.0)
    out = _out_path(args, image, ".mask.png")
    img = _rgb(image)
    h, w = img.shape[:2]
    emb, s, nh, nw = _sam_embed(img)
    found = []  # (score, instance dict, boolean mask)
    if not everything:
        for o in _detect(img, thr)[:64]:
            pts = [[o["x"] * s, o["y"] * s], [(o["x"] + o["w"]) * s, (o["y"] + o["h"]) * s]]
            _, masks = _sam_decode(emb, pts, [2, 3])  # box prompt: the single-mask output (index 0)
            found.append((o["score"], dict(o), _sam_full(masks[0], nh, nw, h, w)))
    else:
        n = 16
        cands = []
        for gy in range(n):
            for gx in range(n):
                px, py = (gx + 0.5) * nw / n, (gy + 0.5) * nh / n
                iou, masks = _sam_decode(emb, [[px, py]], [1])
                j = 1 + int(np.argmax(iou[1:]))  # a point is ambiguous: the best of the three multimask outputs
                lo = masks[j]
                stab = float(((lo > 1.0).sum()) / max(int((lo > -1.0).sum()), 1))
                if iou[j] >= max(thr, 0.86) and stab >= 0.92:
                    cands.append((float(iou[j]), lo > 0.0))
        cands.sort(key=lambda c: -c[0])
        kept = []
        for score, m in cands:  # mask-IoU de-duplication on the low-res masks
            if m.sum() < 16 or any((m & k).sum() / max((m | k).sum(), 1) > 0.7 for _, k in kept):
                continue
            kept.append((score, m))
        for score, m in kept[:255]:
            full = _sam_full(m.astype(np.float32) * 2.0 - 1.0, nh, nw, h, w)
            ys, xs = np.where(full)
            if not len(xs):
                continue
            x1, y1, x2, y2 = int(xs.min()), int(ys.min()), int(xs.max()) + 1, int(ys.max()) + 1
            found.append((score, {"label": "object", "class_id": -1, "x": x1, "y": y1, "w": x2 - x1, "h": y2 - y1,
                                  "score": round(score, 4)}, full))
        found.sort(key=lambda f: -int(f[2].sum()))  # large first, so smaller objects paint on top
    mask = np.zeros((h, w), dtype=np.uint8)
    instances = []
    order = found if everything else sorted(found, key=lambda f: f[0])  # by score: the best instance owns overlaps
    for i, (_, inst, m) in enumerate(order[:255], start=1):
        mask[m] = i
        inst["pixels"] = int(m.sum())
        instances.append(inst)
    _save_png(mask, out)
    for i, inst in enumerate(instances, start=1):
        inst["pixels"] = int((mask == i).sum())
    return {"instances": instances, "mask_path": out, "count": len(instances), "everything": everything,
            "width": int(w), "height": int(h),
            "model": "MobileSAM" + (" (16x16 point grid)" if everything else " on RF-DETR small boxes")}


# ---------------------------------------------------------------- PP-OCRv5

def _ocr_dict() -> list[str]:
    d = _aux.get("ocr_dict")
    if d is None:
        with open(_checked("ocr_dict"), "rb") as fh:
            chars = [ln.decode("utf-8").rstrip("\r\n") for ln in fh]
        d = ["blank"] + chars + [" "]
        _aux["ocr_dict"] = d
    return d


def _mini_box(contour):
    import cv2

    rect = cv2.minAreaRect(contour)
    pts = sorted(list(cv2.boxPoints(rect)), key=lambda p: p[0])
    a, d = (0, 1) if pts[1][1] > pts[0][1] else (1, 0)
    b, c = (2, 3) if pts[3][1] > pts[2][1] else (3, 2)
    return [pts[a], pts[b], pts[c], pts[d]], min(rect[1])


def _ocr_boxes(prob, w: int, h: int):
    import cv2
    import numpy as np
    import pyclipper
    from shapely.geometry import Polygon

    ph, pw = prob.shape
    contours, _ = cv2.findContours(((prob > 0.3) * 255).astype(np.uint8), cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)
    boxes = []
    for cnt in contours[:1000]:
        pts, side = _mini_box(cnt)
        if side < 3:
            continue
        pts = np.array(pts)
        x0, x1 = int(np.clip(np.floor(pts[:, 0].min()), 0, pw - 1)), int(np.clip(np.ceil(pts[:, 0].max()), 0, pw - 1))
        y0, y1 = int(np.clip(np.floor(pts[:, 1].min()), 0, ph - 1)), int(np.clip(np.ceil(pts[:, 1].max()), 0, ph - 1))
        m = np.zeros((y1 - y0 + 1, x1 - x0 + 1), dtype=np.uint8)
        cv2.fillPoly(m, (pts - [x0, y0]).reshape(1, -1, 2).astype(np.int32), 1)
        score = cv2.mean(prob[y0:y1 + 1, x0:x1 + 1], m)[0]
        if score < 0.6:
            continue
        poly = Polygon(pts)
        off = pyclipper.PyclipperOffset()
        off.AddPath(pts.tolist(), pyclipper.JT_ROUND, pyclipper.ET_CLOSEDPOLYGON)
        grown = off.Execute(poly.area * 1.5 / max(poly.length, 1e-6))
        if not grown:
            continue
        box, side = _mini_box(np.array(grown[0]).reshape(-1, 1, 2).astype(np.float32))
        if side < 5:
            continue
        box = np.array(box, dtype=np.float32)
        box[:, 0] = np.clip(np.round(box[:, 0] / pw * w), 0, w - 1)
        box[:, 1] = np.clip(np.round(box[:, 1] / ph * h), 0, h - 1)
        s = box.sum(axis=1)
        tl, br = box[np.argmin(s)], box[np.argmax(s)]
        rest = np.delete(box, (np.argmin(s), np.argmax(s)), axis=0)
        diff = np.diff(rest, axis=1).ravel()
        ordered = np.array([tl, rest[np.argmin(diff)], br, rest[np.argmax(diff)]], dtype=np.float32)
        if np.linalg.norm(ordered[0] - ordered[1]) <= 3 or np.linalg.norm(ordered[0] - ordered[3]) <= 3:
            continue
        boxes.append((ordered, float(score)))
    boxes.sort(key=lambda b: (b[0][0][1], b[0][0][0]))
    for i in range(len(boxes) - 1):  # the publisher's reading order: same line (within 10 px) left to right
        for j in range(i, -1, -1):
            if abs(boxes[j + 1][0][0][1] - boxes[j][0][0][1]) < 10 and boxes[j + 1][0][0][0] < boxes[j][0][0][0]:
                boxes[j], boxes[j + 1] = boxes[j + 1], boxes[j]
            else:
                break
    return boxes


def _ocr_crop(img_bgr, pts):
    import cv2
    import numpy as np

    cw = int(max(np.linalg.norm(pts[0] - pts[1]), np.linalg.norm(pts[2] - pts[3])))
    ch = int(max(np.linalg.norm(pts[0] - pts[3]), np.linalg.norm(pts[1] - pts[2])))
    m = cv2.getPerspectiveTransform(pts, np.float32([[0, 0], [cw, 0], [cw, ch], [0, ch]]))
    crop = cv2.warpPerspective(img_bgr, m, (cw, ch), borderMode=cv2.BORDER_REPLICATE, flags=cv2.INTER_CUBIC)
    if crop.shape[0] / max(crop.shape[1], 1) >= 1.5:
        crop = np.rot90(crop)
    return crop


def _ocr_input(crop, ih: int, iw: int):
    import numpy as np

    h, w = crop.shape[:2]
    rw = min(iw, int(math.ceil(ih * w / max(h, 1))))
    x = np.full((3, ih, iw), 127.5, dtype=np.float32)  # raw pixels, mid-grey padding (see _ocr_read)
    x[:, :, :rw] = _resize(crop, rw, ih).astype(np.float32).transpose(2, 0, 1)
    return x[None]


REC_H, REC_W = 48, 320  # the recognizer is compiled at this fixed size; its CTC output has one step per 8 px
REC_STEP = 8
REC_STRIDE = 160  # windows overlap by half


def _ocr_read(crop, chars) -> tuple[str, float]:
    """Read one text line. The line is scaled to the recognizer's 48 px height at its own aspect; a line wider
    than one 320 px window is read in windows that overlap by half, and their per-step CTC outputs are
    stitched at a step where both neighbouring windows predict blank (nearest the overlap's centre), so no
    glyph is split at a seam; then one greedy CTC decode runs over the stitched sequence. Squashing a long
    line into one window, as the publisher's demo does, reads nothing past about 13:1 (measured: a 19:1
    line returned an empty string), and cutting it at ink gaps loses or invents spaces at the cuts."""
    import numpy as np

    h, w = crop.shape[:2]
    lw = max(1, int(math.ceil(REC_H * w / max(h, 1))))
    line = _resize(crop, lw, REC_H).astype(np.float32)
    steps = int(math.ceil(lw / REC_STEP))
    outs, off = [], 0
    while True:
        # Raw 0..255 (normalisation is compiled in), padded with 127.5: PaddleOCR pads with normalised zero
        # (mid-grey); the publisher's demo pads raw 0, and the edge into black reads as a stray "-".
        x = np.full((REC_H, REC_W, 3), 127.5, dtype=np.float32)
        seg = line[:, off:off + REC_W]
        x[:, :seg.shape[1]] = seg
        outs.append((off // REC_STEP, _run("ocr_rec", {"x": x.transpose(2, 0, 1)[None]})[0][0]))
        if off + REC_W >= lw:
            break
        off += REC_STRIDE
    parts, start = [], 0
    for k, (o, out) in enumerate(outs):
        if k + 1 < len(outs):
            o2, out2 = outs[k + 1]
            centre = (o2 + o + REC_W // REC_STEP) // 2
            blank = [t for t in range(o2 + 2, o + REC_W // REC_STEP - 2)
                     if int(out[t - o].argmax()) == 0 and int(out2[t - o2].argmax()) == 0]
            t = min(blank, key=lambda t: abs(t - centre)) if blank else centre
        else:
            t = steps
        parts.append(out[start - o:t - o])
        start = t
    seq = np.concatenate(parts)
    idx, prob = seq.argmax(axis=1), seq.max(axis=1)
    keep = np.ones(len(idx), dtype=bool)
    keep[1:] = idx[1:] != idx[:-1]
    keep &= idx != 0
    text = "".join(chars[i] if i < len(chars) else "" for i in idx[keep])
    return " ".join(text.split()), (float(prob[keep].mean()) if keep.any() else 0.0)


def tool_ocr(args: dict) -> dict:
    import numpy as np

    image = _image_arg(args)
    img = _rgb(image)
    h, w = img.shape[:2]
    bgr = np.ascontiguousarray(img[:, :, ::-1])  # PP-OCR is trained on BGR, as its publisher's script reads with cv2
    # Detection: aspect kept and padded to the compiled 960x960 (PaddleOCR's own convention). A plain stretch,
    # as the publisher's demo does, splits wide lines (measured: "The quick..." read as "Theo" + "e quick...").
    s = 960.0 / max(h, w)
    nh, nw = max(1, int(round(h * s))), max(1, int(round(w * s)))
    pad = np.full((960, 960, 3), 114, dtype=np.uint8)
    pad[:nh, :nw] = _resize(bgr, nw, nh)
    det = _run("ocr_det", {"x": pad.astype(np.float32).transpose(2, 0, 1)[None]})[0][0, 0][:nh, :nw]
    chars = _ocr_dict()
    lines = []
    for pts, det_score in _ocr_boxes(det, w, h):
        crop = _ocr_crop(bgr, pts)
        cls = _run("ocr_cls", {"x": _ocr_input(crop, 80, 160)})[0][0]
        if int(np.argmax(cls)) == 1 and float(cls[1]) > 0.9:
            crop = np.ascontiguousarray(crop[::-1, ::-1])  # upside-down line
        text, score = _ocr_read(crop, chars)
        if text and score >= 0.5:
            lines.append({"text": text, "score": round(score, 4),
                          "box": [[int(x), int(y)] for x, y in pts.tolist()]})
    return {"text": "\n".join(ln["text"] for ln in lines), "lines": lines, "count": len(lines),
            "width": int(w), "height": int(h), "model": "PP-OCRv5 mobile (det + cls + rec)"}


TOOLS = {"classify": tool_classify, "object_detect": tool_object_detect, "semantic_segment": tool_semantic_segment,
         "segment": tool_segment, "embed": tool_embed, "text_embed": tool_text_embed, "zero_shot": tool_zero_shot,
         "depth": tool_depth, "pose": tool_pose, "ocr": tool_ocr}


# ---------------------------------------------------------------- telemetry, perf, health

def _child(*extra: str, timeout: float = 8.0) -> dict:
    try:
        r = subprocess.run([sys.executable, TELEMETRY, *extra], capture_output=True, text=True, timeout=timeout)
        return json.loads(r.stdout or "{}")
    except (OSError, subprocess.TimeoutExpired, ValueError) as e:
        return {"error": f"{type(e).__name__}: {e}"}


def _refresh_telemetry() -> None:
    doc = _child()
    with _tel_lock:
        _tel.update(doc=doc, at=time.monotonic(), running=False)


def _telemetry(wait: float = 0.0):
    """The newest sample with its age. A sample older than TELEMETRY_TTL kicks one background refresh (never
    two); one older than three TTLs (nobody asked for a while) is not served stale: the caller waits up to
    `wait` seconds for the fresh read (one read takes about 0.6 s)."""
    with _tel_lock:
        at = _tel["at"]
        if time.monotonic() - at > TELEMETRY_TTL and not _tel["running"]:
            _tel["running"] = True
            threading.Thread(target=_refresh_telemetry, daemon=True).start()
    if wait and (_tel["doc"] is None or time.monotonic() - at > 3 * TELEMETRY_TTL):
        deadline = time.monotonic() + wait
        while _tel["at"] == at and time.monotonic() < deadline:
            time.sleep(0.05)
    doc = _tel["doc"]
    if doc is None:
        return None
    return {**doc, "age_sec": round(time.monotonic() - _tel["at"], 1)}


def _runtime_info() -> dict:
    info = {"python": sys.version.split()[0]}
    for pkg in ("axengine", "numpy", "onnxruntime", "opencv-python-headless", "tokenizers"):
        try:
            info[pkg] = importlib.metadata.version(pkg)
        except importlib.metadata.PackageNotFoundError:
            info[pkg] = None
    return info


_RUNTIME = None


def _health() -> dict:
    global _RUNTIME
    if _RUNTIME is None:
        _RUNTIME = _runtime_info()
    rec = _record()
    missing, unverified = set(), set()
    for key in FILES:
        rel = _rel(key)
        if not os.path.isfile(_path(key)):
            missing.add(rel)
        elif rel not in rec:
            unverified.add(rel)
    tools_ready = sorted(t for t, ks in TOOL_FILES.items() if not any(_rel(k) in missing for k in ks))
    return {"enabled": ENABLED, "device": DEVICE, "runtime": _RUNTIME, "loaded": list(_sessions),
            "cmm_resident_mib": _resident_mib(), "cmm_budget_mib": CMM_BUDGET_MIB,
            "models_missing": sorted(missing), "models_unverified": sorted(unverified), "tools": sorted(TOOLS),
            "tools_ready": tools_ready, "perf": _perf, "telemetry": _telemetry(wait=1.5) if ENABLED else None,
            "uptime_sec": int(time.time() - _started), "idle_sec": IDLE_SEC}


class Handler(BaseHTTPRequestHandler):
    server_version = "axera-sidecar/1"

    def log_message(self, fmt, *args):  # quiet by default; AXERA_LOG=1 to see requests
        if os.environ.get("AXERA_LOG") == "1":
            super().log_message(fmt, *args)

    def _send(self, code: int, obj: dict):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # Deliberately no _touch(): /health must never keep a model resident.
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
        except ValueError as e:
            return self._send(400, {"error": "bad_request", "detail": str(e)})
        if not isinstance(args, dict):
            return self._send(400, {"error": "bad_request", "detail": "body must be a JSON object"})
        try:
            with _lock:
                try:
                    t0 = time.perf_counter()
                    result = fn(args)
                    if isinstance(result, dict):
                        result["elapsed_ms"] = round((time.perf_counter() - t0) * 1000, 1)
                finally:
                    _touch()  # idle counts from the END of the call (see the watchdog)
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
                _release()  # the card's CMM goes back before the process does
            finally:
                _lock.release()
            print(f"axera sidecar: idle {IDLE_SEC}s, exiting", file=sys.stderr)
            threading.Thread(target=server.shutdown, daemon=True).start()
            return


def _prefer_as_oom_victim(path: str = OOM_SCORE_ADJ) -> None:
    if not os.path.exists(path):
        return
    try:
        with open(path, "w") as fh:
            fh.write(OOM_SCORE)
    except OSError as e:
        print(f"axera sidecar: could not set oom_score_adj to {OOM_SCORE}: {e}", file=sys.stderr)


def _apply_perf() -> None:
    doc = _child("--apply-perf", timeout=10.0)
    _perf.clear()
    _perf.update({"applied": not doc.get("error"), **{k: v for k, v in doc.items() if k != "source"}})


def main():
    _prefer_as_oom_victim()
    try:
        import cv2

        cv2.setNumThreads(2)
    except ImportError:
        pass
    if ENABLED:
        _apply_perf()
        _telemetry()
    srv = ThreadingHTTPServer((BIND, PORT), Handler)
    threading.Thread(target=_idle_watchdog, args=(srv,), daemon=True).start()
    print(f"axera sidecar: listening on http://{BIND}:{PORT} enabled={ENABLED} models_dir={MODELS_DIR} "
          f"idle={IDLE_SEC}s cmm_budget={CMM_BUDGET_MIB}MiB perf={_perf}", file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
