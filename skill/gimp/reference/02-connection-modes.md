# 02 — Connection modes: console batch, GUI, MCP, Script-Fu server

Pick the mode from the task. Default for autonomous work is **A (console batch)**; use **C
(MCP)** only when a human is looking at a GIMP window or you need a long-lived session with
visual snapshots. Timings measured on the workstation 2026-09-01.

| Mode | Process | Sees GUI images? | Start | When |
|---|---|---|---|---|
| A. Console batch | `gimp-console-3.2.exe -i -d --batch-interpreter … -b … --quit` | no | 3.4 s warm / 8.1 s cold [measured] | every unattended edit/export |
| B. GUI + batch at launch | `gimp-3.2.exe -s --batch-interpreter … -b …` (window stays) | yes (its own) | 5.6 s to window (`-d`) [measured] | start the MCP plug-in in a visible GIMP |
| C. MCP (gimp-mcp) | plug-in socket `127.0.0.1:9877` inside A or B + stdio server | yes, of the host it runs in | +13.6 s GUI→listening [measured] | interactive agent loop, snapshots, a human watching |
| D. Script-Fu server | `(plug-in-script-fu-server 1 "127.0.0.1" 10008 "log")` | its host's | — [doc, not measured] | legacy Scheme clients only |

## A. Console batch (the workhorse) [measured]
```
"C:\Program Files\GIMP 3\bin\gimp-console-3.2.exe" -i -d --batch-interpreter python-fu-eval ^
   -b "exec(open(r'D:\path\script.py').read())" --quit
```
- Fresh process per run; nothing persists between runs. One image pipeline per invocation.
- `-i` is implied by the console binary but harmless; `-d` skips brushes/gradients/patterns
  (faster; prints harmless `INFO: argument 'gradient' … ignored` lines). Never `-f` (no fonts).
- `Gimp.Display.new(image)` → `TypeError: constructor returned NULL` in console mode (no
  display); everything else — load, edit, filters, text, export — works headless.
- Runs fine while a GUI GIMP is open; it is a separate instance with its own PDB and image
  list (`len(Gimp.get_images()) == 0` beside a GUI with an image open).
- Exit codes: 0 ok · 2 python exception · 64 no interpreter · 69 bad interpreter · 70
  Script-Fu error · never exits without `--quit` (wrap with `timeout`). Details: 03.
- Full CLI grammar and quoting: `03-cli-batch.md`.

## B. GUI instance rules [measured]
- `gimp-3.2.exe -s -d` → main window titled "GNU Image Manipulation Program" in 5.6 s.
- **Single-instance:** a second `gimp-3.2.exe <file>` exits after handing the file to the
  running instance (its title became `[probe_in] (imported)-1.0 … 1600x900 - GIMP`).
- **`gimp-3.2.exe -b …` while an instance runs → "Batch commands cannot be run in existing
  instance in Win32", exit 1.** Use `-n/--new-instance` for a second full instance, or MCP.
- Batch given AT LAUNCH runs inside the GUI process (that is how we start the MCP plug-in).
- From Git Bash you also see "Non-native shell detected, GIMP may behave unexpectedly on Unix
  shells in Windows." — cosmetic here; PowerShell `Start-Process` avoids it.
- Never leave a window open when done: stop the tree (see Teardown).

## C. gimp-mcp lifecycle [measured unless tagged]

### Architecture [doc + measured]
```
Claude Code ──MCP stdio──▶ gimp_mcp_server.py (uv, FastMCP, 79 tools + 2 prompts)
                              │ TCP JSON, 127.0.0.1:9877, one connection per call
                              ▼
                         gimp-mcp-plugin.py  = GIMP plug-in process (bin\python.exe),
                              │ PDB procs plug-in-mcp-server / -check / -restart, menu Tools▸MCP
                              ▼
                         GIMP 3.2.4 host (gimp-3.2.exe or gimp-console-3.2.exe)
```
Raw socket protocol (measured by hand): `{"cmds":["python…", …]}` → `{"status":"success","results":[stdout…]}`;
`{"type":"<tool>","params":{…}}` → `{"status":…,"results":…}` or `{"status":"error","error":…,"traceback":…}`.
Python `exec` context persists across calls inside one plug-in process [doc: protocol file].

### 1. Register the server for a NEW Claude session (on-demand) [measured: script read]
```
powershell -NoProfile -File "$env:USERPROFILE\.claude\mcp-ondemand\mcp-ondemand.ps1" status
powershell -NoProfile -File "$env:USERPROFILE\.claude\mcp-ondemand\mcp-ondemand.ps1" on  gimp-mcp
   # writes mcpServers.gimp-mcp into ~/.claude.json (atomic write); takes effect in NEW sessions only
powershell -NoProfile -File "$env:USERPROFILE\.claude\mcp-ondemand\mcp-ondemand.ps1" off gimp-mcp
   # stashes the definition back into stash.json
```
Cost being avoided (from the script header): gimp-mcp = 24 processes / 504 MB across a
13-session desktop with 0 calls. Keep it off unless a session needs it.

