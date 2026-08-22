# Hailo-8L Accelerator Tier Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the offload-harness a `hailo-8l` *accelerator* that coexists with a box's GPU tier, so the OptiPlex 7060 runs `blackwell-8` + `hailo-8l` and Claude's `offload_*` vision calls for face identity, object detection, re-id, depth, low-light and image embeddings run on the NPU through a harness-spawned HTTP sidecar.

**Architecture:** An accelerator is ADDITIVE to the GPU tier, never a replacement: `profile` stays one string everywhere and a new `accelerators: []` list rides beside it (Verdict → installed.json → config.json). `profiles.json` gains a top-level `accelerators` map (separate from `profiles`, so every test that enumerates profiles is untouched). The Hailo repo's runtime gets a tiny stdlib HTTP wrapper (`server/http_server.py`, self-exits on idle); the harness gets `internal/hailoclient` (a `nimclient`-shaped client + an on-demand `Sidecar` manager) and seven gated `offload_*` tools mirroring `handleNIM` (direct handlers, not the pipeline — no cache/ledger coupling in v1). OCR keeps its GPU path and gains `engine:"npu"`.

**Tech Stack:** Go 1.26 (`github.com/dmmdea/offload-harness`, `modelcontextprotocol/go-sdk`), PowerShell 5.1/7 (installer, dot-source test seam), Python 3.11 stdlib `http.server` (sidecar), openpyxl (tier matrix).

**Spec:** Decisions locked by Daniel 2026-08-22, recorded in `~/.claude/plans/optiplex7060-editor-rig-plan.md` row C3 and mem0 `76c64d48`. Recon (file:line map of the tier system) in the same row.

## Global Constraints

- **Tier matrix is ground truth and is updated FIRST** (mem0 canonical 08d1d383 / 3e663f75): `profiles.json` + regenerated xlsx land in Task 2 before any Go code.
- **Schema = option A**: `profile` remains a single string; `accelerators` is an additive `[]string`. A box with no NPU has an empty list — byte-identical behaviour to today. Never a composite id, never a second scalar.
- **Sidecar, not always-on**: no scheduled task, no service. The sidecar self-exits after idle; the harness spawns it on demand.
- **Ownership**: NPU exclusive = face detect/identity, object detect, person re-id, depth, low-light, image embeddings. GPU exclusive = VQA/description. OCR = GPU primary, NPU via explicit `engine:"npu"`.
- **Tailnet/loopback only**: the sidecar binds `127.0.0.1:18813` (port discipline: 18811 fleet, 18812 relay → 18813; recorded in the port file in Task 1).
- **Version bump ritual** (memory): `VERSION` + `main.go` const + `.printing-press.json` + CHANGELOG in one commit; run `go test -count=1 ./...` AFTER the bump, unpiped.
- **`tools/list` byte-identical when the accelerator is absent** — same pin as `agent_delegate` (delta 13).
- Public repos, `dmmdea` account — re-check `gh api user --jq .login` in its own call before every `gh` write.
- Work in a worktree per repo (`D:\dev\worktrees\…`); never on `main`.

---

## File Structure

**Hailo repo** (`D:\Dev\Hailo-8L-Analysis-Pipelines`, Task 1)
- Create `server/http_server.py` — stdlib HTTP wrapper over the existing tool functions; `/health`, `/v1/<tool>`; idle self-exit.
- Create `server/test_http_server.py` — routing/JSON/404/400/health tests with `HAILO_VISION_ENABLED=0` (runs anywhere).
- Create `hailo-http.cmd` — launcher pinning the venv interpreter + `PYTHONPATH`.
- Modify `README.md` — "HTTP sidecar" section.

**Harness** (`D:\Dev\dmmdea\local-offload-public`, Tasks 2–9)
- Modify `setup/templates/profiles.json` — top-level `accelerators` map.
- Modify `G:\My Drive\AI Ecosystem\Ecosystem\Arquitechture\generate-tier-matrix.py` — "Accelerators" sheet.
- Modify `internal/config/config.go` (+ `config_test.go`) — `Accelerators`, `Hailo*` fields, defaults, `HasAccelerator`.
- Modify `internal/hwdetect/classify.go` (+ `classify_test.go`) — `Verdict.Accelerators`, `AcceleratorsFromHailortcli`.
- Modify `setup/detect.ps1` (+ `detect.tests.ps1`) — NPU probe, verdict field.
- Modify `internal/tierseed/tierseed.go` (+ `tierseed_test.go`) — `ResolveAccelerators`.
- Modify `setup/install.ps1` (+ `setup/tests/install-config-seed.test.ps1`) — verdict passthrough, manifest field, accelerator seed merge.
- Modify `internal/fleetnode/gpuinfo.go` (+ `gpuinfo_test.go`) — `InstalledInfo.Accelerators`.
- Create `internal/hailoclient/hailoclient.go`, `sidecar.go` (+ tests) — client + on-demand sidecar.
- Modify `internal/mcpserver/mcpserver.go` (+ `hailo_test.go`) — gated tools, status block, OCR engine param.
- Create `docs/systems/accelerators.md`, `docs/architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md`; modify `AGENTS.md`, `README.md`, `setup/SETUP-AGENT.md`, `CHANGELOG.md`, `VERSION`, `main.go`, `.printing-press.json`.
- Copy this plan to `docs/superpowers/plans/2026-08-22-hailo-8l-accelerator-tier.md`.

---

### Task 1: Hailo HTTP sidecar (the wire contract)

**Files:**
- Create: `server/http_server.py`
- Create: `server/test_http_server.py`
- Create: `hailo-http.cmd`
- Modify: `README.md` (append section after "## Using it from Claude" equivalent — the "## Installation" section ends the file; append at end)
- Modify: `P:\Port Directory\optiplex7060-ports.md` (create if absent)

**Interfaces:**
- Produces (consumed by Task 7): `GET /health` → the `hailo_status()` dict, always HTTP 200. `POST /v1/{face_detect|face_embed|object_detect|person_embed|depth|enhance_low_light|ocr|embed}` with a JSON object body = that tool's keyword arguments → HTTP 200 with the tool's dict (structured `{"error":true,"kind":…}` dicts ALSO return 200 — they are results, mirroring MCP); unknown tool → 404 `{"error":true,"kind":"unknown_tool"}`; non-JSON/non-object body → 400 `{"error":true,"kind":"bad_request"}`. Default bind `127.0.0.1:18813`. Env `HAILO_SIDECAR_IDLE_SEC` (default 300): the process exits 0 after that many seconds with no request.

- [ ] **Step 1: Land a worktree in the Hailo repo**

```bash
git -C "D:/Dev/Hailo-8L-Analysis-Pipelines" worktree add "D:/dev/worktrees/hailo-http" -b feat/http-sidecar
cd "D:/dev/worktrees/hailo-http" && git rev-parse --abbrev-ref HEAD && pwd
```
Expected: `feat/http-sidecar` and the worktree path.

- [ ] **Step 2: Write the failing test**

Create `server/test_http_server.py`:

