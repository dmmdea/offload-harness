#!/usr/bin/env python3
"""Tests for the RKNPU sidecar, runnable on any box: no NPU, no rknn-toolkit-lite2, no board.

    python -B -m unittest discover -s accelerators/rknpu -v        (or: python -B accelerators/rknpu/test_server.py)

Four layers, all in this file:
  * the wire contract of the real process in RKNPU_ENABLED=0 stub mode (the Coral suite's shape): /health, the
    404 and 400 paths, npu_disabled, the sha256 gate, the loopback refusal and the idle self-exit;
  * server.py imported in-process against a fake RKNNLite that records what the sidecar did to it and returns
    canned tensors: the input layout, YOLOv8 decoding, residency, the lock, the idle watchdog, /health;
  * the manifest itself: every artifact pinned by a real hash, every reference resolvable;
  * the two shell scripts, under bash when the box has one: the launcher against a stub python with stub
    taskset and nice, fetch-models.sh against file:// URLs.
Tests that decode images need numpy and Pillow (the sidecar's own dependencies) and skip without them.
"""
from __future__ import annotations

import contextlib
import functools
import hashlib
import http.client
import importlib.metadata
import importlib.util
import io
import json
import os
import pathlib
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from unittest import mock

sys.dont_write_bytecode = True  # this directory is not gitignored for bytecode: keep test runs from littering it

try:
    import numpy as np
    from PIL import Image
except ImportError:  # the stub-mode, manifest and script layers need neither
    np = Image = None

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.join(HERE, "server.py")
MANIFEST = os.path.join(HERE, "models.json")
NEEDS_IMAGING = unittest.skipUnless(Image is not None, "numpy and Pillow are the sidecar's own dependencies")