### 2. Start GIMP with the plug-in listening
Headless (verified working — the plug-in does NOT need a GUI):
```
gimp-console-3.2.exe -i -d --batch-interpreter python-fu-eval --quit ^
  -b "p=Gimp.get_pdb().lookup_procedure('plug-in-mcp-server'); c=p.create_config(); c.set_property('run-mode', Gimp.RunMode.NONINTERACTIVE); p.run(c)"
```
(the `-b` blocks forever inside the plug-in's GLib main loop; `--quit` fires only after you kill it).
GUI (a human can watch): same `-b` on `gimp-3.2.exe -s`; listener up after 13.6 s (process tree:
`gimp-3.2.exe` → `gdbus.exe`, `script-fu.exe`, `python.exe` (python-eval), `python.exe` (mcp plug-in)).
Readiness test: `Get-NetTCPConnection -LocalPort 9877 -State Listen`.
Never call `Gimp.get_pdb().run_procedure(...)` — it does not exist (AttributeError).

### 3. Use it
- Headless: `exec` cmds, `get_gimp_info`, `get_image_metadata`, `add_text`, `export_image`,
  `list_fonts` all answered in 10–90 ms. **`new_canvas` fails headless** ("constructor returned
  NULL" from `Gimp.Display.new`) although the image IS created (metadata showed 320×200 with 1
  layer) — every tool that opens a display needs mode B.
- GUI: `new_canvas` (display_opened true), `add_text` (0.53 s), `export_image` (PNG 6303 B,
  verified 320×200 rgb24 72 dpi with ffprobe/Pillow) all worked; `list_fonts(filter="Sans")`
  returned the 453-font list filtered.
- Through the stdio server (`uv run --directory D:/Dev/tools/gimp-mcp gimp_mcp_server.py`):
  initialize 0.9 s, `check_server` → `{connected:true, gimp_version:"3.2.4"}`,
  `get_image_metadata`, `call_api(api_path="exec", args=["python-fu-eval", ["Gimp.version()"]])`
  → `["3.2.4"]`. With no image open tools raise "No images are currently open in GIMP".
- `get_state_snapshot` **works headless through the stdio server** [measured]: image loaded via
  `call_api exec` (`open_image` fails headless on `Display.new` — but the image IS opened, so
  ignore that error or load with exec), then `get_state_snapshot(max_size=256)` → MCP image
  content `image/png` 4430 B = 256×144 rgb24 in 0.34 s; with
  `region={x,y,width,height}, label` → 128×106 crop in 0.65 s; `get_image_bitmap(max_width=200)`
  → image content too. Over the RAW socket the same tool returned `error: 'args'` (different
  message shape) — always go through the server.
- Tool inventory (79): see `live-dump-qube-2026-09-01.json → mcp_server.tools`; categories in
  the upstream README (adjustments, transforms, selections, layers, drawing, text, filters,
  file ops, info, undo/redo, batch/social/icon exports).

### 4. Stop — kill the TREE
```powershell
Get-CimInstance Win32_Process | ? { $_.ExecutablePath -like 'C:\Program Files\GIMP 3\*' } |
  % { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Get-NetTCPConnection -LocalPort 9877 -ErrorAction SilentlyContinue   # expect nothing in Listen
```
Measured: stopping only `gimp-3.2.exe` left 2 processes and the 9877 listener alive; stopping
only `gimp-console-3.2.exe` left the MCP `python.exe` holding 9877 (next start's plug-in then
failed to bind and the stale one answered with a dead host). `gdbus.exe _win32_run_session_bus`
also lingers after clean console exits. `taskkill /F` from Git Bash breaks (`/F` → `F:/`);
use PowerShell or `taskkill //F //IM`.

## D. Script-Fu server [measured 2026-09-01, headless]
Start (named args — positional form works but logs "Calling Plug-In PDB procedures with arguments as an ordered list is deprecated"):
```
gimp-console-3.2.exe -i -d --batch-interpreter plug-in-script-fu-eval --quit ^
  -b "(plug-in-script-fu-server #:run-mode 1 #:ip \"127.0.0.1\" #:port 10008 #:logfile \"C:/w/sfserver.log\")"
```
Listening on 127.0.0.1:10008 within ~10 s; the `-b` blocks like the MCP plug-in. Wire protocol
(measured bytes): request = `b"G" + len(2 bytes big-endian) + script`; response =
`b"G" + error(1 byte: 0 ok / 1 error) + len(2 bytes big-endian) + text`. Examples:
`(gimp-version)` → `G\x00\x00\t("3.2.4")`; `(define (twice x) (* 2 x)) (twice 21)` → `twice42`;
`(no-such-fn 1)` → `G\x01\x00*Error: eval: unbound variable: no-such-fn`. **State persists** across
requests and across reconnects (`(twice 7)` on a new connection → 14) — unlike `-b` snippets.
Several requests may share one connection; the server answers each in order and logs every
request to the logfile. Killing the host crashes `script-fu.exe` ("fatal error: GIMP crashed") —
harmless, but kill the tree (02 §4). The text console (`--batch=-` with Script-Fu) is Unix-only [doc].

## One-GIMP-per-machine, summarized [measured]
- One **GUI** instance per user session (single-instance hand-off).
- Any number of **console** instances may coexist with it and with each other; each is fully
  independent (own PDB, own images, own plug-in processes). Do not run two console jobs that
  write the same output path.
- Port 9877 is one-per-machine: the MCP plug-in binds with SO_REUSEADDR and `listen(1)`; only
  one live GIMP can own it, and an orphaned plug-in blocks the next one.
