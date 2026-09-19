# 02 — Connecting, interpreters, transport, headless, watchdog

## Preconditions (all hard) [measured]
1. **Resolve STUDIO is running** in some interactive session on the box (external scripting is
   Studio-only since 19.1). Free edition: `scriptapp("Resolve")` returns `None` — no crash.
2. **External scripting = Local** in Preferences → System → General (per Windows account that
   runs Resolve). Do not hand-edit `Resolve.conf`; Resolve rewrites it on exit.
3. **A measured CPython.** On 21.0.4 `fusionscript.dll` was bound to an interpreter ABI and the
   compatible version differed per machine on the SAME Resolve build:

| Machine | Works | Crashes 0xC0000005 in `PyInit_fusionscript` |
|---|---|---|
| editing rig (2026-08-23, 21.0.4.5) | CPython 3.12.10 (bundled `python312\`) | 3.11.9, 3.14.7 |
| workstation (2026-08-28, 21.0.4.5) | CPython 3.14.7 (`C:\Program Files\Python314`) | 3.12.10 embeddable |
| **workstation (2026-09-09, 21.1.0.14)** | **3.14.7, bundled `ResolvePython.exe` 3.14.4, uv CPython 3.11.15 — all bind** | 32-bit 3.14 (not a Win32 app); Python 2 dropped |
| any | `fuscript.exe` (Lua or `-l py3`; on 21.1 `-l py3` reports the SYSTEM 3.14.7) | — |

   21.1 ships `C:\Program Files\Blackmagic Design\DaVinci Resolve\ResolvePython\ResolvePython.exe`
   (Python 3.14.4, `import DaVinciResolveScript` with no env vars, no pip) — prefer it as the pin
   for new deploys (`deploy.ps1 -PythonPin`). The per-machine crash class was NOT reproduced on
   21.1 (09 §2); the Dell has not been re-measured on 21.1, so keep `_probe.py` in the deploy gate
   and still scrub `PYTHONHOME`/`PYTHONPATH` from child envs.

4. A **project page must be open** for writes (see "page null" below).

## Minimal connection (Python) [doc]
```python
import os, sys
os.environ.setdefault("RESOLVE_SCRIPT_API", r"C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Developer\Scripting")
os.environ.setdefault("RESOLVE_SCRIPT_LIB", r"C:\Program Files\Blackmagic Design\DaVinci Resolve\fusionscript.dll")
sys.path.append(os.environ["RESOLVE_SCRIPT_API"] + r"\Modules")
import DaVinciResolveScript as dvr
resolve = dvr.scriptapp("Resolve")          # None => not running / not Studio / scripting not Local
pm = resolve.GetProjectManager(); project = pm.GetCurrentProject()
```
On the rig use `from connect import get_resolve, get_project` (`D:\Editing\ResolveTools\connect.py`).
Windows gotcha [community]: do NOT quote the paths when setting these as machine env vars.

Lua zero-python health probe (works even when python tooling is broken) [measured]:
```
"C:\Program Files\Blackmagic Design\DaVinci Resolve\fuscript.exe" -x "r = bmd.scriptapp('Resolve'); print(r and r:GetProductName() or 'not reachable')"
```
`fuscript -l py3 script.py` runs Python under Resolve's own embedded interpreter (3.12.10 on this build).

## Transport facts [measured]
- Resolve's scripting server listens on **TCP 0.0.0.0:15000**; a client under a DIFFERENT Windows
  account (SSH shell as `<user>`) reaches the editor's instance. Cross-user is not a blocker.
  The 21.1 README says Resolve/Fusion scripting listens on port **1144** (IANA) with the return
  connection in 49152..65535; on our Windows boxes the listener has always been 15000, and 21.1
  additionally shows `0.0.0.0:49152` owned by Resolve (not HTTP; the return-channel port) [measured].
- **21.1's "native MCP server" is NOT a listener**: it is `ResolveMCP.exe` (stdio JSON-RPC, spawned by
  the `DaVinciResolve.mcpb` bundle from Claude Desktop) — full findings in 09 §3.
- macOS-only quirk (not ours): the server may bind the LAN IP; `dvr.pinghosts('')` then
  `scriptapp("Resolve", ip)`.
- Every call is a blocking IPC; there is no cancel. A hung call must be abandoned (thread + join
  timeout, or a process-level watchdog that `os._exit`s).

## The modal-stall trap and the watchdog [measured]
Any GUI modal (auto-backup prompt, "media offline", license prompt, a Save dialog) stalls EVERY
API call indefinitely with no error. The bridge runs each command under a watchdog (default 25 s,
`--timeout N`) and exits **3** on trip. Exit 3 = "Resolve busy — back off, retry", never a
statement about project state. Pattern for your own scripts:
```python
import threading
def call(fn, timeout=25):
    box=[None]; err=[None]
    def run():
        try: box[0]=fn()
        except Exception as e: err[0]=e
    t=threading.Thread(target=run, daemon=True); t.start(); t.join(timeout)
    if t.is_alive(): raise TimeoutError("Resolve busy (modal?)")
    if err[0]: raise err[0]
    return box[0]
```
On 21.x `resolve.DisableBackgroundTasksForCurrentResolveSession()` suppresses auto-backup and other
background tasks for the REST of that Resolve session; there is no re-enable — restart Resolve to
get auto-backup back. Tell the editor when you used it.

## `page == null` ⇒ writes silently fail [measured 2026-08-31 Dell, 2026-09-01 workstation]
When Resolve sits on the Project Manager (or a freshly launched "Untitled Project" with no page
open), `GetCurrentPage()` returns None. All READS still answer (status, projects, pool,
timelines, render-presets, even `GetSetting()`), but `CreateProject`, `ImportMedia`,
`CreateEmptyTimeline`, `InsertGeneratorIntoTimeline`, `smoke` all return None/False. Refuted
hypotheses (do not retry them): unsaved changes; thread affinity.
**Exception = `LoadProject(name)`** — it works from page-null and moves the page to a real one
(`deliver` was observed); after that every write works. So: `resolve.cmd load-project "<name>"
--yes`. Loading the already-current project returns None BY DESIGN (Resolve semantics), the
CLI's "no project with that name" message is wrong in that one case.
**`CreateProject(name)` ALSO works from page-null [measured 2026-09-01 workstation, Studio 21.0.4.5, Disk
DB, zero projects]:** `CreateProject("_ref_scratch")` returned the Project, page went None → `cut`,
and every subsequent write (timeline, generator, Text+, markers, render) worked. So a box with
ZERO projects is not a blocker either — create one from script. (The Dell measurement of
`CreateProject` failing on 2026-08-31 was through the CLI on a box whose console session is owned
by another user; re-measure there before treating it as a Resolve rule.) The workstation's Local
Database now contains `_ref_scratch` (empty, disposable) — `load-project _ref_scratch --yes` is
the fastest page-null escape there. `GetProjectListInCurrentFolder()` returned `[]` immediately
after CreateProject and listed the project only after `SaveProject()` + restart.

## Launching Resolve in another user's console session (editing rig) [measured 2026-08-31]
`schtasks /IT /NP` is refused; use the Task Scheduler COM API with an interactive token:
```powershell
$svc = New-Object -ComObject Schedule.Service; $svc.Connect()
$td  = $svc.NewTask(0)
$td.Principal.UserId    = 'editing-rig\<editor>'   # console owner (find via explorer.exe owner, NOT `query user`)
$td.Principal.LogonType = 3     # TASK_LOGON_INTERACTIVE_TOKEN — no password
$td.Principal.RunLevel  = 0
$a = $td.Actions.Create(0); $a.Path = 'C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe'
$svc.GetFolder('\').RegisterTaskDefinition('launch-resolve',$td,6,$null,$null,3)
$svc.GetFolder('\').GetTask('launch-resolve').Run($null)
# Resolve is up in ~5 s. DELETE THE TASK immediately after (house rule: no unattended schedulers).
$svc.GetFolder('\').DeleteTask('launch-resolve',0)
```
Console owner lookup: `Get-CimInstance Win32_Process -Filter "Name='explorer.exe'" | Invoke-CimMethod -MethodName GetOwner`
(`$_.GetOwner()` does not exist on CimInstance — use `Invoke-CimMethod`).
Resolve started this way lands on the Project Manager ⇒ page null ⇒ `load-project` before any write.

## Headless
`Resolve.exe -nogui` [doc]: UI disabled, scripting APIs keep working. `Resolve.exe -rr` [community]:
remote-render worker mode (no GUI, no logged-in session needed; the render-queue "Remote
rendering" workflow uses a shared PostgreSQL DB — not our setup). **`-nogui` MEASURED 2026-09-01 on the workstation (Studio 21.0.4.5, Disk DB, launched from an SSH-less
local PowerShell with `Start-Process Resolve.exe -ArgumentList '-nogui' -WindowStyle Hidden`):**
- Process up with NO window (`MainWindowHandle` 0), ~2.1 GB working set, ready in <45 s; the
  scripting server listens on `0.0.0.0:15000` as usual; log ends `Fusion | Started script server`.
- License: log shows the same `LeManager | Using lic type:1` as a GUI launch — the Studio seat is
  used normally (it is NOT a free extra seat; deactivate rules unchanged).
- `GetCurrentPage()` came up **`media`, not null**, on "Untitled Project" — so headless skips the
  Project Manager trap entirely. `LoadProject("_ref_scratch")` → page `edit`; `OpenPage("deliver")`
  → True.
- Writes all worked: CreateEmptyTimeline, InsertGeneratorIntoTimeline, InsertFusionTitleIntoTimeline,
  Text+ `SetInput`, SetCurrentRenderFormatAndCodec("mp4","H264_NVIDIA"), SetRenderSettings,
  AddRenderJob, StartRendering. 240-frame 1080p24 NVENC render: `TimeTakenToRenderInMs` 5764,
  file 175,902 bytes, ffprobe `h264 1920x1080 24/1 240 frames`. DeleteRenderJob/DeleteTimelines True.
- `Resolve.Quit()` returns None but exits the process cleanly within ~10 s (after `SaveProject()`).
- Log noise to ignore: `SyManager … Failed to create device handle (error 32/5)`, `Keyboard
  identifiers mismatch`, `DeckLink Failed to create instance` — same as GUI.
Conclusion: on a box you own the seat on (workstation), `-nogui` is the preferred autonomous mode: no
modal dialogs can stall the API (rule 3's watchdog stays anyway), no page-null. NOT tested: whether
`-nogui` works when launched from a non-console session (SSH/service) — on the editing rig keep the
interactive COM-task launch until measured, and never launch a second Resolve while the editor's
GUI instance is running (one Resolve per machine).

## Remote workflow over SSH from the workstation [measured]
```
ssh <user>@editing-rig "cmd /c 'D:\Editing\ResolveTools\resolve.cmd status --json'"
```
- Remote shell is Windows PowerShell 5.1; quote-nesting from Git Bash mangles `$_` and inner
  quotes — for anything beyond one flat command, `scp` a `.ps1`/`.py` and run it by path.
- A timed-out ssh kills only the local client; bound remote work with a remote-side watchdog
  (`Start-Process -PassThru` + `Wait-Process -Timeout` + kill) or orphans accumulate.
- `--json` for machine output; keep media paths contiguous in `import` (a flag between paths
  splits the nargs list).
- Slow-link tip: batch several reads in one Python script on the rig (one SSH round trip,
  one Resolve connection) instead of N CLI calls; write results to a JSON file and `scp` it back.