# ---------------------------------------------------------------- helpers

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # loopback never goes through a proxy


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def call(port: int, method: str, path: str, body=None, raw: bytes | None = None):
    data = raw if raw is not None else (None if body is None else json.dumps(body).encode())
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with _OPENER.open(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        with e:  # closes the response the error wraps
            return e.code, json.loads(e.read() or b"{}")


def clean_env(**extra) -> dict:
    """The caller's environment minus every RKNPU_* (a developer's own settings must not reach a test)."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("RKNPU_")}
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env.update(extra)
    return env


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def write(path: str, data: bytes | str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(data if isinstance(data, bytes) else data.encode())


def read_json(path: str) -> dict:
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def real_manifest() -> dict:
    return read_json(MANIFEST)


COCO = ["person", "bicycle", "car", "motorcycle", "airplane", "bus"] + [f"coco{i}" for i in range(6, 80)]
SPACE_SHUTTLE = 812  # its ImageNet index, so the fake synset below reads like the real one


def label_files() -> dict:
    return {
        "imagenet_synset.txt": "".join(f"n{i:08d} {'space shuttle' if i == SPACE_SHUTTLE else f'class{i}, alias{i}'}\n"
                                       for i in range(1000)),
        "coco_80_labels_list.txt": "".join(name + "\n" for name in COCO),
    }


def write_artifacts(root: str, labels: dict | None = None) -> tuple[str, str]:
    """Dummy model files and label files under root/models, and a copy of the real manifest under root that pins
    exactly them, so the sha256 gate passes on bytes the fake runtime never parses. Returns (models dir, manifest)."""
    models = os.path.join(root, "models")
    man = real_manifest()
    files = label_files()
    files.update(labels or {})
    for key, spec in man["models"].items():
        data = f"rknn:{key}".encode()
        write(os.path.join(models, spec["file"]), data)
        spec["sha256"] = sha(data)
    for name, spec in man["labels"].items():
        data = files[name].encode()
        write(os.path.join(models, name), data)
        spec["sha256"] = sha(data)
    path = os.path.join(root, "models.json")
    write(path, json.dumps(man))
    return models, path


def load_server(**env):
    """Import a private copy of server.py with RKNPU_* set as given (the module reads them at import time)."""
    saved = os.environ.copy()
    try:
        for k in [k for k in os.environ if k.startswith("RKNPU_")]:
            del os.environ[k]
        os.environ.update(env)
        spec = importlib.util.spec_from_file_location("rknpu_server_under_test", SERVER)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        os.environ.clear()
        os.environ.update(saved)
    return mod


def make_fake_runtime():
    """A fresh fake RKNNLite class (its state lives on the class: one per test) and the modules that expose it.

    Like the real one it reports failure by return value: load_rknn / init_runtime return non-zero, inference
    returns None. `events` is the order of load / release calls across instances."""

    class FakeRKNNLite:
        NPU_CORE_0_1_2 = 7  # the wheel's value: cores 0, 1 and 2
        instances: list = []
        events: list = []
        outputs: dict = {}      # model file name -> callable(inputs) -> list of arrays
        load_ret = 0
        init_ret = 0
        on_inference = None     # callable(instance, inputs), run first inside inference()
        release_error = None    # an exception release() raises once

        def __init__(self):
            self.path = self.core_mask = None
            self.released = False
            self.inputs: list = []
            self.rknn_data = b"the model file, held in Python memory by load_rknn"
            type(self).instances.append(self)

        def load_rknn(self, path):
            self.path = path
            type(self).events.append(("load", os.path.basename(path)))
            return type(self).load_ret

        def init_runtime(self, core_mask=None):
            self.core_mask = core_mask
            return type(self).init_ret

        def inference(self, inputs):
            self.inputs.append(inputs[0])
            if type(self).on_inference:
                type(self).on_inference(self, inputs)
            fn = type(self).outputs.get(os.path.basename(self.path))
            return None if fn is None else fn(inputs)

        def release(self):
            self.released = True
            type(self).events.append(("release", os.path.basename(self.path)))
            err, type(self).release_error = type(self).release_error, None
            if err:
                raise err

    api = types.ModuleType("rknnlite.api")
    api.RKNNLite = FakeRKNNLite
    pkg = types.ModuleType("rknnlite")
    pkg.api = api
    pkg.__path__ = []
    return FakeRKNNLite, {"rknnlite": pkg, "rknnlite.api": api}


def yolo_outputs(cells, classes: int = 80):
    """The nine tensors the rknn_model_zoo yolov8 model returns: (box[1,64,g,g], class[1,80,g,g], score-sum
    [1,1,g,g]) for the 80, 40 and 20 grids. Each cell is a hot grid position: a class score and, per side
    (left, top, right, bottom), the distance in grid cells that the DFL bins are peaked at."""
    outs = []
    for g in (80, 40, 20):
        outs += [np.zeros((1, 64, g, g), np.float32), np.zeros((1, classes, g, g), np.float32),
                 np.zeros((1, 1, g, g), np.float32)]
    for c in cells:
        box, cls = outs[3 * c["branch"]], outs[3 * c["branch"] + 1]
        for side, dist in enumerate(c["dist"]):
            box[0, side * 16 + dist, c["gy"], c["gx"]] = 30.0
        cls[0, c["cls"], c["gy"], c["gx"]] = c["score"]
    return outs


def hot(gx, gy, cls=5, score=0.9, branch=1, dist=(2, 3, 4, 1)):
    return {"branch": branch, "gx": gx, "gy": gy, "cls": cls, "score": score, "dist": dist}


def bash_ok(*needs: str) -> str | None:
    """A bash that can run these scripts, or None (a Windows box may resolve `bash` to the WSL launcher)."""
    return _bash(needs)


@functools.lru_cache(maxsize=None)
def _bash(needs: tuple) -> str | None:
    exe = shutil.which("bash")
    if exe is None:
        return None
    probe = 'cd "$1" && echo ok' + "".join(f" && command -v {n} >/dev/null" for n in needs)
    if "python3" in needs:
        probe += " && python3 -c pass"
    with tempfile.TemporaryDirectory() as d:
        try:
            r = subprocess.run([exe, "-c", probe, "_", d], capture_output=True, text=True, timeout=60)
        except (OSError, subprocess.SubprocessError):
            return None
    return exe if r.returncode == 0 and r.stdout.strip() == "ok" else None


def native(path: str) -> str:
    """A path as the launcher's bash printed it, in this OS's own form (Git Bash prints /tmp/x for a Windows temp dir)."""
    bash = bash_ok()
    if os.name == "nt" and bash and path.startswith("/"):
        r = subprocess.run([bash, "-c", 'cygpath -m "$1"', "_", path], capture_output=True, text=True, timeout=30)
        if r.returncode == 0 and r.stdout.strip():
            return r.stdout.strip()
    return path


def same_path(a: str, b: str) -> bool:
    return os.path.normcase(os.path.realpath(native(a))) == os.path.normcase(os.path.realpath(native(b)))


# ---------------------------------------------------------------- the real process, NPU stubbed

class StartedProcess:
    def __init__(self, models_dir: str, **env):
        self.port = free_port()
        e = clean_env(RKNPU_ENABLED="0", RKNPU_PORT=str(self.port), RKNPU_MODELS_DIR=models_dir, RKNPU_IDLE_SEC="0",
                      RKNPU_SYSFS=os.path.join(models_dir, "no-sysfs"))
        e.update(env)
        self.p = subprocess.Popen([sys.executable, "-B", SERVER], env=e, stderr=subprocess.PIPE, text=True)
        for _ in range(200):
            try:
                call(self.port, "GET", "/health")
                return
            except OSError:
                if self.p.poll() is not None:
                    raise RuntimeError("server exited: " + self.p.stderr.read())
                time.sleep(0.05)
        self.stop()
        raise RuntimeError("server did not come up")

    def stop(self):
        self.p.kill()
        self.p.wait()
        self.p.stderr.close()


class ProcessContract(unittest.TestCase):
    """The wire contract the harness's accelclient depends on, against the real process with the NPU stubbed."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.proc = StartedProcess(cls.tmp.name)
        cls.port = cls.proc.port

    @classmethod
    def tearDownClass(cls):
        cls.proc.stop()
        cls.tmp.cleanup()

    def test_health_is_200_with_the_documented_keys(self):
        code, h = call(self.port, "GET", "/health")
        self.assertEqual(code, 200)
        for k in ("enabled", "device", "runtime", "temp_c", "npu_load", "loaded", "models_missing", "tools",
                  "uptime_sec", "idle_sec"):
            self.assertIn(k, h)
        self.assertIs(h["enabled"], False)
        self.assertEqual(h["device"], "rknpu")
        self.assertEqual(h["core_mask"], "0_1_2")
        self.assertEqual(h["tools"], ["classify", "embed", "object_detect"])
        self.assertEqual(h["loaded"], [])
        self.assertIsNone(h["temp_c"])       # no sysfs tree: degrades to null, never an error
        self.assertIsNone(h["npu_load"])

    def test_models_missing_lists_every_manifest_file_on_an_empty_dir(self):
        _, h = call(self.port, "GET", "/health")
        man = real_manifest()
        want = {s["file"] for s in man["models"].values()} | {s["labels"] for s in man["models"].values() if "labels" in s}
        self.assertEqual(set(h["models_missing"]), want)
        self.assertGreaterEqual(len(want), 5)  # three models and two label files

    def test_unknown_paths_and_tools(self):
        code, r = call(self.port, "GET", "/nope")
        self.assertEqual((code, r.get("error")), (404, "not_found"))
        code, r = call(self.port, "POST", "/v1/does_not_exist", {})
        self.assertEqual((code, r.get("error")), (404, "unknown_tool"))
        self.assertEqual(r["known"], ["classify", "embed", "object_detect"])
        code, r = call(self.port, "POST", "/other", {})
        self.assertEqual((code, r.get("error")), (404, "not_found"))

    def test_bad_requests_are_400(self):
        for tool, body in (("classify", {}), ("classify", {"image_path": "/x.jpg", "domain": "dogs"}),
                           ("classify", {"image_path": "/x.jpg", "top_k": "five"}),
                           ("object_detect", {"image_path": "/x.jpg", "score_threshold": 7}),
                           ("embed", {})):
            with self.subTest(tool=tool, body=body):
                code, r = call(self.port, "POST", f"/v1/{tool}", body)
                self.assertEqual((code, r.get("error")), (400, "bad_request"), r)

    def test_a_valid_call_with_the_npu_stubbed_is_a_structured_200(self):
        for tool in ("classify", "object_detect", "embed"):
            with self.subTest(tool=tool):
                code, r = call(self.port, "POST", f"/v1/{tool}", {"image_path": "/x.jpg"})
                self.assertEqual((code, r), (200, {"error": "npu_disabled"}))

    def test_malformed_bodies_are_400(self):
        for raw in (b"not json", b"[1, 2]", b'"text"', b"\xff\xfe"):
            with self.subTest(raw=raw):
                code, r = call(self.port, "POST", "/v1/embed", raw=raw)
                self.assertEqual((code, r.get("error")), (400, "bad_request"))

    @NEEDS_IMAGING
    def test_wrong_bytes_are_refused_not_loaded_and_absent_files_reported(self):
        with tempfile.TemporaryDirectory() as d:
            models, manifest = write_artifacts(d)  # dummy artifacts, pinned by a copy of the manifest
            spec = read_json(manifest)["models"]
            name = spec["resnet18"]["file"]
            write(os.path.join(models, name), b"not a model")
            os.remove(os.path.join(models, spec["yolov8n"]["file"]))
            img = os.path.join(d, "a.png")
            Image.new("RGB", (32, 32), (1, 2, 3)).save(img)
            proc = StartedProcess(models, RKNPU_ENABLED="1", RKNPU_MANIFEST=manifest)
            try:
                code, r = call(proc.port, "POST", "/v1/classify", {"image_path": img})
                self.assertEqual((code, r["error"], r["file"]), (200, "model_sha_mismatch", name), r)
                self.assertEqual((r["expected"], r["got"]), (spec["resnet18"]["sha256"], sha(b"not a model")))
                code, r = call(proc.port, "POST", "/v1/object_detect", {"image_path": img})
                self.assertEqual((code, r["error"], r["file"]), (200, "model_missing", spec["yolov8n"]["file"]), r)
                self.assertEqual(call(proc.port, "GET", "/health")[1]["loaded"], [])
            finally:
                proc.stop()


class ProcessLifecycle(unittest.TestCase):
    def test_a_non_loopback_bind_is_refused_at_startup(self):
        env = clean_env(RKNPU_ENABLED="0", RKNPU_BIND="0.0.0.0", RKNPU_PORT=str(free_port()))
        r = subprocess.run([sys.executable, "-B", SERVER], env=env, capture_output=True, text=True, timeout=30)
        self.assertEqual(r.returncode, 2, r.stderr)
        self.assertIn("non-loopback", r.stderr)

    def test_the_process_exits_by_itself_when_idle_even_while_health_is_polled(self):
        """/health is how the harness asks whether the sidecar is up: it must never keep a model resident."""
        with tempfile.TemporaryDirectory() as d:
            proc = StartedProcess(d, RKNPU_IDLE_SEC="2")
            t0 = time.monotonic()
            try:
                while proc.p.poll() is None and time.monotonic() - t0 < 30:
                    with contextlib.suppress(OSError):
                        call(proc.port, "GET", "/health")
                    time.sleep(0.2)
                self.assertIsNotNone(proc.p.poll(), "still alive after 30 s of polling /health")
                self.assertEqual(proc.p.returncode, 0)
                self.assertGreater(time.monotonic() - t0, 1.0, "exited before it was idle")
            finally:
                proc.stop()


# ---------------------------------------------------------------- the server module against a fake RKNNLite

@NEEDS_IMAGING
class InProcess(unittest.TestCase):
    """server.py imported into this process, an HTTP server on a free port, and rknnlite replaced by a fake."""

    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.tmp = d.name
        self.sysfs = os.path.join(self.tmp, "sys")
        os.makedirs(self.sysfs)
        self.Fake, modules = make_fake_runtime()
        patcher = mock.patch.dict(sys.modules, modules)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.httpd = None
        self.rewrite()
        self.addCleanup(self._stop_http)

    def rewrite(self, labels=None, **env):
        """(Re)write the dummy artifacts and start a fresh server module on top of them."""
        self._stop_http()
        self.models, self.manifest = write_artifacts(self.tmp, labels)
        self.spec = real_manifest()["models"]
        self.file = {k: v["file"] for k, v in self.spec.items()}
        e = {"RKNPU_ENABLED": "1", "RKNPU_MANIFEST": self.manifest, "RKNPU_MODELS_DIR": self.models,
             "RKNPU_SYSFS": self.sysfs, "RKNPU_IDLE_SEC": "0"}
        e.update(env)
        self.mod = load_server(**e)
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), self.mod.Handler)
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.port = self.httpd.server_address[1]

    def _stop_http(self):
        if self.httpd is not None:
            self.httpd.shutdown()
            self.httpd.server_close()
            self.httpd = None

    def post(self, tool, **args):
        return call(self.port, "POST", f"/v1/{tool}", args)

    def health(self):
        return call(self.port, "GET", "/health")[1]

    def image(self, name, size=(64, 64), color=(1, 2, 3)):
        path = os.path.join(self.tmp, name)
        Image.new("RGB", size, color).save(path)
        return path

    def bands(self, name, colors, band, height):
        img = Image.new("RGB", (band * len(colors), height))
        for i, c in enumerate(colors):
            img.paste(Image.new("RGB", (band, height), c), (i * band, 0))
        path = os.path.join(self.tmp, name)
        img.save(path)
        return path

    def outputs(self, key, fn):
        self.Fake.outputs[self.file[key]] = fn


