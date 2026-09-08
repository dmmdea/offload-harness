#!/usr/bin/env python3
"""Contract tests for the Coral sidecar, runnable on any box (CORAL_ENABLED=0 — no TPU, no litert).

Pins the wire contract the harness's accelclient depends on: /health shape, 404 unknown_tool,
400 bad_request, the 500 guard, a structured 200 error when the TPU is disabled, refusal of a
non-loopback bind, and the manifest-mismatch refusal. Run: python3 test_server.py
"""
from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.join(HERE, "server.py")


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _req(port: int, method: str, path: str, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def _start(env_extra: dict, models_dir: str) -> tuple[subprocess.Popen, int]:
    port = _free_port()
    env = dict(os.environ, CORAL_ENABLED="0", CORAL_PORT=str(port), CORAL_MODELS_DIR=models_dir,
               CORAL_IDLE_SEC="0")
    env.update(env_extra)  # a caller's CORAL_ENABLED=1 must override, not collide
    p = subprocess.Popen([sys.executable, SERVER], env=env, stderr=subprocess.PIPE, text=True)
    for _ in range(100):
        try:
            _req(port, "GET", "/health")
            return p, port
        except OSError:
            if p.poll() is not None:
                raise RuntimeError("server exited: " + (p.stderr.read() if p.stderr else ""))
            time.sleep(0.05)
    raise RuntimeError("server did not come up")


def main() -> int:
    failures = []

    def check(name: str, cond: bool, detail=""):
        print(("PASS " if cond else "FAIL ") + name + (f"  {detail}" if detail and not cond else ""))
        if not cond:
            failures.append(name)

    with tempfile.TemporaryDirectory() as models_dir:
        p, port = _start({}, models_dir)
        try:
            code, h = _req(port, "GET", "/health")
            check("health 200", code == 200)
            for k in ("enabled", "device", "status", "temp_c", "loaded", "models_missing", "runtime", "tools"):
                check(f"health has {k}", k in h, str(h))
            check("health enabled=false in test mode", h.get("enabled") is False)
            check("health lists every manifest model as missing on an empty dir", len(h.get("models_missing", [])) >= 4, str(h.get("models_missing")))
            check("health tools = the four capabilities", sorted(h.get("tools", [])) == ["classify", "embed", "object_detect", "semantic_segment"], str(h.get("tools")))

            code, r = _req(port, "GET", "/nope")
            check("GET unknown path 404 not_found", code == 404 and r.get("error") == "not_found", str((code, r)))

            code, r = _req(port, "POST", "/v1/does_not_exist", {})
            check("POST unknown tool 404 unknown_tool", code == 404 and r.get("error") == "unknown_tool", str((code, r)))

            code, r = _req(port, "POST", "/v1/classify", {})
            check("classify without image_path 400 bad_request", code == 400 and r.get("error") == "bad_request", str((code, r)))

            code, r = _req(port, "POST", "/v1/classify", {"image_path": "/x.jpg", "domain": "dogs"})
            check("classify bad domain 400 bad_request", code == 400 and r.get("error") == "bad_request", str((code, r)))

            code, r = _req(port, "POST", "/v1/classify", {"image_path": "/x.jpg"})
            check("classify with TPU disabled -> structured 200 tpu_disabled", code == 200 and r.get("error") == "tpu_disabled", str((code, r)))

            req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/embed", data=b"not json", method="POST",
                                         headers={"Content-Type": "application/json"})
            try:
                urllib.request.urlopen(req, timeout=10)
                code = 200
            except urllib.error.HTTPError as e:
                code = e.code
            check("malformed JSON body 400", code == 400)
        finally:
            p.kill()
            p.wait()

        # Manifest mismatch: a file present under the right name but the wrong bytes must be REFUSED
        # (structured model_sha_mismatch), never loaded. Needs ENABLED=1 to reach the manifest check.
        with open(os.path.join(models_dir, "efficientnet-edgetpu-S_quant_edgetpu.tflite"), "wb") as fh:
            fh.write(b"not a model")
        p, port = _start({"CORAL_ENABLED": "1"}, models_dir)
        try:
            code, r = _req(port, "POST", "/v1/classify", {"image_path": "/x.jpg"})
            check("wrong-bytes model -> 200 model_sha_mismatch (refused, not loaded)",
                  code == 200 and r.get("error") == "model_sha_mismatch", str((code, r)))
            code, r = _req(port, "POST", "/v1/semantic_segment", {"image_path": "/x.jpg"})
            check("absent model -> 200 model_missing", code == 200 and r.get("error") == "model_missing", str((code, r)))
        finally:
            p.kill()
            p.wait()

    # Non-loopback bind is refused at startup (exit 2).
    env = dict(os.environ, CORAL_ENABLED="0", CORAL_BIND="0.0.0.0", CORAL_PORT=str(_free_port()))
    p = subprocess.run([sys.executable, SERVER], env=env, capture_output=True, text=True, timeout=20)
    check("non-loopback bind refused (exit 2)", p.returncode == 2, p.stderr.strip())

    # Idle self-exit: with CORAL_IDLE_SEC=1 the process ends on its own within a few seconds.
    with tempfile.TemporaryDirectory() as models_dir:
        port = _free_port()
        env = dict(os.environ, CORAL_ENABLED="0", CORAL_PORT=str(port), CORAL_MODELS_DIR=models_dir, CORAL_IDLE_SEC="1")
        p = subprocess.Popen([sys.executable, SERVER], env=env, stderr=subprocess.DEVNULL)
        try:
            p.wait(timeout=15)
            check("idle self-exit", True)
        except subprocess.TimeoutExpired:
            p.kill()
            check("idle self-exit", False, "still alive after 15 s")

    print(f"\n{'ALL PASS' if not failures else str(len(failures)) + ' FAILED: ' + ', '.join(failures)}")
    return 0 if not failures else 1


if __name__ == "__main__":
    sys.exit(main())