```python
"""HTTP sidecar contract tests. Run with HAILO_VISION_ENABLED=0 so no device is
touched: every tool then returns its structured disabled dict, which is exactly
the routing/JSON behaviour this layer owns."""
import json
import os
import sys
import threading
import unittest
import urllib.error
import urllib.request

os.environ["HAILO_VISION_ENABLED"] = "0"
sys.path.insert(0, os.path.dirname(__file__))
import http_server  # noqa: E402


class SidecarContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = http_server.make_server("127.0.0.1", 0, idle_sec=0)
        cls.port = cls.srv.server_address[1]
        cls.t = threading.Thread(target=cls.srv.serve_forever, daemon=True)
        cls.t.start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def _get(self, path):
        with urllib.request.urlopen(f"http://127.0.0.1:{self.port}{path}") as r:
            return r.status, json.loads(r.read())

    def _post(self, path, body):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=data,
                                     headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_health_reports_status_dict(self):
        status, body = self._get("/health")
        self.assertEqual(status, 200)
        self.assertIn("enabled", body)
        self.assertFalse(body["enabled"])

    def test_tool_routes_to_structured_result(self):
        status, body = self._post("/v1/face_detect", {"image_path": "nope.jpg"})
        self.assertEqual(status, 200)
        self.assertTrue(body.get("error"))
        self.assertEqual(body.get("kind"), "hailo_disabled")

    def test_unknown_tool_is_404(self):
        status, body = self._post("/v1/teleport", {})
        self.assertEqual(status, 404)
        self.assertEqual(body.get("kind"), "unknown_tool")

    def test_bad_json_is_400(self):
        status, body = self._post("/v1/face_detect", b"{not json")
        self.assertEqual(status, 400)
        self.assertEqual(body.get("kind"), "bad_request")

    def test_non_object_body_is_400(self):
        status, body = self._post("/v1/face_detect", [1, 2, 3])
        self.assertEqual(status, 400)

    def test_every_mcp_tool_is_routable(self):
        for name in ("face_detect", "face_embed", "object_detect", "person_embed",
                     "depth", "enhance_low_light", "ocr", "embed"):
            status, _ = self._post(f"/v1/{name}", {"image_path": "nope.jpg"})
            self.assertEqual(status, 200, name)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 3: Run it to verify it fails**

Run (from the worktree): `"D:\Dev\Hailo-8L-Analysis-Pipelines\.venv\Scripts\python.exe" -m unittest server/test_http_server.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'http_server'`.

- [ ] **Step 4: Write the sidecar**

Create `server/http_server.py`:

```python
"""HTTP sidecar for the offload-harness: the same tools server.py exposes over
MCP, over plain JSON-over-HTTP on loopback, so a Go client can call the NPU.

    hailo-http.cmd            (or)  python server/http_server.py --listen 127.0.0.1:18813

Contract (consumed by the harness's internal/hailoclient):
    GET  /health            -> hailo_status() dict, always 200
    POST /v1/<tool>  {json} -> that tool's dict, 200 (structured error dicts are
                               results, not HTTP errors); 404 unknown tool; 400 bad body

The process EXITS after HAILO_SIDECAR_IDLE_SEC seconds (default 300) without a
request — the harness spawns it on demand, so nothing lingers when the editor is
not using AI features and there is no scheduler to clean up.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import server  # noqa: E402  (the MCP module; its tool functions are plain callables)

TOOLS = {
    "face_detect": server.hailo_face_detect,
    "face_embed": server.hailo_face_embed,
    "object_detect": server.hailo_object_detect,
    "person_embed": server.hailo_person_embed,
    "depth": server.hailo_depth,
    "enhance_low_light": server.hailo_enhance_low_light,
    "ocr": server.hailo_ocr,
    "embed": server.hailo_embed,
}

_last_request = time.monotonic()
_lock = threading.Lock()  # one VDevice, one in-flight inference at a time


def _touch() -> None:
    global _last_request
    _last_request = time.monotonic()


class Handler(BaseHTTPRequestHandler):
    server_version = "hailo-sidecar/1.0"

    def log_message(self, fmt, *args):  # quiet by default; errors still surface
        if os.environ.get("HAILO_SIDECAR_LOG") == "1":
            super().log_message(fmt, *args)

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, default=str).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        _touch()
        if self.path != "/health":
            return self._send(404, {"error": True, "kind": "unknown_path", "message": self.path})
        return self._send(200, server.hailo_status())

    def do_POST(self):
        _touch()
        if not self.path.startswith("/v1/"):
            return self._send(404, {"error": True, "kind": "unknown_path", "message": self.path})
        tool = TOOLS.get(self.path[len("/v1/"):])
        if tool is None:
            return self._send(404, {"error": True, "kind": "unknown_tool", "message": self.path})
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            args = json.loads(raw or b"{}")
        except json.JSONDecodeError as e:
            return self._send(400, {"error": True, "kind": "bad_request", "message": f"body is not JSON: {e}"})
        if not isinstance(args, dict):
            return self._send(400, {"error": True, "kind": "bad_request", "message": "body must be a JSON object of tool arguments"})
        try:
            with _lock:
                result = tool(**args)
        except TypeError as e:  # wrong/missing keyword → the caller's problem, said plainly
            return self._send(400, {"error": True, "kind": "bad_request", "message": str(e)})
        return self._send(200, result)


def make_server(host: str, port: int, idle_sec: int) -> ThreadingHTTPServer:
    """Build (not start) the server. idle_sec<=0 disables the idle exit (tests)."""
    srv = ThreadingHTTPServer((host, port), Handler)
    if idle_sec > 0:
        def reaper():
            while True:
                time.sleep(5)
                if time.monotonic() - _last_request > idle_sec:
                    srv.shutdown()
                    return
        threading.Thread(target=reaper, daemon=True).start()
    return srv


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--listen", default="127.0.0.1:18813", help="host:port (loopback only)")
    ap.add_argument("--idle-sec", type=int, default=int(os.environ.get("HAILO_SIDECAR_IDLE_SEC", "300")))
    a = ap.parse_args()
    host, _, port = a.listen.rpartition(":")
    if host not in ("127.0.0.1", "localhost"):
        print("refusing to bind a non-loopback address; the sidecar is not an authenticated service", file=sys.stderr)
        return 2
    srv = make_server(host, int(port), a.idle_sec)
    print(f"hailo sidecar listening on {a.listen} (idle exit {a.idle_sec}s)", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `"D:\Dev\Hailo-8L-Analysis-Pipelines\.venv\Scripts\python.exe" -m unittest server/test_http_server.py -v`
Expected: 6 tests, `OK`.

- [ ] **Step 6: Launcher + port record + README**

Create `hailo-http.cmd`:

```bat
@echo off
REM hailo-http.cmd -- start the Hailo-8L HTTP sidecar the offload-harness calls.
REM Pins the venv interpreter and PYTHONPATH; the harness spawns this on demand
REM and the process exits itself after HAILO_SIDECAR_IDLE_SEC (default 300).
set "HAILO_ROOT=%~dp0"
set "HAILO_ROOT=%HAILO_ROOT:~0,-1%"
set "PYTHONPATH=%HAILO_ROOT%\shared"
if not defined HAILO_MODELS_DIR set "HAILO_MODELS_DIR=D:\Dev\hailo-models"
if not defined HAILO_VISION_ENABLED set "HAILO_VISION_ENABLED=1"
"%HAILO_ROOT%\.venv\Scripts\python.exe" "%HAILO_ROOT%\server\http_server.py" %*
```

Append to `README.md`:

```markdown
## HTTP sidecar (for the offload-harness)

`hailo-http.cmd` serves the same tools over loopback JSON-over-HTTP so a non-Python
caller (the offload-harness's `internal/hailoclient`) can use the NPU:

    GET  http://127.0.0.1:18813/health                 -> hailo_status()
    POST http://127.0.0.1:18813/v1/face_embed  {"image_path": "..."}  -> the tool's dict

The process exits on its own after `HAILO_SIDECAR_IDLE_SEC` (default 300 s) idle; the
harness starts it on demand, so nothing runs when AI features are not in use.
It refuses to bind anything but loopback — it is not an authenticated service.
```

Create/append `P:\Port Directory\optiplex7060-ports.md`:

```markdown
# OptiPlex 7060 ports
| Port | Owner | Notes |
|---|---|---|
| 11436 | llama-swap | hand-started, not persistent |
| 18811 | offload fleet-serve | hand-started, not persistent |
| 18813 | hailo HTTP sidecar | loopback only; harness-spawned on demand, self-exits idle |
```

- [ ] **Step 7: Smoke the real launcher (the only step that touches the NPU)**

Run on the Dell after pulling the branch:
`cmd /c "D:\Dev\Hailo-8L-Analysis-Pipelines\hailo-http.cmd --idle-sec 20"` in the background, then
`curl -s http://127.0.0.1:18813/health` → `"enabled": true`;
`curl -s -X POST http://127.0.0.1:18813/v1/object_detect -H "Content-Type: application/json" -d "{\"image_path\":\"<a jpg on D:>\"}"` → `{"objects":[…],"count":N}`.
Wait 25 s, then `Get-Process python` must not list the sidecar (idle exit observed).

- [ ] **Step 8: Commit, PR, merge (account re-check in its own call before each `gh` write)**

```bash
git add server/http_server.py server/test_http_server.py hailo-http.cmd README.md
git commit -m "feat: loopback HTTP sidecar over the MCP tools, idle self-exit (for the offload-harness)"
git push -u origin feat/http-sidecar
# gh api user --jq .login   (separate call; must print dmmdea)
gh pr create --repo dmmdea/Hailo-8L-Analysis-Pipelines --base main --head feat/http-sidecar --title "feat: HTTP sidecar for the offload-harness" --body "<intent / what / how tested: 6 unit tests + live smoke incl. idle exit / risk low>"
# gh api user --jq .login   (separate call)
gh pr merge <n> --repo dmmdea/Hailo-8L-Analysis-Pipelines --merge --delete-branch
```

---

### Task 2: Ground truth first — `profiles.json` accelerators + tier matrix

**Files:**
- Modify: `setup/templates/profiles.json` (top level, after `"_fields"`)
- Modify: `G:\My Drive\AI Ecosystem\Ecosystem\Arquitechture\generate-tier-matrix.py`
- Create: `docs/superpowers/plans/2026-08-22-hailo-8l-accelerator-tier.md` (copy of this file)

**Interfaces:**
- Produces: JSON shape `accelerators.<id> = {"kind","owns":[…],"config_seed":{…},"notes"}` read by Task 5 (`tierseed.ResolveAccelerators`) and Task 6 (`install.ps1`). Seed keys here are the exact `config.Config` json tags Task 3 adds: `accelerators`, `hailo_endpoint`, `hailo_sidecar_cmd`, `hailo_timeout_sec`, `hailo_idle_sec`. Token `__HAILO_HOME__` expands to the Hailo repo dir (Task 5/6).

- [ ] **Step 1: Land the harness worktree**

```bash
git -C "D:/Dev/dmmdea/local-offload-public" worktree add "D:/dev/worktrees/harness-hailo" -b feat/hailo-8l-accelerator
cd "D:/dev/worktrees/harness-hailo" && git rev-parse --abbrev-ref HEAD && pwd && go build ./... && echo BUILD-OK
```

- [ ] **Step 2: Add the accelerators block**

In `setup/templates/profiles.json`, insert between the `"_fields": {…}` object and `"profiles": {`:

```json
  "_accelerator_fields": {
    "kind": "npu — the only kind today. An accelerator is ADDITIVE to a box's GPU tier (ADR 0024): `profile` stays one string and `accelerators` lists what rides beside it.",
    "owns": "the offload_* capabilities this accelerator serves EXCLUSIVELY when present; the GPU tier keeps everything else (VQA/description stay on the VLM; OCR stays GPU-primary with engine:npu as the caller's explicit fast path).",
    "config_seed": "config.json keys merged AFTER the GPU tier's seed (internal/tierseed.ResolveAccelerators). __HAILO_HOME__ expands to the Hailo repo checkout (installer env HAILO_HOME, default <OFFLOAD_HOME>/hailo).",
    "detect": "how detect.ps1 / hwdetect recognise the device — documentation of the rule implemented in code."
  },
  "accelerators": {
    "hailo-8l": {
      "kind": "npu",
      "owns": ["face_detect", "face_embed", "object_detect", "person_embed", "depth", "enhance_low_light", "image_embed"],
      "detect": "hailortcli scan lists a device AND hailortcli fw-control identify reports 'Device Architecture: HAILO8L'",
      "config_seed": {
        "accelerators": ["hailo-8l"],
        "hailo_endpoint": "http://127.0.0.1:18813",
        "hailo_sidecar_cmd": "__HAILO_HOME__/hailo-http.cmd",
        "hailo_timeout_sec": 60,
        "hailo_idle_sec": 300
      },
      "notes": "Hailo-8L M.2 (HM21LB1C2KAE), HailoRT 4.24.0 + model zoo v2.19.0 hailo8l HEFs — the current matched pair (5.x is the Hailo-10/15 line). Sidecar = harness-spawned on demand over loopback HTTP, self-exits idle (operator decision 2026-08-22: no scheduler, clean box). Measured on the OptiPlex 7060: ArcFace same-person 0.853 vs different ≤0.106; YOLOv8s on-chip NMS; TinyCLIP-61M 512-d. Windows cannot see the device as an NPU (no MCDM driver) — irrelevant to this route."
    }
  },
```

- [ ] **Step 3: Prove the JSON still parses and every profile test is untouched**

Run: `python -c "import json; d=json.load(open('setup/templates/profiles.json')); print(sorted(d['accelerators']), len(d['profiles']))"`
Expected: `['hailo-8l'] 15`.
Run: `go test ./internal/tierdocs/ ./internal/servingtmpl/ -count=1 && go test -run TestTierDocsAreCurrent -count=1 .`
Expected: PASS (the new top-level key is ignored by `tierdocs.doc`; `docs/tiers` unchanged).

- [ ] **Step 4: Teach the matrix generator the Accelerators sheet**

In `generate-tier-matrix.py`, after the `profiles = data["profiles"]` line add:

```python
accelerators = data.get("accelerators", {})
```

and immediately before the `# Carry the three secondary sheets` comment insert:

```python
# Accelerators sheet (0.81.0): additive NPU/other devices that ride beside a GPU
# tier (ADR 0024). Generated, not carried — a row here is a seat DECISION like
# every GPU-tier row, so it must come from profiles.json, never be hand-edited.
acc_ws = wb.create_sheet("Accelerators")
acc_headers = ["Accelerator", "Kind", "Owns (exclusive offload_* routes)", "Endpoint (default)",
               "Sidecar launcher", "Timeout / idle (s)", "Detection rule", "Add-on to", "Notes"]
acc_widths = [14, 8, 46, 26, 34, 16, 52, 28, 80]
for i, (h, w) in enumerate(zip(acc_headers, acc_widths), 1):
    c = acc_ws.cell(row=1, column=i, value=h)
    c.font = HDR_FONT; c.fill = HDR_FILL
    c.alignment = Alignment(vertical="center", wrap_text=True)
    acc_ws.column_dimensions[get_column_letter(i)].width = w
acc_ws.freeze_panes = "B2"
ar = 2
for aid in sorted(accelerators):
    a = accelerators[aid]
    cs = a.get("config_seed") or {}
    row = [aid, a.get("kind", ""), ", ".join(a.get("owns", [])), cs.get("hailo_endpoint", ""),
           cs.get("hailo_sidecar_cmd", ""), f"{cs.get('hailo_timeout_sec', '')} / {cs.get('hailo_idle_sec', '')}",
           a.get("detect", ""), "any GPU tier on the same box (additive; the GPU profile stays the tier)",
           a.get("notes", "")]
    for i, val in enumerate(row, 1):
        c = acc_ws.cell(row=ar, column=i, value=val)
        c.font = BODY; c.border = THIN
        c.alignment = Alignment(vertical="top", wrap_text=True)
    ar += 1
n = acc_ws.cell(row=ar + 1, column=1, value=(
    "Accelerators are ADDITIVE: a box keeps its single GPU tier id (`profile`) and lists accelerators beside it "
    "(`accelerators: []` in installed.json / config.json). Ownership when both are present: the accelerator owns the "
    "routes in its Owns column exclusively; VQA/description stay on the GPU VLM; OCR stays GPU-primary with "
    "engine:npu as the caller's explicit fast-batch path. Decided by the operator 2026-08-22. As of 0.81.0."))
n.font = NOTE
acc_ws.merge_cells(start_row=ar + 1, start_column=1, end_row=ar + 1, end_column=len(acc_headers))
acc_ws.row_dimensions[ar + 1].height = 48
```

- [ ] **Step 5: Regenerate the matrix (close Excel first) and verify the sheet**

Run: `python "G:/My Drive/AI Ecosystem/Ecosystem/Arquitechture/generate-tier-matrix.py" D:/dev/worktrees/harness-hailo/setup/templates/profiles.json`
Expected: `saved … sheets: ['GPU Tier x Model Matrix', 'Accelerators', 'Qube Live Seats', 'llama-swap Roster', 'Tiers & Doctrine']`.
Run: `python -c "import openpyxl; ws=openpyxl.load_workbook(r'G:\My Drive\AI Ecosystem\Ecosystem\Arquitechture\2026-08-16_tier-model-matrix.xlsx')['Accelerators']; print(ws['A2'].value, '|', ws['C2'].value)"`
Expected: `hailo-8l | face_detect, face_embed, object_detect, person_embed, depth, enhance_low_light, image_embed`.

- [ ] **Step 6: Copy the plan into the repo and commit**

```bash
mkdir -p docs/superpowers/plans && cp "C:/Users/dmmde/.claude/plans/2026-08-22-hailo-8l-accelerator-tier.md" docs/superpowers/plans/
git add setup/templates/profiles.json docs/superpowers/plans/2026-08-22-hailo-8l-accelerator-tier.md
git commit -m "tiers: declare the hailo-8l accelerator (additive to the GPU tier) + plan

Ground truth first: profiles.json gains a top-level accelerators map, separate
from profiles so nothing that enumerates GPU tiers changes; the tier matrix
xlsx gained an Accelerators sheet (generator updated beside the workbook)."
```

---

### Task 3: Config — `accelerators` + `hailo_*` keys

**Files:**
- Modify: `internal/config/config.go` (struct: after the NIM block at the `// --- fleet-node server` comment; `Default()`: after `NIMMaxTokens`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.Accelerators []string`, `Config.HailoEndpoint string`, `Config.HailoSidecarCmd string`, `Config.HailoTimeoutSec int`, `Config.HailoIdleSec int`; `func (c Config) HasAccelerator(id string) bool`. Defaults: endpoint `http://127.0.0.1:18813`, timeout 60, idle 300, sidecar cmd `""`, accelerators `nil`.

- [ ] **Step 1: Write the failing tests** (append to `config_test.go`)

```go
func TestAcceleratorDefaultsAreInert(t *testing.T) {
	c := Default()
	if len(c.Accelerators) != 0 {
		t.Fatalf("Accelerators default = %v, want empty (a box with no NPU is byte-identical to today)", c.Accelerators)
	}
	if c.HasAccelerator("hailo-8l") {
		t.Fatal("HasAccelerator(hailo-8l) true on a default config")
	}
	if c.HailoEndpoint != "http://127.0.0.1:18813" {
		t.Errorf("HailoEndpoint = %q, want loopback :18813", c.HailoEndpoint)
	}
	if c.HailoSidecarCmd != "" {
		t.Errorf("HailoSidecarCmd = %q, want \"\" (an installer seeds it; never a baked path)", c.HailoSidecarCmd)
	}
	if c.HailoTimeoutSec != 60 || c.HailoIdleSec != 300 {
		t.Errorf("timeout/idle = %d/%d, want 60/300", c.HailoTimeoutSec, c.HailoIdleSec)
	}
}

func TestAcceleratorFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"accelerators":["hailo-8l"],"hailo_endpoint":"http://127.0.0.1:19999","hailo_sidecar_cmd":"D:/x/hailo-http.cmd","hailo_timeout_sec":7,"hailo_idle_sec":9}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasAccelerator("hailo-8l") || c.HasAccelerator("tpu") {
		t.Fatalf("HasAccelerator: got %v", c.Accelerators)
	}
	if c.HailoEndpoint != "http://127.0.0.1:19999" || c.HailoSidecarCmd != "D:/x/hailo-http.cmd" || c.HailoTimeoutSec != 7 || c.HailoIdleSec != 9 {
		t.Fatalf("round-trip lost a field: %+v", c)
	}
}
```
(`Load` is the existing loader used by `TestFleetFieldsRoundTrip` at `config_test.go:165` — reuse its exact call shape if it differs from `Load(p)`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run 'TestAccelerator' -count=1`
Expected: compile error `c.Accelerators undefined`.

- [ ] **Step 3: Add the fields + defaults + helper**

In the `Config` struct, directly above `// --- fleet-node server (`fleet-serve` / `fleet-measure`; docs/FLEET-NODE.md) ---`:

```go
	// --- accelerators (ADR 0024): devices that ride BESIDE the GPU tier ---
	// Accelerators lists the additive accelerator ids present on this box
	// (today: "hailo-8l"). `profile` stays the one GPU tier; an empty list is
	// byte-identical to a box with no accelerator — tools/list does not change.
	Accelerators []string `json:"accelerators,omitempty"`
	// HailoEndpoint is the loopback HTTP sidecar base (server/http_server.py in
	// the Hailo repo). Loopback only — the sidecar is not an authenticated service.
	HailoEndpoint string `json:"hailo_endpoint,omitempty"`
	// HailoSidecarCmd launches the sidecar on demand (hailo-http.cmd). Empty =
	// never spawn: the harness expects something else to have started it and
	// defers when /health is unreachable.
	HailoSidecarCmd string `json:"hailo_sidecar_cmd,omitempty"`
	// HailoTimeoutSec bounds one NPU call (cold HEF load is ~1-8 s). Default 60.
	HailoTimeoutSec int `json:"hailo_timeout_sec,omitempty"`
	// HailoIdleSec is passed to the sidecar as its self-exit idle window. Default 300.
	HailoIdleSec int `json:"hailo_idle_sec,omitempty"`
```

In `Default()`, after `NIMMaxTokens: 1024,`:

```go
		HailoEndpoint:   "http://127.0.0.1:18813",
		HailoTimeoutSec: 60,
		HailoIdleSec:    300,
```

After the `ModelRoutes` method add:

```go
// HasAccelerator reports whether id is listed in Accelerators. It is THE gate
// for accelerator-backed tools and status blocks: registration, not routing
// heuristics, so tools/list stays byte-identical on a box without the device.
func (c Config) HasAccelerator(id string) bool {
	for _, a := range c.Accelerators {
		if a == id {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/config/ -count=1`
Expected: PASS (including `TestPathFieldsCoverEveryPathTypedStructField` — `HailoSidecarCmd` is a command path; if that test flags it, add it to the path-field list the test enumerates, following how `SdcppBin` is listed).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: accelerators list + hailo_* sidecar keys (defaults inert)"
```

---

### Task 4: Detection — `Verdict.Accelerators` in Go and PowerShell

**Files:**
- Modify: `internal/hwdetect/classify.go` (`Verdict` struct at :60; new funcs at end of file)
- Test: `internal/hwdetect/classify_test.go`
- Modify: `setup/detect.ps1` (new function before `Get-Profile`; the verdict object at the end; the self-test table)
- Test: `setup/detect.tests.ps1`

**Interfaces:**
- Produces: `Verdict.Accelerators []string` (json `accelerators`), `func AcceleratorsFromHailortcli(scanOut, identifyOut string) []string` (pure), `func DetectAccelerators(run func(args ...string) (string, error)) []string` (wrapper; `run` executes `hailortcli <args>`). detect.ps1 emits `accelerators=@(...)` in its JSON verdict.

- [ ] **Step 1: Failing Go test** (append to `classify_test.go`)

```go
func TestAcceleratorsFromHailortcli(t *testing.T) {
	scan := "Hailo Devices:\n[-] Device: 0000:03:00.0\n"
	ident := "Executing on device: 0000:03:00.0\nIdentifying board\nDevice Architecture: HAILO8L\nPart Number: HM21LB1C2KAE\n"
	if got := AcceleratorsFromHailortcli(scan, ident); len(got) != 1 || got[0] != "hailo-8l" {
		t.Fatalf("got %v, want [hailo-8l]", got)
	}
	// A full Hailo-8 is NOT an 8L: its HEFs are a different build and must not claim the tier.
	if got := AcceleratorsFromHailortcli(scan, "Device Architecture: HAILO8\n"); len(got) != 0 {
		t.Fatalf("HAILO8 classified as %v, want none", got)
	}
	if got := AcceleratorsFromHailortcli("Hailo Devices:\n", ""); len(got) != 0 {
		t.Fatalf("no device classified as %v, want none", got)
	}
}

func TestDetectAcceleratorsToleratesMissingTool(t *testing.T) {
	run := func(args ...string) (string, error) { return "", errors.New("exec: hailortcli: not found") }
	if got := DetectAccelerators(run); got != nil {
		t.Fatalf("got %v, want nil when hailortcli is absent", got)
	}
}

func TestVerdictCarriesAccelerators(t *testing.T) {
	v := Classify(Facts{Vendor: "nvidia", Arch: "blackwell", VRAMGb: 8, GPUCount: 1, RAMGb: 64})
	if v.Profile != "blackwell-8" {
		t.Fatalf("profile = %q", v.Profile)
	}
	if v.Accelerators != nil {
		t.Fatalf("Classify must not populate accelerators (detection is a separate probe); got %v", v.Accelerators)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), `"accelerators":null`) {
		t.Fatalf("nil accelerators must be omitted from JSON, got %s", b)
	}
}
```
Add `"encoding/json"`, `"errors"`, `"strings"` to the test imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/hwdetect/ -run 'Accelerator|VerdictCarries' -count=1`
Expected: compile error `undefined: AcceleratorsFromHailortcli`.

- [ ] **Step 3: Implement**

In `Verdict`, after `RAMTier string \`json:"ram_tier"\``:

```go
	// Accelerators lists additive devices found BESIDE the GPU (ADR 0024), e.g.
	// "hailo-8l". Classify never fills it — detection is a separate probe
	// (DetectAccelerators) the installers run and merge, so a box with no NPU
	// serialises exactly as before (omitempty).
	Accelerators []string `json:"accelerators,omitempty"`
```

Append to `classify.go`:

```go
// AcceleratorsFromHailortcli classifies the Hailo device from hailortcli's own
// output: `scan` must list at least one "Device:" line and `identify` must
// report the 8L architecture. A full Hailo-8 is deliberately NOT matched — its
// model-zoo HEFs are a different build (hailo8/ vs hailo8l/) and would be
// rejected at load with an architecture mismatch.
func AcceleratorsFromHailortcli(scanOut, identifyOut string) []string {
	if !strings.Contains(scanOut, "Device:") {
		return nil
	}
	for _, line := range strings.Split(identifyOut, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "Device Architecture" && strings.TrimSpace(v) == "HAILO8L" {
			return []string{"hailo-8l"}
		}
	}
	return nil
}

// DetectAccelerators probes for accelerators with an injected runner so the
// decision stays pure and testable. run executes `hailortcli <args...>`; any
// error (tool absent, driver down) means "no accelerator" — never a failure,
// because a missing NPU is the normal case on most boxes.
func DetectAccelerators(run func(args ...string) (string, error)) []string {
	scan, err := run("scan")
	if err != nil {
		return nil
	}
	ident, err := run("fw-control", "identify")
	if err != nil {
		return nil
	}
	return AcceleratorsFromHailortcli(scan, ident)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/hwdetect/ -count=1`
Expected: PASS (the shipped-table test is unchanged).

- [ ] **Step 5: PowerShell port** — in `setup/detect.ps1`, insert before `function Get-Profile`:

```powershell
# Accelerators (ADR 0024): additive devices beside the GPU. Mirror of
# internal/hwdetect.AcceleratorsFromHailortcli — change the Go rule FIRST, then
# this. A missing hailortcli or a failed probe is "no accelerator", never an error.
function Get-Accelerators {
  $cli = Get-Command hailortcli -ErrorAction SilentlyContinue
  if (-not $cli) { $cli = Get-Item 'C:\Program Files\HailoRT\bin\hailortcli.exe' -ErrorAction SilentlyContinue }
  if (-not $cli) { return @() }
  $exe = if ($cli.PSObject.Properties['Source']) { $cli.Source } else { $cli.FullName }
  try { $scan = (& $exe scan 2>$null) -join "`n" } catch { return @() }
  if ($scan -notmatch 'Device:') { return @() }
  try { $ident = (& $exe fw-control identify 2>$null) -join "`n" } catch { return @() }
  if ($ident -match '(?m)^\s*Device Architecture:\s*HAILO8L\s*$') { return @('hailo-8l') }
  return @()
}
```

After the `$profileId = $sel.profile` / `$bigRam` lines add:

```powershell
$accelerators = @(Get-Accelerators)
if ($env:OFFLOAD_ACCELERATORS) { $accelerators = @($env:OFFLOAD_ACCELERATORS -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
```

In the `Write-Host "Profile: …"` line's neighbourhood add `Write-Host "Accelerators: $(if ($accelerators.Count) { $accelerators -join ', ' } else { 'none' })"`, and in the final `@{ … }` verdict object add `accelerators=@($accelerators);` after `big_ram=$bigRam;`.

- [ ] **Step 6: PowerShell self-test** — in `setup/detect.tests.ps1` add, following its existing `Assert-*` style:

```powershell
Write-Host "== accelerators: Get-Accelerators is pure on the verdict (no device here) =="
. (Join-Path $PSScriptRoot 'detect.ps1') -SelfTest  # existing self-test entry keeps the functions loaded
$json = (& $psHost -NoProfile -File (Join-Path $PSScriptRoot 'detect.ps1') | Where-Object { $_ -match '^\s*\{.*\}\s*$' } | Select-Object -Last 1) | ConvertFrom-Json
Assert ($null -ne $json.PSObject.Properties['accelerators']) 'verdict carries an accelerators array (may be empty)'
$env:OFFLOAD_ACCELERATORS = 'hailo-8l'
$json2 = (& $psHost -NoProfile -File (Join-Path $PSScriptRoot 'detect.ps1') | Where-Object { $_ -match '^\s*\{.*\}\s*$' } | Select-Object -Last 1) | ConvertFrom-Json
Remove-Item Env:OFFLOAD_ACCELERATORS
Assert (@($json2.accelerators) -contains 'hailo-8l') 'OFFLOAD_ACCELERATORS override lands in the verdict'
```
(If `detect.tests.ps1` does not already define `$psHost`, set `$psHost = (Get-Process -Id $PID).Path` above, exactly as install.ps1 does.)

Run: `powershell -ExecutionPolicy Bypass -File setup\detect.tests.ps1` → exit 0, both new asserts PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/hwdetect/classify.go internal/hwdetect/classify_test.go setup/detect.ps1 setup/detect.tests.ps1
git commit -m "hwdetect: accelerators beside the GPU tier — hailo-8l via hailortcli (Go + PS parity)"
```

---

### Task 5: Seeding — `tierseed.ResolveAccelerators`

**Files:**
- Modify: `internal/tierseed/tierseed.go` (new types + func after `Resolve`)
- Test: `internal/tierseed/tierseed_test.go`

**Interfaces:**
- Consumes: `profiles.json` `accelerators` map (Task 2). `Options` already carries `Home`, `GOOS`; add `HailoHome string`.
- Produces: `type Accelerator struct { Kind string; Owns []string; ConfigSeed map[string]any; Notes string }`, `type Doc struct { Profiles map[string]Profile; Accelerators map[string]Accelerator }` (if a `Doc`/loader type already exists in the package, extend it rather than adding a second), `func ResolveAccelerators(accs map[string]Accelerator, ids []string, opt Options) (map[string]any, error)` — merged, validated, `__HAILO_HOME__`/`__OFFLOAD_HOME__`/`__EXE__` expanded. Unknown id → error.

- [ ] **Step 1: Failing test**

```go
func TestResolveAcceleratorsExpandsAndValidates(t *testing.T) {
	accs := map[string]Accelerator{
		"hailo-8l": {Kind: "npu", ConfigSeed: map[string]any{
			"accelerators": []any{"hailo-8l"}, "hailo_endpoint": "http://127.0.0.1:18813",
			"hailo_sidecar_cmd": "__HAILO_HOME__/hailo-http.cmd", "hailo_timeout_sec": 60,
		}},
	}
	out, err := ResolveAccelerators(accs, []string{"hailo-8l"}, Options{Home: `C:\stack`, HailoHome: `D:\Dev\Hailo`, GOOS: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if out["hailo_sidecar_cmd"] != "D:/Dev/Hailo/hailo-http.cmd" {
		t.Fatalf("token not expanded: %v", out["hailo_sidecar_cmd"])
	}
	if _, err := ResolveAccelerators(accs, []string{"tpu"}, Options{}); err == nil {
		t.Fatal("unknown accelerator id must be an error, not a silent skip")
	}
	bad := map[string]Accelerator{"x": {ConfigSeed: map[string]any{"no_such_key": 1}}}
	if _, err := ResolveAccelerators(bad, []string{"x"}, Options{}); err == nil {
		t.Fatal("a seed key that is not a config.Config json tag must fail at authoring time")
	}
	if out, _ := ResolveAccelerators(accs, nil, Options{}); out != nil {
		t.Fatalf("no ids -> nil seed, got %v", out)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/tierseed/ -run ResolveAccelerators -count=1` → `undefined: Accelerator`.

- [ ] **Step 3: Implement** (append to `tierseed.go`; add `HailoHome string` to `Options` with the comment `// HailoHome is the Hailo repo checkout __HAILO_HOME__ expands to.`)

```go
// Accelerator is one profiles.json `accelerators` entry: an additive device
// beside the GPU tier (ADR 0024). Its seed merges AFTER the tier's own seed.
type Accelerator struct {
	Kind       string         `json:"kind"`
	Owns       []string       `json:"owns"`
	ConfigSeed map[string]any `json:"config_seed"`
	Notes      string         `json:"notes"`
}

// ResolveAccelerators merges the seeds of the listed accelerator ids, validates
// every key against config.Config, and expands __HAILO_HOME__ (plus the usual
// __OFFLOAD_HOME__/__EXE__). An id with no entry is an authoring error — an
// installer that detected a device the table does not describe must say so.
func ResolveAccelerators(accs map[string]Accelerator, ids []string, opt Options) (map[string]any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	merged := map[string]any{}
	for _, id := range ids {
		a, ok := accs[id]
		if !ok {
			return nil, fmt.Errorf("accelerator %q detected but not declared in profiles.json accelerators", id)
		}
		for k, v := range a.ConfigSeed {
			merged[k] = v
		}
	}
	if err := validate(merged, "", "accelerators:"+strings.Join(ids, "+")); err != nil {
		return nil, err
	}
	goos := opt.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	home := strings.TrimRight(strings.ReplaceAll(opt.Home, `\`, "/"), "/")
	hailoHome := strings.TrimRight(strings.ReplaceAll(opt.HailoHome, `\`, "/"), "/")
	out := map[string]any{}
	for k, v := range merged {
		ev := expand(v, home, exe)
		if s, ok := ev.(string); ok {
			ev = strings.ReplaceAll(s, "__HAILO_HOME__", hailoHome)
		}
		out[k] = ev
	}
	return out, nil
}
```
If `validate` rejects an empty backend for a key that is backend-gated (`vae_mode`), that is correct — accelerator seeds never carry it.

- [ ] **Step 4: Run to verify pass** — `go test ./internal/tierseed/ -count=1` → PASS.

- [ ] **Step 5: Commit** — `git add internal/tierseed && git commit -m "tierseed: ResolveAccelerators — accelerator seeds merge after the tier seed, __HAILO_HOME__ token"`

---

### Task 6: Installer + manifest + fleet advertisement

**Files:**
- Modify: `setup/install.ps1` (verdict parse ~:866; seed merge ~:1300; manifest ~:1346)
- Test: `setup/tests/install-config-seed.test.ps1`
- Modify: `internal/fleetnode/gpuinfo.go` (`InstalledInfo` :70)
- Test: `internal/fleetnode/gpuinfo_test.go`

**Interfaces:**
- Consumes: detect verdict `accelerators` (Task 4), `profiles.json` `accelerators` (Task 2).
- Produces: `installed.json` gains `accelerators = @(...)`; a fresh `config.json` gains the accelerator seed; `InstalledInfo.Accelerators []string`; `/fleet/health` payload gains `accelerators` (find where `InstalledInfo` is folded into the health JSON in `internal/fleetnode/server.go` — grep `Backend` — and add the field beside it).

- [ ] **Step 1: Failing installer test** (append to `install-config-seed.test.ps1`)

```powershell
Write-Host "== accelerator seed: merged after the tier seed, __HAILO_HOME__ expanded =="
Assert ([bool](Get-Command Get-AcceleratorSeed -ErrorAction SilentlyContinue)) 'dot-source seam defines Get-AcceleratorSeed'
$pdoc = Get-Content -Raw (Join-Path (Join-Path $setupDir 'templates') 'profiles.json') | ConvertFrom-Json
$accSeed = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @('hailo-8l') -HailoHome 'D:\Dev\Hailo'
Assert ($null -ne $accSeed) 'seed returned for hailo-8l'
$m2 = Merge-ConfigSeed -ConfigText $tplText -Seed $accSeed -OffloadHome 'C:\stack'
$o2 = $m2 | ConvertFrom-Json
Assert (@($o2.accelerators) -contains 'hailo-8l') 'config.accelerators lists hailo-8l'
Assert ($o2.hailo_sidecar_cmd -eq 'D:/Dev/Hailo/hailo-http.cmd') 'hailo_sidecar_cmd expanded __HAILO_HOME__'
Assert ($o2.hailo_endpoint -eq 'http://127.0.0.1:18813') 'hailo_endpoint seeded'
$none = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @() -HailoHome 'D:\x'
Assert ($null -eq $none) 'no accelerators -> no seed (config byte-identical to today)'
$threw = $false
try { Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids @('tpu') -HailoHome 'D:\x' | Out-Null } catch { $threw = $true }
Assert $threw 'undeclared accelerator id throws (authoring error, never silent)'
```

- [ ] **Step 2: Run to verify failure** — `powershell -ExecutionPolicy Bypass -File setup\tests\install-config-seed.test.ps1` → the new asserts FAIL (`Get-AcceleratorSeed` undefined).

- [ ] **Step 3: Implement in `install.ps1`**

Next to `function Merge-ConfigSeed` add:

```powershell
# Accelerator seed (ADR 0024): parity copy of internal/tierseed.ResolveAccelerators
# (authoritative; change Go FIRST). Merged AFTER the tier seed so an accelerator can
# never be overwritten by the GPU tier's keys. __HAILO_HOME__ = the Hailo repo dir.
function Get-AcceleratorSeed {
  param($ProfilesDoc, [string[]]$Ids, [string]$HailoHome)
  if (-not $Ids -or $Ids.Count -eq 0) { return $null }
  $merged = [ordered]@{}
  foreach ($id in $Ids) {
    if (-not $ProfilesDoc.accelerators -or -not $ProfilesDoc.accelerators.PSObject.Properties[$id]) {
      throw "accelerator '$id' detected but not declared in profiles.json accelerators"
    }
    $seed = $ProfilesDoc.accelerators.$id.config_seed
    foreach ($p in $seed.PSObject.Properties) { $merged[$p.Name] = $p.Value }
  }
  $hh = ($HailoHome -replace '\\', '/').TrimEnd('/')
  $out = [ordered]@{}
  foreach ($k in $merged.Keys) {
    $v = $merged[$k]
    if ($v -is [string]) { $v = $v.Replace('__HAILO_HOME__', $hh) }
    $out[$k] = $v
  }
  return [pscustomobject]$out
}
```

At the verdict parse (after `$cudaToolkit = $verdictObj.cuda_toolkit`):

```powershell
  $accelerators = @($verdictObj.accelerators | Where-Object { $_ })
```
and with the overrides: `if ($env:OFFLOAD_ACCELERATORS) { $accelerators = @($env:OFFLOAD_ACCELERATORS -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }`; initialise `$accelerators = @()` on the `$profileId = $null; …` line. `$HAILO_HOME = if ($env:HAILO_HOME) { $env:HAILO_HOME } else { Join-Path $HOME_DIR 'hailo' }` beside `$HOME_DIR`'s definition.

In Step 8's seed block, after the `$agentSeat` merge and before `WriteAllText`:

```powershell
    $accSeed = Get-AcceleratorSeed -ProfilesDoc $pdoc -Ids $accelerators -HailoHome $HAILO_HOME
    if ($accSeed) {
      $cfgText = Merge-ConfigSeed -ConfigText $cfgText -Seed $accSeed -OffloadHome $HOME_DIR
      Write-Host "      accelerators ($($accelerators -join ',')): $(@($accSeed.PSObject.Properties.Name) -join ', ')" -ForegroundColor DarkGray
    }
```
(`$pdoc` is only set when `$profileId -and (Test-Path $profilesJson)`; hoist `$pdoc = Get-Content -Raw $profilesJson | ConvertFrom-Json` above that `if` guarded by `Test-Path $profilesJson` so an accelerator on a profile-less render still seeds.)

In the manifest, after `big_ram        = $bigRam`: `accelerators   = @($accelerators)`.

- [ ] **Step 4: Run to verify pass** — `powershell -ExecutionPolicy Bypass -File setup\tests\install-config-seed.test.ps1` → exit 0. Also `pwsh -File setup/render.tests.ps1` → unchanged PASS (no profile changed).

- [ ] **Step 5: Fleet advertisement (Go)** — in `gpuinfo.go` `InstalledInfo` add `Accelerators []string \`json:"accelerators,omitempty"\`` with the comment `// Accelerators are the additive devices the installer detected (ADR 0024); advertised so a delegator can route NPU-owned work here.` In `gpuinfo_test.go` add:

```go
func TestReadInstalledInfoCarriesAccelerators(t *testing.T) {
	p := filepath.Join(t.TempDir(), "installed.json")
	os.WriteFile(p, []byte(`{"profile":"blackwell-8","backend":"cuda","accelerators":["hailo-8l"]}`), 0o644)
	info, err := ReadInstalledInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Accelerators) != 1 || info.Accelerators[0] != "hailo-8l" {
		t.Fatalf("accelerators = %v", info.Accelerators)
	}
	os.WriteFile(p, []byte(`{"profile":"blackwell-8","backend":"cuda"}`), 0o644)
	info, _ = ReadInstalledInfo(p)
	if info.Accelerators != nil {
		t.Fatalf("absent field must stay nil, got %v", info.Accelerators)
	}
}
```
Then surface it in the health payload: grep `internal/fleetnode/server.go` for where the resolved provider's `Vendor`/`Arch` are written into the `/fleet/health` JSON and add `"accelerators": info.Accelerators` beside them (the `InstalledInfo` value is already in scope there via `ReadInstalledInfo(InstalledJSONPath())`). Add an assertion to the existing health test in `server_test.go` that a manifest with `accelerators` shows up in the payload.

Run: `go test ./internal/fleetnode/ -count=1` → PASS.

- [ ] **Step 6: Commit**

```bash
git add setup/install.ps1 setup/tests/install-config-seed.test.ps1 internal/fleetnode
git commit -m "installer+fleet: detect accelerators, seed their config after the tier seed, advertise in installed.json and /fleet/health"
```

---

### Task 7: `internal/hailoclient` — client + on-demand sidecar

**Files:**
- Create: `internal/hailoclient/hailoclient.go`, `internal/hailoclient/sidecar.go`
- Test: `internal/hailoclient/hailoclient_test.go`, `internal/hailoclient/sidecar_test.go`

**Interfaces:**
- Consumes: the Task 1 wire contract.
- Produces:
  - `type Client struct{…}`; `func New(base string, timeout time.Duration) *Client`; `func (c *Client) Health(ctx) (map[string]any, error)`; `func (c *Client) Call(ctx, tool string, args map[string]any) (map[string]any, error)` — a 200 with `{"error":true}` is returned as the map with `err == nil` (it is a result); 4xx/5xx → `error` carrying the body.
  - `type Sidecar struct{…}`; `func NewSidecar(c *Client, spawn func() error, startTimeout time.Duration) *Sidecar`; `func (s *Sidecar) Ensure(ctx) error` — healthy → nil; else spawn once and poll `/health` until `startTimeout`; `spawn == nil` → error `"sidecar not running and no hailo_sidecar_cmd configured"`.
  - `func SpawnCmd(cmdPath string, idleSec int) func() error` — Windows: `exec.Command("cmd.exe", "/c", cmdPath, "--idle-sec", strconv.Itoa(idleSec))` with `cmd.Start()` (not `Run`: the sidecar outlives the call), `HideWindow` via `syscall.SysProcAttr{HideWindow: true}` (house rule: no visible consoles); non-Windows: `exec.Command("sh", "-c", …)`.

- [ ] **Step 1: Failing client tests**

```go
package hailoclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fakeSidecar(t *testing.T, enabled bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"enabled": enabled, "hefs_missing": []string{}})
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "bad_request"})
			return
		}
		switch r.URL.Path {
		case "/v1/face_embed":
			json.NewEncoder(w).Encode(map[string]any{"faces": []any{map[string]any{"embedding": []float64{0.1, 0.2}}}, "count": 1, "seen": args["image_path"]})
		case "/v1/depth":
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "image_missing", "message": "nope"})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "unknown_tool"})
		}
	})
	return httptest.NewServer(mux)
}

func TestCallReturnsResultAndEchoesArgs(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	c := New(srv.URL, 5*time.Second)
	out, err := c.Call(context.Background(), "face_embed", map[string]any{"image_path": "a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if out["seen"] != "a.jpg" || out["count"].(float64) != 1 {
		t.Fatalf("unexpected result %v", out)
	}
}

func TestStructuredErrorIsAResultNotAnError(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	out, err := New(srv.URL, time.Second).Call(context.Background(), "depth", map[string]any{})
	if err != nil {
		t.Fatalf("a 200 with error:true is a structured result; got err %v", err)
	}
	if out["kind"] != "image_missing" {
		t.Fatalf("kind = %v", out["kind"])
	}
}

func TestHTTPErrorSurfacesBody(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	if _, err := New(srv.URL, time.Second).Call(context.Background(), "teleport", map[string]any{}); err == nil {
		t.Fatal("404 must be an error")
	}
}

func TestHealth(t *testing.T) {
	srv := fakeSidecar(t, false)
	defer srv.Close()
	h, err := New(srv.URL, time.Second).Health(context.Background())
	if err != nil || h["enabled"] != false {
		t.Fatalf("health = %v, %v", h, err)
	}
}

func TestUnreachableIsAnError(t *testing.T) {
	if _, err := New("http://127.0.0.1:1", 200*time.Millisecond).Health(context.Background()); err == nil {
		t.Fatal("closed port must error")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/hailoclient/ -count=1` → `undefined: New`.

- [ ] **Step 3: Implement `hailoclient.go`**

```go
// Package hailoclient calls the Hailo-8L HTTP sidecar (server/http_server.py in
// the Hailo repo) over loopback. It is the harness's accelerator lane (ADR 0024):
// LOCAL and free like llama-swap, never cloud — but a separate device with its
// own process, so it gets its own client rather than riding the OpenAI shape.
// Mirrors nimclient: pure net/http, no SDK, a result is a map the caller shapes.
package hailoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client targets one sidecar base (scheme://host:port, no path).
type Client struct {
	base string
	http *http.Client
}

// New builds a client. timeout bounds ONE call including a cold HEF load.
func New(base string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

// Base is the configured endpoint, for status reporting.
func (c *Client) Base() string { return c.base }

// Health returns the sidecar's hailo_status() dict. An unreachable sidecar is an
// error — the caller decides whether to spawn it (Sidecar.Ensure) or defer.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// Call POSTs args to /v1/<tool> and returns the tool's dict. A 200 carrying
// {"error":true,...} is a STRUCTURED RESULT (the tool refused the input), so it
// comes back as the map with a nil error — the MCP handler passes it through
// verbatim. Only transport failures and non-200 statuses are errors.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/"+tool, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) do(req *http.Request) (map[string]any, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hailo sidecar unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hailo sidecar %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("hailo sidecar returned non-JSON: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Failing sidecar tests** (`sidecar_test.go`)

```go
package hailoclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureNoopWhenHealthy(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	var spawned int32
	s := NewSidecar(New(srv.URL, time.Second), func() error { atomic.AddInt32(&spawned, 1); return nil }, time.Second)
	if err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spawned != 0 {
		t.Fatal("spawned although already healthy")
	}
}

func TestEnsureSpawnsThenWaitsForHealth(t *testing.T) {
	var up int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&up) == 0 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"enabled":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	spawn := func() error { go func() { time.Sleep(150 * time.Millisecond); atomic.StoreInt32(&up, 1) }(); return nil }
	s := NewSidecar(New(srv.URL, time.Second), spawn, 3*time.Second)
	if err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRefusesWithoutSpawn(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), nil, time.Second)
	if err := s.Ensure(context.Background()); err == nil || !errors.Is(err, ErrNoSidecarCmd) {
		t.Fatalf("want ErrNoSidecarCmd, got %v", err)
	}
}

func TestEnsureSpawnFailureIsLoud(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), func() error { return errors.New("boom") }, time.Second)
	if err := s.Ensure(context.Background()); err == nil {
		t.Fatal("spawn error must propagate")
	}
}

func TestEnsureTimesOutIfNeverHealthy(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), func() error { return nil }, 600*time.Millisecond)
	if err := s.Ensure(context.Background()); err == nil {
		t.Fatal("a sidecar that never answers must time out, not hang")
	}
}
```

- [ ] **Step 5: Implement `sidecar.go`**

```go
package hailoclient

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// ErrNoSidecarCmd: the sidecar is down and this box has no way to start it.
var ErrNoSidecarCmd = errors.New("hailo sidecar not running and no hailo_sidecar_cmd configured")

// Sidecar starts the HTTP sidecar ON DEMAND (operator decision 2026-08-22: no
// scheduler, no always-on service — the sidecar self-exits idle, the harness
// brings it back when needed). Ensure is the single entry point every NPU tool
// calls first; concurrent callers share one spawn.
type Sidecar struct {
	c            *Client
	spawn        func() error
	startTimeout time.Duration
	mu           sync.Mutex
}

// NewSidecar wires a client to a spawn function (nil = cannot spawn).
func NewSidecar(c *Client, spawn func() error, startTimeout time.Duration) *Sidecar {
	return &Sidecar{c: c, spawn: spawn, startTimeout: startTimeout}
}

// Ensure returns nil once /health answers. Down + spawnable → spawn once and
// poll until startTimeout; down + not spawnable → ErrNoSidecarCmd.
func (s *Sidecar) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.c.Health(ctx); err == nil {
		return nil
	}
	if s.spawn == nil {
		return ErrNoSidecarCmd
	}
	if err := s.spawn(); err != nil {
		return fmt.Errorf("starting hailo sidecar: %w", err)
	}
	deadline := time.Now().Add(s.startTimeout)
	for time.Now().Before(deadline) {
		if _, err := s.c.Health(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("hailo sidecar did not become healthy within %s", s.startTimeout)
}

// SpawnCmd launches the configured launcher DETACHED (Start, not Run — the
// sidecar outlives this call and exits on its own idle timer) with no console
// window. The idle window is passed through so config is the single source.
func SpawnCmd(cmdPath string, idleSec int) func() error {
	return func() error {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd.exe", "/c", cmdPath, "--idle-sec", strconv.Itoa(idleSec))
			hideWindow(cmd)
		} else {
			cmd = exec.Command("sh", "-c", cmdPath+" --idle-sec "+strconv.Itoa(idleSec))
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait() // reap; never block the caller
		return nil
	}
}
```
Plus two tiny files: `sidecar_windows.go` (`//go:build windows`) with `func hideWindow(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }` and `sidecar_other.go` (`//go:build !windows`) with `func hideWindow(*exec.Cmd) {}`.

- [ ] **Step 6: Run to verify pass** — `go test ./internal/hailoclient/ -count=1 -race` → PASS (10 tests). `go vet ./internal/hailoclient/` clean.

- [ ] **Step 7: Commit** — `git add internal/hailoclient && git commit -m "hailoclient: loopback client + on-demand sidecar (spawn once, poll health, idle self-exit)"`

---

### Task 8: MCP surface — gated NPU tools, status block, OCR engine switch

**Files:**
- Modify: `internal/mcpserver/mcpserver.go` (`Server` struct; `buildServer` after the `agent_delegate` block; `handleStatus` after `remote`; `handleOCR`; new handlers at end)
- Test: `internal/mcpserver/hailo_test.go`

**Interfaces:**
- Consumes: `config.HasAccelerator`, `hailoclient.{New,NewSidecar,SpawnCmd}`.
- Produces tools (registered ONLY when `cfg.HasAccelerator("hailo-8l")`): `offload_face_detect{image_path}`, `offload_face_embed{image_path,max_faces}`, `offload_object_detect{image_path,score_threshold}`, `offload_person_embed{image_path}`, `offload_depth{image_path,out_path}`, `offload_enhance_low_light{image_path,out_path}`, `offload_image_embed{image_path}`. Each returns the sidecar dict verbatim, or `{"deferred":true,"reason":…}` on transport/spawn failure. `offload_ocr` gains optional `engine: "gpu"|"npu"` (default gpu). `offload_status` gains `accelerators: {"hailo-8l": {endpoint, sidecar_cmd_configured, health|health_error}}` (only when the accelerator is listed — probe with a 2 s budget; never spawn from status).

- [ ] **Step 1: Failing tests** (`hailo_test.go`)

```go
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

var hailoTools = []string{"offload_face_detect", "offload_face_embed", "offload_object_detect",
	"offload_person_embed", "offload_depth", "offload_enhance_low_light", "offload_image_embed"}

func TestHailoToolsRegistrationGatedOnAccelerator(t *testing.T) {
	off := listTools(t, config.Default())
	for _, tool := range off {
		for _, h := range hailoTools {
			if tool.Name == h {
				t.Fatalf("%s advertised with no accelerator", h)
			}
		}
	}
	cfgOn := config.Default()
	cfgOn.Accelerators = []string{"hailo-8l"}
	on := listTools(t, cfgOn)
	var stripped []*mcp.Tool
	found := map[string]bool{}
	for _, tool := range on {
		isHailo := false
		for _, h := range hailoTools {
			if tool.Name == h {
				found[h] = true
				isHailo = true
			}
		}
		if !isHailo {
			stripped = append(stripped, tool)
		}
	}
	if len(found) != len(hailoTools) {
		t.Fatalf("expected all %d NPU tools, found %v", len(hailoTools), found)
	}
	offJSON, _ := json.Marshal(off)
	strippedJSON, _ := json.Marshal(stripped)
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("the accelerator changed the tool list beyond adding its own tools (offload_ocr's schema must only GAIN an optional field — check it is identical when the accelerator is absent)")
	}
}

func fakeHailo(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true,"hefs_missing":[]}`)) })
	mux.HandleFunc("/v1/face_embed", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"faces":[{"x":1,"y":2,"w":3,"h":4,"score":0.9,"embedding":[0.5,0.5]}],"count":1}`))
	})
	mux.HandleFunc("/v1/ocr", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"text":"NPU READ THIS","char_count":13,"boxes":[]}`)) })
	return httptest.NewServer(mux)
}

func hailoServer(t *testing.T, endpoint string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Accelerators = []string{"hailo-8l"}
	cfg.HailoEndpoint = endpoint
	return New(pipeline.New(cfg, nil, nil, nil))
}

func callText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*mcp.TextContent).Text
}

func TestFaceEmbedPassesSidecarResultThrough(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, err := s.handleHailoTool("face_embed")(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{Arguments: json.RawMessage(`{"image_path":"a.jpg"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	out := callText(t, res)
	if !strings.Contains(out, `"count":1`) || !strings.Contains(out, `"embedding":[0.5,0.5]`) {
		t.Fatalf("result not passed through: %s", out)
	}
}

func TestNPUToolDefersWhenSidecarDownAndUnspawnable(t *testing.T) {
	s := hailoServer(t, "http://127.0.0.1:1")
	res, _ := s.handleHailoTool("face_embed")(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{Arguments: json.RawMessage(`{"image_path":"a.jpg"}`)}})
	out := callText(t, res)
	if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "hailo_sidecar_cmd") {
		t.Fatalf("want a defer naming the missing sidecar cmd, got %s", out)
	}
}

func TestOCREngineNPURoutesToSidecar(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, _ := s.handleOCR(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{Arguments: json.RawMessage(`{"image":"a.jpg","engine":"npu"}`)}})
	if out := callText(t, res); !strings.Contains(out, "NPU READ THIS") {
		t.Fatalf("engine:npu did not reach the sidecar: %s", out)
	}
}

func TestOCREngineNPUWithoutAcceleratorDefers(t *testing.T) {
	s := New(pipeline.New(config.Default(), nil, nil, nil))
	res, _ := s.handleOCR(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{Arguments: json.RawMessage(`{"image":"a.jpg","engine":"npu"}`)}})
	if out := callText(t, res); !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "no hailo-8l") {
		t.Fatalf("engine:npu on a box without the accelerator must defer plainly, got %s", out)
	}
}

func TestStatusReportsAcceleratorHealth(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, _ := s.handleStatus(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{}})
	out := callText(t, res)
	if !strings.Contains(out, `"accelerators"`) || !strings.Contains(out, `"hailo-8l"`) || !strings.Contains(out, `"enabled":true`) {
		t.Fatalf("status lacks the accelerator block: %s", out)
	}
	plain := New(pipeline.New(config.Default(), nil, nil, nil))
	res2, _ := plain.handleStatus(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParams{}})
	if strings.Contains(callText(t, res2), `"accelerators"`) {
		t.Fatal("a box with no accelerator must not grow an accelerators block")
	}
}
```
(`handleStatus` probes llama-swap at `cfg.Endpoint`; on a closed default port it records `served_probe_error` and continues — acceptable in tests, matches existing status tests.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/mcpserver/ -run 'Hailo|NPU|OCREngine|StatusReportsAccelerator' -count=1` → `undefined: handleHailoTool`.

- [ ] **Step 3: Implement**

`Server` struct — add `hailo *hailoclient.Sidecar` and `hailoOnce sync.Once`. Add:

```go
// hailoSidecar lazily builds the accelerator lane from config: one client, one
// spawn function (nil when hailo_sidecar_cmd is unset), one Sidecar shared by
// every NPU tool so concurrent first calls share a single spawn.
func (s *Server) hailoSidecar() *hailoclient.Sidecar {
	s.hailoOnce.Do(func() {
		cfg := s.p.Cfg()
		timeout := time.Duration(cfg.HailoTimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		var spawn func() error
		if cfg.HailoSidecarCmd != "" {
			spawn = hailoclient.SpawnCmd(cfg.HailoSidecarCmd, cfg.HailoIdleSec)
		}
		s.hailo = hailoclient.NewSidecar(hailoclient.New(cfg.HailoEndpoint, timeout), spawn, 45*time.Second)
	})
	return s.hailo
}

// hailoCall is the one path every NPU tool takes: ensure the sidecar, call the
// tool, pass the dict through. Transport/spawn failures become defers (the
// caller does the work another way); the sidecar's own structured refusals
// ({"error":true,"kind":...}) pass through untouched — they are results.
func (s *Server) hailoCall(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	sc := s.hailoSidecar()
	if err := sc.Ensure(ctx); err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": "hailo-8l: " + err.Error()})
	}
	out, err := sc.Client().Call(ctx, tool, args)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": "hailo-8l: " + err.Error()})
	}
	return jsonResult(out)
}

// handleHailoTool adapts one sidecar tool to an MCP handler. The MCP argument
// names are the sidecar's keyword names, so the JSON passes through unchanged.
func (s *Server) handleHailoTool(tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in map[string]any
		if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
			return bad, nil
		}
		if in["image_path"] == nil || in["image_path"] == "" {
			return jsonResult(map[string]any{"deferred": true, "reason": "empty image_path"})
		}
		return s.hailoCall(ctx, tool, in)
	}
}
```
(Add `func (s *Sidecar) Client() *Client { return s.c }` to `hailoclient/sidecar.go`. Confirm the handler type name the go-sdk exports — it is whatever `srv.AddTool`'s second parameter type is; use that exact type.)

In `buildServer`, after the `agent_delegate` block and before `return srv`:

```go
	// Accelerator tools (ADR 0024): registered ONLY when the box lists the
	// device, so tools/list is byte-identical without it — the same pin as
	// agent_delegate. Each maps 1:1 to a sidecar tool; ownership is exclusive
	// (the GPU VLM never serves these; see docs/systems/accelerators.md).
	if s.p != nil && s.p.Cfg().HasAccelerator("hailo-8l") {
		type npuTool struct{ name, sidecar, desc, schema string }
		img := `"image_path":{"type":"string","description":"local image file path (JPEG/PNG)"}`
		for _, t := range []npuTool{
			{"offload_face_detect", "face_detect", "Detect faces in an image on the LOCAL Hailo-8L NPU (free, on-box, ~300 FPS). Returns {faces:[{x,y,w,h,score,kps}],count}; kps = 5 landmarks (eyes, nose, mouth corners) in image pixels.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
			{"offload_face_embed", "face_embed", "Face IDENTITY vectors on the LOCAL Hailo-8L NPU: every face -> a 512-d ArcFace embedding. Cosine similarity between two is the identity score (same person ~0.5+, different ~0.3-). Use to cluster who appears where across a project, no cloud. Returns {faces:[{x,y,w,h,score,kps,embedding}],count}.", `{"type":"object","properties":{` + img + `,"max_faces":{"type":"integer","description":"strongest-score faces to embed (default 16)"}},"required":["image_path"]}`},
			{"offload_object_detect", "object_detect", "Detect the 80 COCO object classes (person, car, dog, laptop, ...) on the LOCAL Hailo-8L NPU (YOLOv8s, on-chip NMS). Returns {objects:[{label,class_id,x,y,w,h,score}],count} sorted by score.", `{"type":"object","properties":{` + img + `,"score_threshold":{"type":"number","description":"minimum score (default 0.3)"}},"required":["image_path"]}`},
			{"offload_person_embed", "person_embed", "Person RE-IDENTIFICATION vectors on the LOCAL Hailo-8L NPU (YOLOv8s person boxes -> OSNet 512-d). Works with NO visible face (clothing/body) — tracks the same person across shots. Returns {people:[{x,y,w,h,score,embedding}],count}.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
			{"offload_depth", "depth", "Preview-grade relative depth map on the LOCAL Hailo-8L NPU (Depth-Anything-V2, 224 px). Writes an 8-bit PNG (bright = near). Returns {depth_path,min,max,mean}. Shot analysis / parallax previews, not a production depth pass.", `{"type":"object","properties":{` + img + `,"out_path":{"type":"string","description":"output PNG (default: next to the input as <name>.depth.png)"}},"required":["image_path"]}`},
			{"offload_enhance_low_light", "enhance_low_light", "Brighten an under-exposed frame on the LOCAL Hailo-8L NPU (Zero-DCE) at the original resolution. Returns {enhanced_path,width,height}. Preview-grade.", `{"type":"object","properties":{` + img + `,"out_path":{"type":"string","description":"output PNG (default: <name>.enhanced.png)"}},"required":["image_path"]}`},
			{"offload_image_embed", "embed", "512-d IMAGE embedding on the LOCAL Hailo-8L NPU (TinyCLIP ViT-61M) for similarity search / clustering of frames and thumbnails. Returns {embedding,dim}.", `{"type":"object","properties":{` + img + `},"required":["image_path"]}`},
		} {
			srv.AddTool(&mcp.Tool{Name: t.name, Description: t.desc, InputSchema: json.RawMessage(t.schema)}, s.handleHailoTool(t.sidecar))
		}
	}
```

`handleOCR` becomes:

```go
func (s *Server) handleOCR(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image  string `json:"image"`
		Engine string `json:"engine"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// OCR ownership (2026-08-22): GPU VLM is primary; the NPU's PaddleOCR is the
	// caller's EXPLICIT fast-batch path, never an automatic fallback — the two
	// read stylised text differently and a silent switch would change results.
	if in.Engine == "npu" {
		if !s.p.Cfg().HasAccelerator("hailo-8l") {
			return jsonResult(map[string]any{"deferred": true, "reason": "engine:npu requested but this box lists no hailo-8l accelerator"})
		}
		return s.hailoCall(ctx, "ocr", map[string]any{"image_path": in.Image})
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskOCR, Image: in.Image}))
}
```
and the `offload_ocr` schema gains `"engine":{"type":"string","enum":["gpu","npu"],"description":"gpu (default): the local vision model; npu: the Hailo-8L PaddleOCR path when this box has the accelerator (fast batch transcription)"}`. **Important for the byte-identical pin:** the schema string is static, so add `engine` unconditionally — it is documentation of an option that defers cleanly when absent; the pin test compares the accelerator-off list to the accelerator-on list minus the NPU tools, and both carry the same `offload_ocr` schema.

In `handleStatus`, after the `remote` map:

```go
	// Accelerators (ADR 0024): reported only when listed; a quick health probe
	// that NEVER spawns the sidecar — status must stay side-effect free.
	var accel map[string]any
	if cfg.HasAccelerator("hailo-8l") {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		entry := map[string]any{
			"endpoint":              cfg.HailoEndpoint,
			"sidecar_cmd_configured": cfg.HailoSidecarCmd != "",
			"owns":                  []string{"face_detect", "face_embed", "object_detect", "person_embed", "depth", "enhance_low_light", "image_embed"},
			"note":                  "on-demand loopback sidecar; the first NPU call starts it (cold ~2 s + HEF load), it exits itself after hailo_idle_sec idle",
		}
		if h, err := hailoclient.New(cfg.HailoEndpoint, 2*time.Second).Health(pctx); err != nil {
			entry["health_error"] = err.Error() + " (not running — normal between uses)"
		} else {
			entry["health"] = h
		}
		accel = map[string]any{"hailo-8l": entry}
	}
```
and include `"accelerators": accel` in the returned payload ONLY when `accel != nil` (build the payload map, then `if accel != nil { payload["accelerators"] = accel }`). Update the `offload_status` tool description to mention `accelerators:{...}` when present, and `local.note` to say "every offload_* tool except offload_nim runs on LOCAL models — the GPU roster or a listed accelerator".

- [ ] **Step 4: Run to verify pass** — `go test ./internal/mcpserver/ -count=1 -race` → PASS, including the pre-existing `TestAgentDelegateRegistrationGated` (the `offload_ocr` schema change is identical in both arms).

- [ ] **Step 5: Commit** — `git add internal/mcpserver internal/hailoclient && git commit -m "mcpserver: 7 NPU tools gated on the hailo-8l accelerator, status block, offload_ocr engine:npu"`

---

### Task 9: Docs, ADR, version — ship the harness PR

**Files:**
- Create: `docs/systems/accelerators.md`, `docs/architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md`
- Modify: `AGENTS.md` (systems list), `README.md` (tool list), `setup/SETUP-AGENT.md` (HAILO_HOME / OFFLOAD_ACCELERATORS), `CHANGELOG.md`, `VERSION`, `main.go:55`, `.printing-press.json:4`

- [ ] **Step 1: ADR 0024** (follow the exact header shape of `0023-agent-lane-tailnet-auth-and-locality.md` — title, Status: Accepted, Date 2026-08-22, Context / Decision / Consequences):

Context: the harness classified a box into exactly one `profile`; a second device had no representation, and vision routed only to llama-swap by model id. Decision: accelerators are ADDITIVE — `profile` stays the one GPU tier id; `accelerators: []` lists devices beside it (installed.json, Verdict, config); each accelerator declares the capabilities it OWNS exclusively; its runtime is an on-demand loopback sidecar the harness spawns and that exits itself idle; ownership when both present: NPU = structured vision outputs (boxes/vectors/maps), GPU = language about images, OCR GPU-primary + explicit `engine:npu`. Rejected: composite ids (every tier-id consumer breaks), a second scalar (hard-codes one device), always-on service (scheduler on a clean box). Consequences: tools/list grows only on boxes with the device; tier docs untouched; the matrix gains an Accelerators sheet; concurrency is serialised by the one-process sidecar.

- [ ] **Step 2: `docs/systems/accelerators.md`** — sections: What an accelerator is (ADR 0024) · Detection (`hailortcli scan` + `identify` → `HAILO8L`; Go `hwdetect.DetectAccelerators`, PS `Get-Accelerators`, override `OFFLOAD_ACCELERATORS`) · Seeding (`profiles.json accelerators.<id>.config_seed`, `tierseed.ResolveAccelerators`, `__HAILO_HOME__`, installer env `HAILO_HOME`) · Runtime (sidecar contract verbatim from Task 1; `internal/hailoclient`; spawn/idle; port 18813 loopback only) · Tools (the 7 + `offload_ocr engine:npu`) · Ownership table (from the decision) · Status (`offload_status.accelerators`) · Verifying on a box (the Task 10 commands) · Limits (single in-flight inference; no ledger entry in v1 — recorded follow-up; Windows cannot see the device as an NPU and that is irrelevant to this route).

- [ ] **Step 3: Wire the docs** — `AGENTS.md` systems list: add `accelerators.md — NPU/other devices beside the GPU tier (hailo-8l)`. `README.md` tool table: add the 7 tools with one line each + the `offload_ocr engine` note. `setup/SETUP-AGENT.md`: a short "Accelerators" subsection naming `HAILO_HOME`, `OFFLOAD_ACCELERATORS`, and that an EXISTING config.json is never touched (seed the keys by hand on an already-installed box — exact keys listed). `CHANGELOG.md` under `[Unreleased]` → becomes `## [0.81.0] - 2026-08-22` with "### Added — hailo-8l accelerator (ADR 0024)" bullets mirroring the commit messages.

- [ ] **Step 4: Version bump (one commit)** — `VERSION` → `0.81.0`; `main.go:55` → `const version = "0.81.0"`; `.printing-press.json:4` → `"version": "0.81.0"`.

- [ ] **Step 5: The full gate, unpiped**

```bash
go build ./... && go vet ./... && go test -count=1 ./...
go test -run TestDocsLint -count=1 .
powershell -ExecutionPolicy Bypass -File setup\detect.tests.ps1
powershell -ExecutionPolicy Bypass -File setup\tests\install-config-seed.test.ps1
pwsh -File setup/render.tests.ps1
PYTHONUTF8=1 ~/.local/bin/semgrep scan --config=p/default --error --quiet internal/hailoclient internal/mcpserver/mcpserver.go internal/hwdetect internal/tierseed internal/config setup/install.ps1 setup/detect.ps1
```
Expected: all green; semgrep exit 0 (exit 2 = the scanner broke, investigate, never interpret).

- [ ] **Step 6: Commit, push, review, PR** — commit docs+bump; push; dispatch `pr-review-toolkit:code-reviewer` + `silent-failure-hunter` (model sonnet) on the diff against the Intent above, with the PowerShell-specific prompts from clean-ship (1-element array unwrap on `@($accelerators)` paths; `Start-Process` quoting); fix findings; re-review the delta; then in separate calls: `gh api user --jq .login` → `gh pr create --repo dmmdea/offload-harness …` (confirm the public remote's repo name with `git remote -v` first — the local dir is `local-offload-public`) → account check → `gh pr merge --merge --delete-branch`.

---

### Task 10: Deploy to the OptiPlex 7060 and verify end-to-end

**Files:** none in-repo (a capability report may be added under `docs/tiers/reports/` later; accelerators have no tier page by design).

- [ ] **Step 1: Build and ship the harness exe** — on Qube: `go build -o offload-harness.exe .` in the merged `main` checkout; `scp offload-harness.exe dmmde@optiplex7060:'D:/offload-harness/offload-harness.exe'`; verify `& D:\offload-harness\offload-harness.exe --version` → `local-offload 0.81.0`.

- [ ] **Step 2: Pull the Hailo repo on the Dell** — `git -C D:\Dev\Hailo-8L-Analysis-Pipelines pull` → `hailo-http.cmd` present.

- [ ] **Step 3: Seed the existing config (installer never rewrites an existing config.json)** — with Node, as done for the image keys, merge into `C:\Users\dmmde\.local-offload\config.json`:

```json
{"accelerators":["hailo-8l"],"hailo_endpoint":"http://127.0.0.1:18813","hailo_sidecar_cmd":"D:/Dev/Hailo-8L-Analysis-Pipelines/hailo-http.cmd","hailo_timeout_sec":60,"hailo_idle_sec":300}
```
and append `"accelerators": ["hailo-8l"]` to `D:\offload-stack\installed.json` (so `/fleet/health` advertises it). Back both files up first.

- [ ] **Step 4: Verify the three things the done-criteria name**

1. `offload_status` (via the harness MCP in a Dell Claude session, or `& D:\offload-harness\offload-harness.exe status` if the CLI exposes it) → `accelerators.hailo-8l.endpoint` present; first call `health_error` (not running) is expected.
2. Call `offload_face_embed` on a real photo → `{faces:[{…embedding:[512]}],count:N}`; immediately after, `curl http://127.0.0.1:18813/health` → `loaded_networks` non-empty (the NPU ran, not a cache). Run `offload_object_detect` on a street photo → plausible labels.
3. `offload_ocr` with `engine:"npu"` → `{text:…}` from PaddleOCR; without `engine` → the GPU path as before.
4. Wait `hailo_idle_sec` + 10 s → `Get-Process python` no longer lists the sidecar; the next `offload_*` call spawns it again (cold ~2 s). Record both timings.
5. `tools/list` on a box WITHOUT the accelerator (Qube) is unchanged: `claude mcp` tools count identical to before the upgrade.

- [ ] **Step 5: Evidence + records** — evidence file in the scratchpad with the pasted outputs; `docs/systems/accelerators.md` "Verified on" line (commit in a trivial follow-up PR if anything in the doc needs a measured number); clean-ship ledger line; mem0 evidence entry; plan file row C3 → ✅.

---

## Self-Review

**Spec coverage:** (1) schema A → Tasks 3/4/6 (`accelerators` list in Verdict, installed.json, config; `profile` untouched). (2) sidecar + `hailoclient` mirroring `nimclient` → Tasks 1/7. (3) ownership → Task 8 (7 exclusive tools; OCR `engine:npu`; VQA untouched) + ADR/doc in Task 9. Matrix-first → Task 2 precedes all Go. Dell gets both tiers → Task 10. Gaps: none found; ledger accounting for NPU calls is an explicit v1 non-goal recorded in the doc.

**Placeholder scan:** every code step carries the code; the two "follow the shape of X" instructions (ADR header, health-payload insertion point) name the exact file and the exact neighbouring symbol to grep.

**Type consistency:** `HasAccelerator` (T3) used in T8; `Verdict.Accelerators` (T4) consumed by detect.ps1 JSON + T6; `ResolveAccelerators(accs, ids, Options{HailoHome})` (T5) mirrored by `Get-AcceleratorSeed -ProfilesDoc -Ids -HailoHome` (T6); `hailoclient.New/NewSidecar/SpawnCmd/ErrNoSidecarCmd/Sidecar.Client/Ensure` (T7) used in T8; sidecar tool names in T8 (`face_detect…embed`) match `TOOLS` in T1; MCP tool names in the T8 test list match the registration loop.