class IdleClampTests(InProcess):
    """RKNPU_IDLE_SEC (the launcher's --idle-sec) is clamped to 1..300: no model may idle past five minutes."""

    def idle(self, raw):
        with mock.patch("sys.stderr", io.StringIO()) as err:
            self.rewrite(RKNPU_IDLE_SEC=raw)
        return self.mod.IDLE_SEC, err.getvalue()

    def test_zero_negative_and_over_300_all_become_300_with_a_log_line(self):
        for raw in ("0", "-1", "-300", "301", "86400"):
            with self.subTest(raw):
                got, log = self.idle(raw)
                self.assertEqual(got, 300)
                self.assertIn(f"idle seconds {raw} is outside 1..300, using 300", log)
                self.assertEqual(self.health()["idle_sec"], 300)

    def test_in_range_values_pass_untouched_and_silently(self):
        for raw in ("1", "45", "300"):
            with self.subTest(raw):
                got, log = self.idle(raw)
                self.assertEqual((got, log), (int(raw), ""))

    def test_the_default_is_300(self):
        self.assertEqual(self.mod._clamp_idle(300), 300)
        with mock.patch("sys.stderr", io.StringIO()):
            self.assertEqual(load_server(RKNPU_MANIFEST=self.manifest).IDLE_SEC, 300)


class OomVictimTests(InProcess):
    def test_the_process_asks_to_be_the_oom_killers_first_pick(self):
        path = os.path.join(self.tmp, "oom_score_adj")
        pathlib.Path(path).write_text("0")
        self.mod._prefer_as_oom_victim(path)
        self.assertEqual(pathlib.Path(path).read_text(), "500")
        absent = os.path.join(self.tmp, "absent")
        self.mod._prefer_as_oom_victim(absent)  # not Linux: nothing to write, nothing created
        self.assertFalse(os.path.exists(absent))
        with mock.patch("sys.stderr", io.StringIO()) as err:
            self.mod._prefer_as_oom_victim(self.tmp)  # unwritable (a directory here): logged, never raised
        self.assertIn("could not set oom_score_adj", err.getvalue())

    def test_main_sets_the_oom_score_before_it_serves(self):
        with mock.patch.object(self.mod, "_prefer_as_oom_victim") as prefer,                 mock.patch.object(self.mod, "ThreadingHTTPServer", side_effect=RuntimeError("stop before serving")):
            with self.assertRaises(RuntimeError):
                self.mod.main()
        prefer.assert_called_once_with()


class ClassifyTests(InProcess):
    def logits(self, **hot_):
        v = np.zeros((1, 1000), np.float32)
        for i, x in hot_.items():
            v[0, int(i[1:])] = x
        return v

    def test_scores_are_a_softmax_and_labels_lose_their_synset_id(self):
        v = self.logits(i812=10.0, i3=8.0)
        self.outputs("resnet18", lambda _: [v])
        code, r = self.post("classify", image_path=self.image("a.png"), top_k=3)
        self.assertEqual(code, 200, r)
        self.assertEqual(set(r), {"results", "best", "model", "domain"})
        self.assertEqual([x["label"] for x in r["results"][:2]], ["space shuttle", "class3, alias3"])
        self.assertEqual(len(r["results"]), 3)
        self.assertAlmostEqual(r["results"][0]["score"], np.exp(10) / (np.exp(10) + np.exp(8) + 998), places=5)
        self.assertGreater(r["results"][0]["score"], r["results"][1]["score"])
        self.assertEqual(r["best"], r["results"][0])
        self.assertEqual((r["model"], r["domain"]), (self.file["resnet18"], "imagenet"))
        self.assertEqual(len(self.post("classify", image_path=self.image("a.png"))[1]["results"]), 5)  # top_k defaults to 5

    def test_the_input_is_4d_nhwc_uint8_rgb_squashed_on_all_three_cores(self):
        self.outputs("resnet18", lambda _: [self.logits(i1=1.0)])
        # Three vertical bands: a squash (the Coral and Rockchip behaviour) keeps all three, a crop would not.
        path = self.bands("bands.png", [(255, 0, 0), (0, 255, 0), (0, 0, 255)], 224, 224)
        code, _ = self.post("classify", image_path=path)
        self.assertEqual(code, 200)
        rt = self.Fake.instances[0]
        self.assertEqual(rt.core_mask, self.Fake.NPU_CORE_0_1_2)
        x = rt.inputs[0]
        self.assertEqual((x.shape, x.dtype), ((1, 224, 224, 3), np.uint8))
        self.assertEqual([tuple(int(v) for v in x[0, 100, c]) for c in (10, 112, 213)],
                         [(255, 0, 0), (0, 255, 0), (0, 0, 255)])

    def test_a_large_jpeg_is_reduced_to_the_model_input(self):
        self.outputs("resnet18", lambda _: [self.logits(i1=1.0)])
        path = os.path.join(self.tmp, "big.jpg")
        Image.new("RGB", (1600, 1200), (200, 100, 50)).save(path, quality=95)
        code, _ = self.post("classify", image_path=path)
        self.assertEqual(code, 200)
        x = self.Fake.instances[0].inputs[0]
        self.assertEqual(x.shape, (1, 224, 224, 3))
        self.assertTrue(np.allclose(x[0, 100, 100], (200, 100, 50), atol=6), x[0, 100, 100])

    def test_a_label_list_shorter_than_the_logits_falls_back_to_the_index(self):
        self.rewrite(labels={"imagenet_synset.txt": "n00000000 only one\n"})
        self.outputs("resnet18", lambda _: [self.logits(i5=9.0)])
        _, r = self.post("classify", image_path=self.image("a.png"), top_k=1)
        self.assertEqual(r["best"]["label"], "5")

    def test_bad_arguments_are_refused_before_any_npu_work(self):
        img = self.image("a.png")
        for args in ({}, {"image_path": 5}, {"image_path": ""}, {"image_path": img, "domain": "birds"},
                     {"image_path": img, "domain": 3}, {"image_path": img, "top_k": 0},
                     {"image_path": img, "top_k": 101}, {"image_path": img, "top_k": "5"},
                     {"image_path": img, "top_k": 2.5}, {"image_path": img, "top_k": True}):
            with self.subTest(args=args):
                code, r = self.post("classify", **args)
                self.assertEqual((code, r["error"]), (400, "bad_request"), r)
        self.assertEqual(self.Fake.instances, [])

    def test_the_stub_answers_before_it_touches_an_image_or_a_library(self):
        self.rewrite(RKNPU_ENABLED="0")
        for tool in ("classify", "object_detect", "embed"):
            code, r = self.post(tool, image_path=os.path.join(self.tmp, "missing.png"))
            self.assertEqual((code, r), (200, {"error": "npu_disabled"}))
        self.assertEqual(self.Fake.instances, [])


