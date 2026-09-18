---
name: davinci-resolve
description: Use when the user wants to edit, cut, trim, splice, assemble, color grade, title, caption/subtitle, sync audio, add transitions, or render/export a video in DaVinci Resolve — or any task that means operating Resolve: importing footage/media, building or changing a timeline, the media pool, B-roll, markers, multicam, Fusion, Fairlight audio, transcription, or exporting/rendering. Triggers on "edit my video", "cut the intro", "trim this footage", "remove dead air/silence", "make a rough cut", "color grade the footage", "render/export this", "add a title", DaVinci Resolve, Resolve Studio, and YouTube edit work.
---

# DaVinci Resolve

Drive DaVinci Resolve Studio programmatically. **Host-aware — check `hostname` first**;
the live Resolve deployment is per-machine and the layers below are NOT interchangeable.

## Capability matrix (read this before promising an edit)

| Capability | State |
|---|---|
| Reads: status, projects (+attributes), timelines, clips (video+audio, named timeline), selection, media pool (recursive), render presets, export-spec | **VERIFIED live** (2026-08-23, Dell rig, Studio 21.0.4.5) |
| Writes: `import`, `page`, `create-project`, `load-project`, `delete-project`, `create-timeline`, `render`, `smoke` | **VERIFIED WORKING 2026-08-31** on the Dell rig — `smoke --yes --timeout 180` returns SMOKE PASS end-to-end (creates a disposable project, builds a timeline, inserts a generator, RENDERS and verifies the file, deletes the project, restores the editor's project). |
| Spec→timeline build (append-from-spec), Fusion templates | The deployed CLI (`build f759aaa29`, 2026-08-25) DOES expose `build --spec PATH [--fps] [--lut NAME=PATH]` taking an edit-spec 1.0.0 — newer than this table's old "NOT BUILT". Untested, and it rides the same write path that is measured broken above, so treat as unusable until writes work. Still: do NOT hand-roll raw fusionscript writes against a live project |
| **21.1 catalog + sidecar (2026-09-09, workstation)** | **146 commands VERIFIED live on Studio 21.1.0.14** through `resolve_cli.py serve --port 18800`: bins/clips/markers/flags/proxies, tracks, items (incl. 21.1 `add-transition`, `set-item --speed/--fade-in`, `set-item-properties`), multicam create/smart-switch/flatten, colour versions/CDL/LUT/DRX/groups/gallery, Fusion tool scripting (Lock/Undo wrapped), render queue/presets/quick-export/`verify-render`, system presets, `transcription` readback, `validate-dctl`. Branch `feat/resolve-http-sidecar` (PR #5). Reference: `reference/09-resolve-21-1.md` + `10-sidecar-and-cli.md`. **`archive-project` is refused: `ArchiveProject` crashes Resolve 21.1 from a script.** |
| **Printed CLI `davinci-resolve-pp-cli` (2026-09-09)** | **Promoted to the local printing-press library** `%USERPROFILE%\printing-press\library\davinci-resolve` (build with `go build ./cmd/davinci-resolve-pp-cli`; NOT published to the public library — never publish without asking). 147 endpoint commands generated 1:1 from the sidecar catalog + 7 hand-authored history commands (`runs list/show`, `marker search`, `render history`, `color audit`, `fusion titles search`, `timeline log`, `doctor history`). Live dogfood 511/511 against `_ref_scratch`, shipcheck 7/7, scorecard 93/100. Needs the sidecar on `127.0.0.1:18800` (`resolve.cmd serve --port 18800`). Details + fixture recipe: `reference/10-sidecar-and-cli.md` §4–5. |
| Resolve AI via API | Split surface — see `video-pipeline/docs/MASTER-PLAN.md` §2.3. **21.1: `GetTranscription()` returns per-word timing** (transcribe on media/cut/edit/fairlight; synchronous, ~20 s cold). Harness STT (hotwords) still owns karaoke timing; Resolve's transcript is a real cross-check now. `GenerateSpeech` needs the AI Speech Generator Extras pack (returns an error STRING without it) |

## Access layers by host

### Dell editing rig the editing rig — the editing rig (PRIMARY)
> **2026-09-10: upgraded in place to Studio 21.1.0.14 and bridge `c099f222a` deployed** (silent
> install over SSH while Resolve was closed; `deploy.ps1` verify PASS). NOT yet re-verified on
> 21.1 here: interpreter compatibility (`_probe.py` first — the pin is per-machine), the licence
> line after the upgrade, and a read pass; all need Resolve open in the console session.
> The 2026-08-31 measurements below were taken on 21.0.4.5.
>
> **SEAT STATUS 2026-08-31: LICENSED, bridge VERIFIED end-to-end (smoke passes).** There are
> TWO Studio keys: one moved laptop -> workstation, and this rig has its own. It briefly prompted for a
> key on 2026-08-31 and was reactivated ("Activated successfully. 1 activation left"); the log
> line that proves activation state is `LeManager | License Key:` in
> `%APPDATA%\...\DaVinci Resolve\Support\logs\davinci_resolve.log` — read it before theorising.
> Console session is owned by `editing-rig\<editor>` (168-file profile, history to 8/23) —
> that is the account to launch Resolve as, not `<user>`. Reads and writes both work cross-user
> from an SSH shell as `<user>`.
- **CLI:** `D:\Editing\ResolveTools\resolve.cmd` — deployed FROM the repo
  `<dev>\video-pipeline` (`engine/resolve_bridge/`, deploy via `deploy.ps1`).
  The rig copy is a build artifact: never hand-edit it there; `resolve.cmd --version` reports
  the deployed git SHA.
- **Preconditions:** Resolve Studio must be RUNNING in the console session — nothing
  auto-launches on this box. Studio is activated. Reach the box via `ssh <user>@editing-rig`
  (remote shell is Windows PowerShell 5.1, and the session arrives ELEVATED).
- **Launching it yourself (verified 2026-08-31).** You do not have to wait for a human to
  open Resolve. The console session can be owned by a DIFFERENT local account than the one
  you SSH in as, and a GUI app must be started inside that session:
  - `schtasks /create ... /IT /NP` is **refused** — "/IT switch cannot be used with /NP".
    Use the Task Scheduler **COM API** instead, which takes an interactive token with no
    stored password:
    ```powershell
    $svc = New-Object -ComObject Schedule.Service; $svc.Connect()
    $td  = $svc.NewTask(0)
    $td.Principal.UserId    = '<HOST>\<console user>'   # who owns session 1
    $td.Principal.LogonType = 3     # TASK_LOGON_INTERACTIVE_TOKEN - no password
    $td.Principal.RunLevel  = 0     # Resolve does not need admin
    $a = $td.Actions.Create(0); $a.Path = 'C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe'
    $svc.GetFolder('\').RegisterTaskDefinition($TN,$td,6,$null,$null,3)   # 6=CREATE_OR_UPDATE
    $svc.GetFolder('\').GetTask($TN).Run($null)
    ```
    Measured: Resolve up in ~5 s, in session 1. **Delete the task immediately after** — it is
    a launcher, never a scheduler (house rule: no unattended schedulers).
  - Find the console owner with `Get-CimInstance Win32_Process -Filter "Name='explorer.exe'"`
    + `GetOwner`; `query user` shows only the display name, which may contain a space and is
    NOT necessarily the account name.
- **Cross-user is NOT the blocker.** Resolve's scripting server listens on **TCP 0.0.0.0:15000**,
  so a bridge running as a different account can reach it at the socket level.
- **`status.page == null` MEANS WRITES WILL SILENTLY FAIL. Check it before every write.**
  Measured 2026-08-31: with Resolve sitting on the **Project Manager** (no project page open),
  every READ answers normally — `status` even reports the project NAME, `projects`, `pool`,
  `timelines` and `render-presets` all return correct data — while writes return `None`:
  `create-project`, `import`, `create-timeline` and `smoke` all failed. The single tell is
  `"page": null`. Open any project and all of it works immediately (smoke then passes
  end-to-end, render included).
  Two hypotheses were tested and REFUTED on the way, so do not spend time on them: it is NOT
  unsaved changes (LoadProject succeeds with no save), and it is NOT thread affinity
  (CreateProject succeeds from a daemon worker thread as well as the main thread).
- **`load-project` is the EXCEPTION, and it is your way out of the Project Manager.**
  Re-measured 2026-08-31 after launching Resolve fresh (page `null`, current project
  "Untitled Project"): `load-project "New Project 1" --yes` **succeeded** and moved
  `status.page` to `deliver`, after which every write worked. So you do not need a human to
  click a project — do it over the CLI. An earlier note here said `load-project` also fails
  when the page is null; that was the *already-open-project* case below being misread as a
  page-null symptom.
- **`load-project <the already-open project>` returns `None` BY DESIGN** — that is Resolve's
  semantics for loading the current project, not an error. The CLI's message ("no project with
  that name in the current folder") is wrong in that case; ignore it if `status.project`
  already names your target.
- **`smoke` disables Resolve's background tasks and CANNOT re-enable them** ("no API for it").
  Auto-backup stays off until Resolve is restarted — tell the editor, or restart it yourself.
- Once Studio IS licensed, the scripting preference still has to be **Local**:
  **Preferences → System → General → External scripting using = Local** (one-time, per account).
  Do not hand-write `Resolve.conf` — the format is undocumented and Resolve rewrites it on exit.
- **Invocation (golden sample, real output 2026-08-23):**
  ```
  ssh <user>@editing-rig "cmd /c 'D:\Editing\ResolveTools\resolve.cmd status --json'"
  {"product": "DaVinci Resolve Studio", "version": "21.0.4.5", "page": "cut",
   "project": "Untitled Project", "timeline": null, "cli": "resolve_cli build ..."}
  ```
  `--json` for machine output; `--yes` required by every mutating command; flags accepted
  before or after the subcommand.
- **Watchdog:** every command times out (default 25s, `--timeout N`) because the API stalls
  behind ANY Resolve GUI modal (auto-backup classic). Exit **3** = "Resolve busy — back off,
  retry"; it is NOT a truth about the project. Exit 1 + `ERROR:` on stderr = connect/state
  errors (message says which). Interpreter is the BUNDLED CPython 3.12.10 embeddable
  (`python312\`) — 3.11/3.14 crash 0xC0000005 on this Resolve build; never repoint.
- **Footage root:** `D:\Editing\` (Projects/Footage/Exports/Assets).

### laptop 15P the laptop — laptop (DORMANT: no Studio seat since 2026-08-28)
- Resolve updated to **21.0.4.5** but its license seat moved to the workstation — Resolve cannot
  launch there, so the whole scripting layer is inert until a seat returns. When it does:
  run `_probe.py` FIRST (its MCP venv is python 3.14.6; compatibility is per-machine).
- MCP server `davinci-resolve` (samuelgursky **v2.103.1**; rollback tag `backup/pre-2.103.1`)
  + scripting bridge at `%USERPROFILE%\resolve-claude\`. Reinstall deps ONLY with
  `mcp[cli]>=1.29,<2` (mcp 2.0 breaks it).
- Kernel docs (fusion/render/audio/media-pool/…) are vendored in the repo at `docs/reference/`.
- Footage: `<cloud-drive>\YouTube\MyTools Auto Reviews`.

### workstation — Studio **21.1.0.14** (upgraded 2026-09-08; was 21.0.4.5, activated locally 2026-08-27/28)
> 21.1 facts (interpreters, native MCP, new API, crash list) live in `reference/09-resolve-21-1.md`.
> Any 64-bit CPython 3.11–3.14 binds on 21.1 (system 3.14.7, bundled `ResolvePython.exe` 3.14.4,
> uv 3.11.15 all measured); the interpreter notes below describe 21.0.4.
> **HOLDS THE STUDIO SEAT as of 2026-08-31.** Profile created 8/27 21:59, bridge deployed
> 8/27 21:49. To move the seat back to the editing rig, DEACTIVATE HERE FIRST, then
> activate on the editing rig. Resolve is not usually running here, so check before assuming
> the seat is in use.
- **CLI:** `D:\Editing\ResolveTools\resolve.cmd` — deployed from the same repo via
  `deploy.ps1 -Local -Dest 'D:/Editing/ResolveTools' -PythonPin 'C:\Program Files\Python314\python.exe'`.
  Interpreter is PIN-based: on the workstation fusionscript works under machine **3.14.7** and
  CRASHES under the 3.12.10 embeddable — the exact inverse of the Dell, same Resolve build.
  Compatibility is PER-MACHINE: measure with `_probe.py`, never copy another box's pin.
- **Preconditions:** Resolve must be running in the local session (launch
  `C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe`); external scripting
  verified live 2026-08-28. Both GPUs (5070 Ti + 5060 Ti) available — coordinate with
  ComfyUI/llama-swap loads before heavy renders.
- **Headless works here (measured 2026-09-01):** `Resolve.exe -nogui` comes up with no window,
  page `media` (never null), same Studio seat, writes + NVENC render verified end-to-end
  (`reference/02-connection-transport.md` → Headless). Prefer it for autonomous runs on the workstation.
  One Resolve per machine — never start it while a GUI instance is running.
- **Page-null escape on the workstation:** `CreateProject` works from page-null here, and a disposable
  empty project `_ref_scratch` exists in the Local Database — `load-project _ref_scratch --yes`.
- Installer kept for fleet reuse: `D:\Temp\DaVinci_Resolve_Studio_21.0.4_Windows.zip`.

## How to work
1. **Read before acting.** Lead with reads — `status`, `timelines`, `clips`, `selection`,
   `pool` — to see current state. `selection` shows what the editor has selected right now
   (note: selecting N video clips returns linked audio items too; filter by the `type` field).
2. **Never disturb the editor's session.** Reads are safe while she works; `--timeline NAME`
   reads a named timeline WITHOUT switching her GUI. Anything mutating (--yes commands, project
   creation, renders) needs an idle/supervised window — state intent and get a yes first.
3. **Verify after.** Re-read (`status`, `clips`, `export-spec`) and report what actually changed.
4. **Producing an edit:** generate the edit-spec JSON against
   `video-pipeline/shared/schema/edit-spec.schema.json` (v1.0.0) — `export-spec` shows
   the shape from a real timeline. The spec→timeline write path is Phase 0's remaining half.

## Boundaries (one job → one owner)
- This skill = **operate Resolve to perform edits**.
- Reviewing a finished exported video file for correction notes → **video-editor-review**.
- Writing prompts for AI video *generators* (Seedance / Kling / Veo / Runway) → **visual-video**.

## Reference library (load on demand — NOT auto-loaded, keeps this file lean)
`reference/README.md` in this skill folder indexes a complete autonomous-editing guide built
2026-09-01 (official README digest, bridge internals, kernels, Fusion scripting, live dumps,
research). Read the file that matches the task before researching anything:
`01-hosts-hardware` (boxes, GPU/VRAM, paths, licenses) · `02-connection-transport` (interpreter
matrix, watchdog, page-null, launching Resolve in the editor's session, headless) ·
`03-cli-reference` (every `resolve.cmd` verb, exit codes, recovery) · `04-api-reference` (condensed
API catalog, constants, settings keys) · `05-editing-playbook` (editor vocabulary → API ops,
frame math, what cannot be done + workaround) · `06-render-delivery` (render sequence, codec ids,
YouTube/Shorts numbers, NVENC, verify-by-file) · `07-fusion-titles-color-audio` (Text+, keyframes,
LUT/CDL/DRX, audio limits, captions) · `08-failure-modes` (40 known traps with fixes) ·
`live-dump-2026-09-01.json` (all 158 project-setting keys, render formats/codec ids, presets).
Facts there are tagged [measured]/[doc]/[community]/[inferred]; append new measurements with a date.

## Pointers
- Repo (source of truth, masterplan, schema, research): `<dev>\video-pipeline`
- Bridge operational README (interpreter matrix, deploy, fuscript escape hatch):
  `engine/resolve_bridge/README.md` (repo) = `D:\Editing\ResolveTools\README.md` (rig)
- Authoritative API doc (rig): `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Developer\Scripting\README.txt`