class DetectTests(InProcess):
    def detect(self, cells, size=(640, 640), **args):
        self.outputs("yolov8n", lambda _: yolo_outputs(cells))
        return self.post("object_detect", image_path=self.image("scene.png", size), **args)

    def assertBox(self, obj, x, y, w, h, label="bus", cid=5, score=0.9):
        self.assertEqual((obj["label"], obj["class_id"]), (label, cid))
        for got, want, name in ((obj["x"], x, "x"), (obj["y"], y, "y"), (obj["w"], w, "w"), (obj["h"], h, "h"),
                                (obj["score"], score, "score")):
            self.assertAlmostEqual(got, want, delta=0.01, msg=name)

    def test_a_cell_decodes_through_dfl_and_the_grid_into_image_pixels(self):
        # stride 16, cell (10, 20), distances (2, 3, 4, 1) cells: x 136..232, y 280..344 in model pixels.
        code, r = self.detect([hot(10, 20)])
        self.assertEqual(code, 200, r)
        self.assertEqual(set(r), {"objects", "count", "model", "image_width", "image_height"})
        self.assertEqual((r["count"], r["image_width"], r["image_height"], r["model"]), (1, 640, 640, self.file["yolov8n"]))
        self.assertEqual(set(r["objects"][0]), {"label", "class_id", "x", "y", "w", "h", "score"})
        self.assertBox(r["objects"][0], 136, 280, 96, 64)

    def test_boxes_are_mapped_back_through_the_letterbox(self):
        cases = (
            # (image size, cell x, expected x y w h): scale 0.5 with 160 rows of padding above and below
            ((1280, 640), 10, (272, 240, 192, 128)),
            # scale 1 with 160 columns of padding left and right: the model box is shifted back by the padding
            ((320, 640), 20, (136, 280, 96, 64)),
            # scale 2, no padding
            ((320, 320), 10, (68, 140, 48, 32)),
        )
        for size, gx, want in cases:
            with self.subTest(size=size):
                _, r = self.detect([hot(gx, 20)], size=size)
                self.assertEqual(r["count"], 1, r)
                self.assertBox(r["objects"][0], *want)

    def test_the_image_is_letterboxed_on_black_into_a_4d_uint8_input(self):
        self.detect([], size=(1280, 640))
        rt = self.Fake.instances[0]
        self.assertEqual(rt.core_mask, self.Fake.NPU_CORE_0_1_2)
        x = rt.inputs[0]
        self.assertEqual((x.shape, x.dtype), ((1, 640, 640, 3), np.uint8))
        rows = [tuple(int(v) for v in x[0, r, 320]) for r in (0, 159, 160, 479, 480, 639)]
        black, colour = (0, 0, 0), (1, 2, 3)
        self.assertEqual(rows, [black, black, colour, colour, black, black])

    def test_boxes_are_clamped_to_the_image(self):
        _, r = self.detect([hot(1, 1, cls=0, branch=0, dist=(3, 3, 3, 3)),
                            hot(19, 19, cls=1, score=0.8, branch=2, dist=(1, 1, 3, 3))])
        top_left, bottom_right = r["objects"]
        self.assertBox(top_left, 0, 0, 36, 36, label="person", cid=0)
        self.assertBox(bottom_right, 592, 592, 48, 48, label="bicycle", cid=1, score=0.8)

    def test_nms_suppresses_overlaps_within_a_class_only(self):
        near = 11  # one cell to the right: IoU 0.71 with the strongest box
        _, r = self.detect([hot(10, 20), hot(near, 20, score=0.8)])
        self.assertEqual([round(o["score"], 2) for o in r["objects"]], [0.9], r)
        _, r = self.detect([hot(10, 20), hot(near, 20, cls=6, score=0.8)])
        self.assertEqual([(o["label"], round(o["score"], 2)) for o in r["objects"]], [("bus", 0.9), ("coco6", 0.8)])
        _, r = self.detect([hot(10, 20), hot(40, 40, score=0.8, branch=0)])  # far apart, same class
        self.assertEqual(r["count"], 2)

    def test_score_threshold_defaults_to_the_recipes_and_can_be_set(self):
        cells = [hot(10, 20), hot(30, 60, cls=6, score=0.3, branch=0), hot(50, 50, cls=7, score=0.2, branch=0)]
        self.assertEqual(self.detect(cells)[1]["count"], 2)  # the recipe's 0.25 keeps 0.9 and 0.3, drops 0.2
        self.assertEqual(self.detect(cells, score_threshold=0.1)[1]["count"], 3)
        _, r = self.detect(cells, score_threshold=0.95)
        self.assertEqual((r["count"], r["objects"]), (0, []))

    def test_the_candidate_and_detection_caps_bound_the_work(self):
        cells = [hot(10, 20), hot(30, 60, cls=6, score=0.8, branch=0)]
        self.mod.MAX_DETECTIONS = 1
        _, r = self.detect(cells)
        self.assertEqual([o["label"] for o in r["objects"]], ["bus"])
        self.mod.MAX_DETECTIONS = 100
        self.mod.MAX_CANDIDATES = 1  # only the strongest candidate ever reaches NMS
        _, r = self.detect(cells)
        self.assertEqual([o["label"] for o in r["objects"]], ["bus"])

    def test_an_unexpected_output_layout_is_a_structured_error(self):
        wrong = (lambda _: [np.zeros((1, 64, 80, 80), np.float32)],
                 lambda _: [np.zeros((1, 32, g, g), np.float32) for g in (80, 80, 80, 40, 40, 40, 20, 20, 20)])
        for fn in wrong:
            self.outputs("yolov8n", fn)
            code, r = self.post("object_detect", image_path=self.image("a.png", (640, 640)))
            self.assertEqual((code, r["error"]), (200, "internal"), r)
            self.assertIn("layout", r["detail"])

    def test_bad_arguments(self):
        img = self.image("a.png")
        for args in ({}, {"image_path": img, "score_threshold": 2}, {"image_path": img, "score_threshold": -0.1},
                     {"image_path": img, "score_threshold": "0.5"}, {"image_path": img, "score_threshold": True}):
            with self.subTest(args=args):
                code, r = self.post("object_detect", **args)
                self.assertEqual((code, r["error"]), (400, "bad_request"), r)


class EmbedTests(InProcess):
    def test_the_vector_comes_back_with_its_space_and_model(self):
        v = (np.arange(512, dtype=np.float32) / 512).reshape(1, 512)
        self.outputs("clip-vit-b32-image", lambda _: [v])
        code, r = self.post("embed", image_path=self.image("a.png"))
        self.assertEqual(code, 200, r)
        self.assertEqual(set(r), {"embedding", "dim", "space", "model"})
        self.assertEqual((r["dim"], len(r["embedding"])), (512, 512))
        self.assertAlmostEqual(r["embedding"][2], 2 / 512, places=6)
        self.assertEqual((r["space"], r["model"]), (self.spec["clip-vit-b32-image"]["space"], self.file["clip-vit-b32-image"]))

    def test_the_short_side_is_scaled_to_the_input_and_the_centre_cropped(self):
        self.outputs("clip-vit-b32-image", lambda _: [np.zeros((1, 512), np.float32)])
        # Three 224-wide bands: scaled to a 672x224 strip, the centre 224 columns are all green.
        path = self.bands("bands.png", [(255, 0, 0), (0, 255, 0), (0, 0, 255)], 224, 224)
        self.assertEqual(self.post("embed", image_path=path)[0], 200)
        x = self.Fake.instances[0].inputs[0]
        self.assertEqual((x.shape, x.dtype), ((1, 224, 224, 3), np.uint8))
        self.assertEqual({tuple(int(v) for v in x[0, 100, c]) for c in (0, 111, 223)}, {(0, 255, 0)})
        self.post("embed", image_path=self.image("small.png", (100, 50)))  # smaller than the input: scaled up
        self.assertEqual(self.Fake.instances[0].inputs[1].shape, (1, 224, 224, 3))


class ImageErrorTests(InProcess):
    def test_missing_and_unreadable_images_are_refused_before_a_model_is_touched(self):
        junk = os.path.join(self.tmp, "junk.jpg")
        write(junk, b"this is not an image")
        cut = os.path.join(self.tmp, "cut.jpg")
        noise = Image.frombytes("RGB", (300, 300), os.urandom(300 * 300 * 3))
        noise.save(cut, quality=95)
        with open(cut, "rb") as fh:
            data = fh.read()
        write(cut, data[: len(data) // 2])
        bomb = self.image("bomb.png", (20, 20))
        for tool in ("classify", "object_detect", "embed"):
            with self.subTest(tool=tool, case="missing"):
                code, r = self.post(tool, image_path=os.path.join(self.tmp, "nope.png"))
                self.assertEqual((code, r["error"]), (200, "image_missing"))
            with self.subTest(tool=tool, case="a directory"):
                self.assertEqual(self.post(tool, image_path=self.tmp)[1]["error"], "image_missing")
            with self.subTest(tool=tool, case="not an image"):
                code, r = self.post(tool, image_path=junk)
                self.assertEqual((code, r["error"]), (200, "image_unreadable"))
            with self.subTest(tool=tool, case="truncated"):
                self.assertEqual(self.post(tool, image_path=cut)[1]["error"], "image_unreadable")
        old = Image.MAX_IMAGE_PIXELS
        Image.MAX_IMAGE_PIXELS = 100  # 400 pixels is past twice the limit: Pillow refuses it as a decompression bomb
        self.addCleanup(setattr, Image, "MAX_IMAGE_PIXELS", old)
        self.assertEqual(self.post("classify", image_path=bomb)[1]["error"], "image_unreadable")
        self.assertEqual(self.Fake.instances, [])


class ResidencyTests(InProcess):
    def setUp(self):
        super().setUp()
        self.outputs("resnet18", lambda _: [np.zeros((1, 1000), np.float32)])
        self.outputs("clip-vit-b32-image", lambda _: [np.zeros((1, 512), np.float32)])
        self.outputs("yolov8n", lambda _: yolo_outputs([]))
        self.img = self.image("a.png")

    def test_a_model_is_built_once_and_stays_resident(self):
        self.post("classify", image_path=self.img)
        self.post("classify", image_path=self.img)
        self.assertEqual(len(self.Fake.instances), 1)
        self.assertEqual(self.health()["loaded"], ["resnet18"])

    def test_switching_models_releases_the_old_context_before_the_new_one_loads(self):
        self.post("classify", image_path=self.img)
        self.post("embed", image_path=self.img)
        self.assertEqual(self.Fake.events, [("load", self.file["resnet18"]), ("release", self.file["resnet18"]),
                                            ("load", self.file["clip-vit-b32-image"])])
        self.assertEqual(self.health()["loaded"], ["clip-vit-b32-image"])
        self.post("object_detect", image_path=self.img)
        self.assertTrue(self.Fake.instances[1].released)
        self.assertEqual(self.health()["loaded"], ["yolov8n"])

    def test_the_files_copy_in_python_memory_is_dropped_once_the_context_has_it(self):
        self.post("classify", image_path=self.img)
        self.assertIsNone(self.Fake.instances[0].rknn_data)

    def test_a_failing_release_is_logged_and_does_not_block_the_next_load(self):
        self.post("classify", image_path=self.img)
        self.Fake.release_error = RuntimeError("driver said no")
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            code, r = self.post("embed", image_path=self.img)
        self.assertEqual(code, 200, r)
        self.assertIn("release of resnet18 failed: driver said no", err.getvalue())
        self.assertEqual(self.health()["loaded"], ["clip-vit-b32-image"])

    def test_a_mismatched_model_is_refused_and_the_resident_one_is_left_alone(self):
        self.post("classify", image_path=self.img)
        write(os.path.join(self.models, self.file["yolov8n"]), b"tampered")
        code, r = self.post("object_detect", image_path=self.img)
        self.assertEqual((code, r["error"], r["model"], r["file"]), (200, "model_sha_mismatch", "yolov8n", self.file["yolov8n"]))
        self.assertEqual((r["expected"], r["got"]), (self.mod.MODELS["yolov8n"]["sha256"], sha(b"tampered")))
        self.assertEqual(len(self.Fake.instances), 1)
        self.assertFalse(self.Fake.instances[0].released)
        self.assertEqual(self.health()["loaded"], ["resnet18"])

    def test_an_absent_model_says_how_to_get_it(self):
        os.remove(os.path.join(self.models, self.file["yolov8n"]))
        os.remove(os.path.join(self.models, self.file["resnet18"]))
        _, r = self.post("object_detect", image_path=self.img)
        self.assertEqual((r["error"], r["file"]), ("model_missing", self.file["yolov8n"]))
        self.assertIn("--convert", r["hint"])  # no public download exists for it
        _, r = self.post("classify", image_path=self.img)
        self.assertEqual((r["error"], r["file"]), ("model_missing", self.file["resnet18"]))
        self.assertIn("fetch-models.sh", r["hint"])
        self.assertNotIn("--convert", r["hint"])

    def test_label_files_are_gated_like_models_and_before_the_npu_is_touched(self):
        write(os.path.join(self.models, "imagenet_synset.txt"), b"tampered\n")
        code, r = self.post("classify", image_path=self.img)
        self.assertEqual((code, r["error"], r["file"]), (200, "model_sha_mismatch", "imagenet_synset.txt"))
        os.remove(os.path.join(self.models, "coco_80_labels_list.txt"))
        _, r = self.post("object_detect", image_path=self.img)
        self.assertEqual((r["error"], r["file"]), ("model_missing", "coco_80_labels_list.txt"))
        self.assertEqual(self.Fake.instances, [])

    def test_a_missing_runtime_is_reported_and_leaves_the_resident_model_alone(self):
        self.post("classify", image_path=self.img)
        with mock.patch.dict(sys.modules, {"rknnlite": None, "rknnlite.api": None}):  # makes the import fail
            code, r = self.post("embed", image_path=self.img)
        self.assertEqual((code, r["error"]), (200, "runtime_missing"))
        self.assertIn("rknn-toolkit-lite2", r["detail"])
        self.assertFalse(self.Fake.instances[0].released)
        self.assertEqual(self.health()["loaded"], ["resnet18"])

    def test_the_runtimes_failures_come_back_as_return_codes_and_are_all_checked(self):
        self.Fake.load_ret = -1
        code, r = self.post("classify", image_path=self.img)
        self.assertEqual((code, r["error"], r["model"]), (200, "model_load_failed", "resnet18"))
        self.Fake.load_ret, self.Fake.init_ret = 0, -1
        _, r = self.post("classify", image_path=self.img)
        self.assertEqual(r["error"], "npu_unavailable")
        self.assertTrue(self.Fake.instances[-1].released)  # the half-built context goes back
        self.assertEqual(self.health()["loaded"], [])
        self.Fake.init_ret = 0
        self.outputs("resnet18", lambda _: None)
        _, r = self.post("classify", image_path=self.img)
        self.assertEqual(r["error"], "inference_failed")

    def test_an_exception_is_a_500_without_a_stack_trace_and_frees_the_lock(self):
        def boom(rt, inputs):
            raise RuntimeError("boom")
        self.Fake.on_inference = boom
        code, r = self.post("classify", image_path=self.img)
        self.assertEqual((code, r), (500, {"error": "internal", "detail": "RuntimeError: boom"}))
        self.Fake.on_inference = None
        self.assertEqual(self.post("classify", image_path=self.img)[0], 200)


class ConcurrencyTests(InProcess):
    def setUp(self):
        super().setUp()
        self.outputs("resnet18", lambda _: [np.zeros((1, 1000), np.float32)])
        self.img = self.image("a.png")

    def test_one_npu_call_runs_at_a_time(self):
        active = {"now": 0, "max": 0}
        guard = threading.Lock()

        def slow(rt, inputs):
            with guard:
                active["now"] += 1
                active["max"] = max(active["max"], active["now"])
            time.sleep(0.3)
            with guard:
                active["now"] -= 1

        self.Fake.on_inference = slow
        results = []
        threads = [threading.Thread(target=lambda: results.append(self.post("classify", image_path=self.img)))
                   for _ in range(3)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(30)
        self.assertEqual([code for code, _ in results], [200, 200, 200])
        self.assertEqual(active["max"], 1)

    def test_health_answers_while_a_call_is_in_flight(self):
        inside, release = threading.Event(), threading.Event()

        def block(rt, inputs):
            inside.set()
            release.wait(30)

        self.Fake.on_inference = block
        t = threading.Thread(target=lambda: self.post("classify", image_path=self.img))
        t.start()
        try:
            self.assertTrue(inside.wait(30))
            self.assertEqual(self.health()["loaded"], ["resnet18"])  # the first call is mid-inference
        finally:
            release.set()
            t.join(30)


class IdleClockTests(InProcess):
    def setUp(self):
        super().setUp()
        self.outputs("resnet18", lambda _: [np.zeros((1, 1000), np.float32)])
        self.img = self.image("a.png")

    def test_health_and_unknown_routes_do_not_count_as_activity_and_tool_calls_do(self):
        self.mod._last_call = -1.0
        call(self.port, "GET", "/health")
        call(self.port, "GET", "/nope")
        call(self.port, "POST", "/v1/nope", {})
        call(self.port, "POST", "/other", {})
        self.assertEqual(self.mod._last_call, -1.0)
        self.post("classify", image_path=self.img)
        self.assertNotEqual(self.mod._last_call, -1.0)
        self.mod._last_call = -1.0
        self.post("classify")  # a bad request is still a request for a tool
        self.assertNotEqual(self.mod._last_call, -1.0)


class Watchdog(InProcess):
    """The idle watchdog, run against a stand-in for the HTTP server (only shutdown() is used)."""

    class Stopper:
        def __init__(self):
            self.stopped = threading.Event()

        def shutdown(self):
            self.stopped.set()

    def setUp(self):
        super().setUp()
        quiet = mock.patch("sys.stderr", io.StringIO())  # the watchdog announces its exit there
        quiet.start()
        self.addCleanup(quiet.stop)
        self.addCleanup(self._end_watchdog)  # runs first: LIFO
        self.rewrite(RKNPU_IDLE_SEC="1")  # ticks every 0.25 s
        self.outputs("resnet18", lambda _: [np.zeros((1, 1000), np.float32)])
        self.img = self.image("a.png")
        self.server = self.Stopper()
        self.started = False

    def start(self):
        self.started = True
        threading.Thread(target=self.mod._idle_watchdog, args=(self.server,), daemon=True).start()

    def _end_watchdog(self):
        """Let a running watchdog finish, so none outlives its test and announces itself into the next one."""
        if self.started:
            self.mod._last_call = -1.0
            self.server.stopped.wait(10)

    def test_idle_releases_the_model_before_stopping_the_server(self):
        self.post("classify", image_path=self.img)
        self.mod._last_call = -1.0
        self.start()
        self.assertTrue(self.server.stopped.wait(10))
        self.assertTrue(self.Fake.instances[0].released)
        self.assertIsNone(self.mod._resident)

    def test_a_call_in_flight_is_never_idle(self):
        self.mod._last_call = -1.0
        with self.mod._lock:
            self.start()
            self.assertFalse(self.server.stopped.wait(1.0))
        self.assertTrue(self.server.stopped.wait(10))

    def test_a_slow_call_counts_idle_from_its_end_not_its_start(self):
        self.Fake.on_inference = lambda rt, inputs: time.sleep(1.6)  # longer than the 1 s idle limit
        self.start()
        self.assertEqual(self.post("classify", image_path=self.img)[0], 200)
        self.assertFalse(self.server.stopped.wait(0.6), "read as idle straight after a slow call")
        self.assertTrue(self.server.stopped.wait(10), "and still stops once it really is idle")

    def test_the_response_write_is_not_an_idle_window(self):
        """The lock is free once the tool returns; the clock must already be fresh by then."""
        self.Fake.on_inference = lambda rt, inputs: time.sleep(1.4)
        writing, go = threading.Event(), threading.Event()
        real_send = self.mod.Handler._send

        def held_send(handler, code, obj):
            if code == 200:
                writing.set()
                go.wait(30)
            return real_send(handler, code, obj)

        with mock.patch.object(self.mod.Handler, "_send", held_send):
            t = threading.Thread(target=lambda: self.post("classify", image_path=self.img))
            t.start()
            try:
                self.assertTrue(writing.wait(30))
                self.start()
                self.assertFalse(self.server.stopped.wait(0.8), "shut down with a response still being written")
            finally:
                go.set()
                t.join(30)


class HealthTests(InProcess):
    def write_sysfs(self, zones=(), load=None, driver=None):
        for i, (kind, temp) in enumerate(zones):
            zone = os.path.join(self.sysfs, "class", "thermal", f"thermal_zone{i}")
            write(os.path.join(zone, "type"), kind + "\n")
            write(os.path.join(zone, "temp"), temp + "\n")
        if load is not None:
            write(os.path.join(self.sysfs, "kernel", "debug", "rknpu", "load"), load + "\n")
        if driver is not None:
            write(os.path.join(self.sysfs, "module", "rknpu", "version"), driver + "\n")

    def test_temperature_load_and_driver_come_from_sysfs(self):
        self.write_sysfs(zones=[("package-thermal", "71153"), ("npu-thermal", "47500")],
                         load="NPU load:  Core0: 10%, Core1: 20%, Core2: 30%,", driver="0.9.8")
        h = self.health()
        self.assertEqual(h["temp_c"], 47.5)  # the npu zone, not the first one
        self.assertEqual(h["npu_load"], [10, 20, 30])
        self.assertEqual(h["runtime"]["rknpu_driver"], "0.9.8")

    def test_load_is_ordered_by_core(self):
        self.write_sysfs(load="NPU load:  Core1: 5%, Core0: 7%,")
        self.assertEqual(self.health()["npu_load"], [7, 5])

    def test_everything_degrades_to_null_where_a_node_is_absent_or_unreadable(self):
        h = self.health()
        self.assertEqual((h["temp_c"], h["npu_load"], h["runtime"]["rknpu_driver"]), (None, None, None))
        self.write_sysfs(zones=[("package-thermal", "71153"), ("gpu-thermal", "69307")], load="Permission denied")
        h = self.health()
        self.assertEqual((h["temp_c"], h["npu_load"]), (None, None))

    def test_runtime_versions(self):
        lib = os.path.join(self.tmp, "librknnrt.so")
        write(lib, b"\x7fELF\x00junk\x00librknnrt version: 2.3.2 (429f97ae6b@2025-04-09T09:09:27)\x00more")
        self.rewrite(RKNPU_RUNTIME_LIB=lib)
        with mock.patch("importlib.metadata.version", return_value="9.9.9") as v:
            r = self.health()["runtime"]
        v.assert_called_with("rknn-toolkit-lite2")
        self.assertEqual((r["rknn_toolkit_lite2"], r["librknnrt"], r["librknnrt_path"]),
                         ("9.9.9", "2.3.2 (429f97ae6b@2025-04-09T09:09:27)", lib))
        self.rewrite()
        with mock.patch("importlib.metadata.version", side_effect=importlib.metadata.PackageNotFoundError):
            self.assertIsNone(self.health()["runtime"]["rknn_toolkit_lite2"])

    def test_the_documented_fields_and_the_files_still_missing(self):
        self.rewrite(RKNPU_IDLE_SEC="300")
        os.remove(os.path.join(self.models, self.file["clip-vit-b32-image"]))
        os.remove(os.path.join(self.models, "coco_80_labels_list.txt"))
        h = self.health()
        self.assertEqual((h["enabled"], h["device"], h["idle_sec"], h["loaded"]), (True, "rknpu", 300, []))
        self.assertEqual(h["models_missing"], sorted([self.file["clip-vit-b32-image"], "coco_80_labels_list.txt"]))
        self.assertIsInstance(h["uptime_sec"], int)
        want = sorted(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None
        self.assertEqual(h["cpu_affinity"], want)


class HandlerTests(InProcess):
    def test_malformed_bodies_and_lengths_are_400(self):
        for raw in (b"{", b"[]", b"7", b"\xff"):
            with self.subTest(raw=raw):
                code, r = call(self.port, "POST", "/v1/classify", raw=raw)
                self.assertEqual((code, r["error"]), (400, "bad_request"))
        code, r = call(self.port, "POST", "/v1/classify", raw=b"")
        self.assertEqual((code, r["detail"]), (400, "image_path required"))  # an empty body reads as {}
        for length in ("-1", "-5", "many"):  # -1 is the trap: read(-1) would wait for an EOF that never comes
            with self.subTest(length=length):
                conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
                try:
                    conn.putrequest("POST", "/v1/classify")
                    conn.putheader("Content-Length", length)
                    conn.endheaders()
                    resp = conn.getresponse()
                    self.assertEqual((resp.status, json.loads(resp.read())["error"]), (400, "bad_request"))
                finally:
                    conn.close()


class RuntimeLibShimTests(InProcess):
    """RKNN-Toolkit-Lite2 2.3.2 probes /usr/lib/librknnrt.so and ignores LD_LIBRARY_PATH (measured on the board:
    init_runtime returns -1, "Can not find dynamic library"), so RKNPU_RUNTIME_LIB redirects it, briefly."""

    PROBED = ("/usr/lib/librknn_runtime.so", "/usr/lib64/librknn_runtime.so", "/usr/lib/librknnrt.so")

    def setUp(self):
        super().setUp()
        self.lib = os.path.join(self.tmp, "private", "librknnrt.so")
        write(self.lib, b"a library")
        self.rr = types.ModuleType("rknnlite.api.rknn_runtime")
        loaded = []

        class CDLL:
            def __init__(self, name, *a, **k):
                loaded.append(name)

        self.rr.CDLL, self.loaded, self.original_cdll = CDLL, loaded, CDLL
        sys.modules["rknnlite.api"].rknn_runtime = self.rr  # a private fake api module: nothing to undo
        patcher = mock.patch.dict(sys.modules, {"rknnlite.api.rknn_runtime": self.rr})
        patcher.start()
        self.addCleanup(patcher.stop)
        rr, probed = self.rr, self.PROBED

        def init_runtime(fake, core_mask=None):  # the wheel: probe three paths, then CDLL the one it found
            found = next((p for p in probed if os.path.exists(p)), None)
            if found is None:
                return -1
            rr.CDLL(found)
            return 0

        self.Fake.init_runtime = init_runtime
        self.outputs("resnet18", lambda _: [np.zeros((1, 1000), np.float32)])

    def test_init_runtime_loads_the_private_library_and_the_probe_is_put_back(self):
        self.rewrite(RKNPU_RUNTIME_LIB=self.lib)
        real_exists = os.path.exists
        code, r = self.post("classify", image_path=self.image("a.png"))
        self.assertEqual(code, 200, r)
        self.assertEqual(self.loaded, [self.lib])
        self.assertIs(os.path.exists, real_exists)
        self.assertIs(self.rr.CDLL, self.original_cdll)

    def test_the_patch_is_scoped_to_one_path_and_undone_when_init_raises(self):
        self.rewrite(RKNPU_RUNTIME_LIB=self.lib)
        real_exists = os.path.exists
        with self.mod._runtime_lib_shim():
            self.assertTrue(os.path.exists(self.PROBED[-1]))
            self.assertEqual(os.path.exists(os.path.join(self.tmp, "nothing")), False)
            self.assertEqual(os.path.exists(self.lib), True)
            self.rr.CDLL("libc.so.6")
            self.rr.CDLL(self.PROBED[0])
        self.assertEqual(self.loaded, ["libc.so.6", self.lib])  # only the wheel's own paths are redirected
        with self.assertRaises(RuntimeError), self.mod._runtime_lib_shim():
            raise RuntimeError("init_runtime blew up")
        self.assertIs(os.path.exists, real_exists)
        self.assertIs(self.rr.CDLL, self.original_cdll)

    def test_without_a_configured_library_nothing_is_patched(self):
        real_exists = os.path.exists
        for lib in ("", os.path.join(self.tmp, "absent", "librknnrt.so")):
            with self.subTest(lib=lib):
                self.rewrite(RKNPU_RUNTIME_LIB=lib)
                with self.mod._runtime_lib_shim():
                    self.assertIs(os.path.exists, real_exists)
                    self.assertIs(self.rr.CDLL, self.original_cdll)


# ---------------------------------------------------------------- the manifest

class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.man = real_manifest()

    def pins(self):
        m = self.man
        out = {f"runtime.{k}": v["sha256"] for k, v in m["runtime"].items() if isinstance(v, dict)}
        for section in ("models", "labels", "testdata"):
            out.update({f"{section}.{k}": v["sha256"] for k, v in m[section].items()})
        out.update({f"models.{k}.convert.onnx": v["convert"]["onnx"]["sha256"] for k, v in m["models"].items() if "convert" in v})
        return out

    def test_every_artifact_is_pinned_by_a_real_sha256(self):
        for name, pin in self.pins().items():
            with self.subTest(artifact=name):
                self.assertRegex(pin, r"^[0-9a-f]{64}$")

    def test_downloads_come_from_a_release_tag_never_a_moving_branch(self):
        urls = [v["url"] for k, v in self.man["runtime"].items() if isinstance(v, dict)]
        urls += [v["url"] for section in ("models", "labels", "testdata") for v in self.man[section].values() if "url" in v]
        urls += [d[k] for d in self.man["datasets"].values() for k in ("list", "base")]
        for url in urls:
            with self.subTest(url=url):
                self.assertTrue(url.startswith("https://"))
                if "raw.githubusercontent.com" in url:
                    self.assertIn(f"/v{self.man['runtime']['rknn_toolkit_lite2']}/", url)

    def test_every_model_is_downloadable_or_convertible_and_its_references_resolve(self):
        for key, spec in self.man["models"].items():
            with self.subTest(model=key):
                self.assertEqual("url" in spec, "convert" not in spec)
                for field in ("file", "sha256", "task", "input"):
                    self.assertIn(field, spec)
                if "labels" in spec:
                    self.assertIn(spec["labels"], self.man["labels"])
                if "convert" in spec:
                    if spec["convert"].get("dataset"):
                        self.assertIn(spec["convert"]["dataset"], self.man["datasets"])
                    self.assertIn("url", spec["convert"]["onnx"])

    def test_the_wheel_and_the_runtime_library_agree_with_the_toolkit_version(self):
        version = self.man["runtime"]["rknn_toolkit_lite2"]
        self.assertIn(f"rknn_toolkit_lite2-{version}-", self.man["runtime"]["wheel"]["file"])
        self.assertIn(f"/v{version}/", self.man["runtime"]["librknnrt"]["url"])

    def test_the_tools_use_models_the_manifest_has(self):
        mod = load_server(RKNPU_ENABLED="0")
        for key in (*mod.CLASSIFY_DOMAINS.values(), mod.DETECT_KEY, mod.EMBED_KEY):
            with self.subTest(model=key):
                self.assertIn(key, mod.MODELS)
        self.assertEqual({mod.MODELS[k]["task"] for k in mod.CLASSIFY_DOMAINS.values()}, {"classify"})
        self.assertEqual(mod.MODELS[mod.DETECT_KEY]["task"], "object_detect")
        self.assertEqual(mod.MODELS[mod.EMBED_KEY]["task"], "embed")
        self.assertTrue(mod.MODELS[mod.EMBED_KEY]["space"])


# ---------------------------------------------------------------- the shell scripts

class LauncherTests(unittest.TestCase):
    """rknpu-http.sh against a stub python and stub taskset / nice that record how they were called.

    The launcher broke twice in the Coral lane's first live gate (the harness passes "--idle-sec <n>", and the
    home was resolved one level up); both shapes are pinned here too."""

    def setUp(self):
        self.bash = bash_ok()
        if self.bash is None:
            self.skipTest("no usable bash on this box")
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.home = d.name
        self.lane = os.path.join(self.home, "accelerators", "rknpu")
        os.makedirs(self.lane)
        for name in ("rknpu-http.sh", "server.py", "models.json"):
            shutil.copy(os.path.join(HERE, name), self.lane)
        os.makedirs(os.path.join(self.home, "venv", "bin"))
        os.makedirs(os.path.join(self.home, "models"))
        write(os.path.join(self.home, "lib", "librknnrt.so"), b"a library")
        self.record = os.path.join(self.home, "record.json")
        self.wrapped = os.path.join(self.home, "wrapped.log")
        self.stub(os.path.join(self.home, "venv", "bin", "python"),
                  "printf '{\"argv\":\"%s\",\"idle\":\"%s\",\"home\":\"%s\",\"models\":\"%s\",\"manifest\":\"%s\",\"port\":\"%s\","
                  "\"bind\":\"%s\",\"lib\":\"%s\",\"ld\":\"%s\",\"omp\":\"%s\"}' \"$*\" \"$RKNPU_IDLE_SEC\" \"$RKNPU_HOME\" "
                  "\"$RKNPU_MODELS_DIR\" \"$RKNPU_MANIFEST\" \"$RKNPU_PORT\" \"$RKNPU_BIND\" \"$RKNPU_RUNTIME_LIB\" "
                  "\"$LD_LIBRARY_PATH\" \"$OMP_NUM_THREADS\" > " + self.q(self.record))
        self.bin = os.path.join(self.home, "bin")
        for tool in ("taskset", "nice"):  # both take two option words, then the command to run
            self.stub(os.path.join(self.bin, tool), f"printf '{tool} %s\\n' \"$*\" >> {self.q(self.wrapped)}; shift 2; exec \"$@\"")

    @staticmethod
    def q(path: str) -> str:
        return '"' + path.replace("\\", "/") + '"'

    @staticmethod
    def stub(path: str, body: str):
        write(path, "#!/usr/bin/env bash\n" + body + "\n")
        os.chmod(path, 0o755)

    def run_launcher(self, *args, script=None, **env):
        e = clean_env(RKNPU_LOG_FILE=os.path.join(self.home, "sidecar.log"), PATH=self.bin + os.pathsep + os.environ.get("PATH", ""))
        e.update(env)
        for f in (self.record, self.wrapped):
            if os.path.exists(f):
                os.remove(f)
        r = subprocess.run([self.bash, script or os.path.join(self.lane, "rknpu-http.sh"), *args], env=e,
                           capture_output=True, text=True, timeout=60)
        got = {}
        for _ in range(50):  # a setsid that forks would let the launcher return before its child wrote the record
            if os.path.exists(self.record) or r.returncode != 0:
                break
            time.sleep(0.1)
        with contextlib.suppress(OSError, ValueError), open(self.record, encoding="utf-8") as fh:
            got = json.load(fh)
        return r, got

    def test_the_harness_call_shape_and_the_nested_layout(self):
        r, got = self.run_launcher("--idle-sec", "7")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(got["idle"], "7")
        self.assertTrue(same_path(got["home"], self.home), got)  # found by walking up to venv/
        self.assertTrue(same_path(got["models"], os.path.join(self.home, "models")), got)
        self.assertTrue(same_path(got["manifest"], os.path.join(self.lane, "models.json")), got)
        self.assertTrue(got["argv"].replace("\\", "/").endswith("/accelerators/rknpu/server.py"), got)
        self.assertEqual((got["port"], got["bind"]), ("18815", "127.0.0.1"))

    def test_the_other_ways_to_say_the_idle_seconds(self):
        for args, env, want in ((("--idle-sec=9",), {}, "9"), (("11",), {}, "11"), ((), {"RKNPU_IDLE_SEC": "13"}, "13"),
                                ((), {}, "300"), (("--idle-sec", "5", "stray", "-x"), {}, "5")):
            with self.subTest(args=args, env=env):
                r, got = self.run_launcher(*args, **env)
                self.assertEqual((r.returncode, got.get("idle")), (0, want), r.stderr)

    def test_a_non_integer_idle_is_refused_with_exit_2(self):
        r, got = self.run_launcher("--idle-sec", "abc")
        self.assertEqual((r.returncode, got), (2, {}))
        self.assertIn("idle seconds must be an integer, got 'abc'", r.stderr)  # the offending value, not the flag

    def test_the_flat_layout_resolves_its_home_too(self):
        flat = os.path.join(self.home, "flat")  # the profiles.json seed: __RKNPU_HOME__/rknpu-http.sh, beside venv/
        os.makedirs(os.path.join(flat, "venv", "bin"))
        shutil.copy(os.path.join(self.home, "venv", "bin", "python"), os.path.join(flat, "venv", "bin", "python"))
        for name in ("rknpu-http.sh", "server.py", "models.json"):
            shutil.copy(os.path.join(HERE, name), flat)
        r, got = self.run_launcher("--idle-sec", "3", script=os.path.join(flat, "rknpu-http.sh"))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(same_path(got["home"], flat), got)
        self.assertTrue(same_path(got["manifest"], os.path.join(flat, "models.json")), got)

    def test_the_process_is_pinned_to_the_a55_cores_at_nice_10_by_default(self):
        r, _ = self.run_launcher("--idle-sec", "7")
        self.assertEqual(r.returncode, 0, r.stderr)
        with open(self.wrapped, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
        self.assertTrue(lines[0].startswith("taskset -c 0-3 nice -n 10 "), lines)  # taskset outermost, then nice, then python
        self.assertTrue(lines[1].startswith("nice -n 10 "), lines)

    def test_the_cpu_list_is_configurable_and_validated(self):
        r, _ = self.run_launcher("--idle-sec", "7", RKNPU_CPUS="0,2")
        self.assertEqual(r.returncode, 0, r.stderr)
        with open(self.wrapped, encoding="utf-8") as fh:
            self.assertTrue(fh.readline().startswith("taskset -c 0,2 "))
        r, got = self.run_launcher("--idle-sec", "7", RKNPU_CPUS="abc")
        self.assertEqual((r.returncode, got), (2, {}))
        self.assertIn("RKNPU_CPUS", r.stderr)

    def test_the_private_runtime_library_and_single_threaded_numpy(self):
        _, got = self.run_launcher("--idle-sec", "7")
        self.assertTrue(same_path(got["lib"], os.path.join(self.home, "lib", "librknnrt.so")), got)
        self.assertEqual(got["omp"], "1")
        self.assertTrue(same_path(got["ld"].split(":")[0], os.path.join(self.home, "lib")), got)
        _, got = self.run_launcher("--idle-sec", "7", LD_LIBRARY_PATH="/opt/other")
        self.assertTrue(got["ld"].endswith(":/opt/other"), got)
        _, got = self.run_launcher("--idle-sec", "7", RKNPU_RUNTIME_LIB="/elsewhere/librknnrt.so")
        self.assertEqual(got["lib"], "/elsewhere/librknnrt.so")  # a caller's choice is kept
        os.remove(os.path.join(self.home, "lib", "librknnrt.so"))
        _, got = self.run_launcher("--idle-sec", "7")
        self.assertEqual(got["lib"], "")  # no library, nothing exported: the sidecar then reports it itself

    def test_no_python_is_exit_2_with_the_home_it_looked_in(self):
        os.remove(os.path.join(self.home, "venv", "bin", "python"))
        r, got = self.run_launcher("--idle-sec", "7")
        self.assertEqual((r.returncode, got), (2, {}))
        self.assertIn("no python at", r.stderr)
        self.assertIn("RKNPU_HOME=", r.stderr)


class FetchModelsTests(unittest.TestCase):
    """fetch-models.sh against file:// URLs: what it downloads, where it puts it, and what it refuses."""

    def setUp(self):
        self.bash = bash_ok("curl", "sha256sum", "python3")
        if self.bash is None:
            self.skipTest("no bash with curl, sha256sum and python3 on this box")
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.root = d.name.replace("\\", "/")
        self.home = self.root + "/home"
        os.makedirs(self.home + "/venv")
        self.src = {"librknnrt.so": b"a runtime", "tk.whl": b"a wheel", "net.rknn": b"a model",
                    "labels.txt": b"one\ntwo\n", "pic.jpg": b"a picture"}
        for name, data in self.src.items():
            write(f"{self.root}/src/{name}", data)
        self.manifest = self.write_manifest()

    def entry(self, name, **extra):
        return {"file": name, "url": pathlib.Path(f"{self.root}/src/{name}").as_uri(), "sha256": sha(self.src[name]), **extra}

    def write_manifest(self, **overrides):
        man = {
            "runtime": {"rknn_toolkit_lite2": "2.3.2", "target": "rk3588", "wheel": self.entry("tk.whl"),
                        "librknnrt": self.entry("librknnrt.so")},
            "models": {"net": self.entry("net.rknn"),
                       "big": {"file": "big.rknn", "sha256": sha(b"a converted model"), "convert": {}}},
            "labels": {"labels.txt": {k: v for k, v in self.entry("labels.txt").items() if k != "file"}},
            "testdata": {"pic.jpg": {k: v for k, v in self.entry("pic.jpg").items() if k != "file"}},
        }
        man.update(overrides)
        path = f"{self.root}/models.json"
        write(path, json.dumps(man))
        return path

    def run_fetch(self, *args, script=None, **env):
        e = clean_env(RKNPU_HOME=self.home, RKNPU_MANIFEST=self.manifest)
        e.update(env)
        return subprocess.run([self.bash, script or os.path.join(HERE, "fetch-models.sh"), *args], env=e,
                              capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=120)

    def test_downloads_verify_and_land_in_the_right_directories_and_a_convertible_model_is_missing(self):
        r = self.run_fetch()
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)  # the converted model has no download
        for name, where in (("librknnrt.so", "lib"), ("tk.whl", "wheels"), ("net.rknn", "models"), ("labels.txt", "models"),
                            ("pic.jpg", "models")):
            with self.subTest(file=name):
                self.assertIn(f"ok       {name}", r.stdout)
                self.assertTrue(os.path.isfile(f"{self.home}/{where}/{name}"))
        self.assertRegex(r.stdout, r"MISSING  big\.rknn .*fetch-models\.sh --convert")
        self.assertFalse(os.path.exists(f"{self.home}/models/big.rknn"))
        again = self.run_fetch()
        self.assertNotIn("fetching", again.stdout)  # what verifies is not fetched twice

    def test_only_the_convertible_model_missing_is_the_only_reason_for_exit_1(self):
        write(f"{self.home}/models/big.rknn", b"a converted model")
        self.assertEqual(self.run_fetch().returncode, 0)

    def test_a_download_that_does_not_match_its_pin_is_removed_and_reported(self):
        man = read_json(self.manifest)
        man["models"]["net"]["sha256"] = "0" * 64
        write(self.manifest, json.dumps(man))
        r = self.run_fetch()
        self.assertEqual(r.returncode, 1)
        self.assertRegex(r.stdout, r"FAILED   net\.rknn \(sha256 [0-9a-f]{64} != 0{64}\)")
        self.assertEqual([f for f in os.listdir(f"{self.home}/models") if f.startswith("net")], [])  # no file, no .part

    def test_a_file_on_disk_that_drifted_from_its_pin_is_replaced(self):
        write(f"{self.home}/models/net.rknn", b"drifted")
        self.run_fetch()
        with open(f"{self.home}/models/net.rknn", "rb") as fh:
            self.assertEqual(fh.read(), self.src["net.rknn"])

    def test_a_manifest_that_does_not_parse_stops_the_script_before_any_download(self):
        write(self.manifest, "{ not json")
        r = self.run_fetch()
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(os.path.exists(f"{self.home}/models/net.rknn"))
        self.assertNotIn("fetching", r.stdout)

    def test_convert_without_the_toolkit_stops_before_downloading_anything(self):
        cache = f"{self.root}/cache"
        r = self.run_fetch("--convert", RKNPU_CONVERT_PYTHON=sys.executable, RKNPU_CONVERT_CACHE=cache)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("rknn", r.stderr)
        self.assertEqual(os.listdir(cache), [])

    def test_an_unknown_argument_is_a_usage_error(self):
        r = self.run_fetch("--bogus")
        self.assertEqual(r.returncode, 2)
        self.assertIn("usage", r.stderr)

    def test_no_home_and_no_models_dir_is_a_usage_error(self):
        lonely = f"{self.root}/a/b/c"  # three levels of directories, none of them holding a venv/
        os.makedirs(lonely)
        shutil.copy(os.path.join(HERE, "fetch-models.sh"), lonely)
        r = self.run_fetch(RKNPU_HOME="", script=f"{lonely}/fetch-models.sh")
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("no RKNPU_HOME", r.stderr)


if __name__ == "__main__":
    unittest.main()
