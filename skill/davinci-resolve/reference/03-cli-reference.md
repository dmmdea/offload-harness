# 03 — `resolve.cmd` (resolve_cli.py) reference — deployed build f759aaa29 on the rig

> **2026-09-09:** the bridge grew to **146 commands** plus the loopback HTTP sidecar
> (`resolve_cli.py serve --port 18800`) on branch `feat/resolve-http-sidecar` (PR #5). The 17
> verbs below are unchanged; everything else (clips, markers, tracks, items, transitions, colour,
> Fusion, render queue, presets, AI passes, transcription, multicam…) is documented in
> `10-sidecar-and-cli.md`, and the printed `davinci-resolve` CLI wraps the same routes. The
> "promotion triggers" section at the bottom has been met.

Source of truth: `<dev>\video-pipeline\engine\resolve_bridge\resolve_cli.py`
(workstation). Rig copy is a build artifact — never hand-edit; `resolve.cmd --version` prints the SHA.
Deploy: `pwsh engine/resolve_bridge/deploy.ps1` (remote Dell default) or
`deploy.ps1 -Local -Dest 'D:/Editing/ResolveTools' -PythonPin '<python.exe>'` (workstation).
Unit tests: `py -3 -m pytest engine/resolve_bridge/tests -q`. `check_readonly.py` (AST gate)
fails the deploy if a non-write command path calls a mutating API.

Global flags (before or after the subcommand): `--json` · `--yes` (required by every write) ·
`--timeout S` (default 25) · `--version`.

## Exit codes
| code | meaning | what to do |
|---|---|---|
| 0 | ok | — |
| 1 | state/connect error, message on stderr (`ERROR: …`) | read the message; usually "not reachable" (Resolve down / not Studio / scripting not Local / wrong python) or "no project/timeline" |
| 2 | usage | fix the command |
| 3 | **watchdog timeout** ("Resolve busy") | a modal is open or a long op; back off, retry later; NOT a fact about the project. After a trip during `render`/`smoke`, clean up by hand (below) |
| 4 | unexpected exception (bug or unguarded None) | report with the traceback |

`--json` output includes a `warnings` list marking API calls that returned None — "empty or
failed" is not "verified empty".

## Read commands (safe any time, never switch the editor's GUI)
| command | what it returns |
|---|---|
| `status` | product, version, `page` (**check for null**), project, timeline, cli build |
| `projects [--attributes]` | names in the current DB folder; attributes (lastModifiedDate, creationDate, notes, liveCollaborationMode) WITHOUT opening them |
| `timelines` | timelines in the open project |
| `clips [--audio] [--timeline NAME]` | items per track (start/end/duration/name/source range); `--timeline` reads a NAMED timeline read-safely |
| `selection` | the editor's current timeline selection (linked audio items come along — filter by `type`) |
| `pool [--recursive]` | media pool clips (current folder or whole tree) with `File Path` |
| `render-presets` | preset names |
| `render-status` | render queue (`GetRenderJobList`) |
| `export-spec [--timeline NAME]` | serializes a timeline to edit-spec 1.0.0 JSON (`format: "export"`) |

## Write commands (`--yes` mandatory; need `page != null`)
| command | behaviour |
|---|---|
| `import PATH...` | `MediaPool.ImportMedia` into the current pool folder (keep paths contiguous) |
| `page NAME` | `OpenPage` (media/cut/edit/fusion/color/fairlight/deliver) — switches the GUI |
| `create-project NAME` | creates AND loads (switches the GUI) |
| `load-project NAME` | switches the GUI's project; the page-null escape hatch; returns None for the already-open project (by design) |
| `delete-project NAME` | refuses on the currently loaded project (API contract) |
| `create-timeline NAME` | `CreateEmptyTimeline` in the current project |
| `render [--out DIR] [--name STEM]` | renders the CURRENT timeline to mp4/H264 (`SelectAllFrames`, `ExportAudio: False` in this build!) with all guards; `--timeout >= 60` enforced |
| `smoke [--out DIR]` | disposable-project end-to-end proof (`_pp_smoke_<id>` project + timeline + Solid Color generator + 1 s render + verify + delete + restore editor's project); `--timeout >= 90`; disables background tasks for the session |
| `build --spec PATH [--fps 29.97] [--lut NAME=PATH]...` | edit-spec 1.0.0 → sets `timelineFrameRate` FIRST (refuses if the project won't change rate: use an empty project), imports missing `src` (matched by pool `File Path`, case/slash-insensitive), creates the spec's timeline at the spec resolution (`useCustomSettings=1` + width/height + output width/height), grows V/A tracks, appends every clip with pre-resolved `startFrame/endFrame/recordFrame/trackIndex/mediaType`, applies `fill`→`ZoomX/ZoomY`, `grade`→`SetLUT(1, lut)` via the `--lut` map (default `cinematic-10` → `MyTools/Cinematic 10.cube`), READS BACK `GetStart/GetDuration` and exits 1 on any mismatch (timeline left for inspection). Overlay/template entries are skipped with a warning. Refuses: missing media, grade without LUT mapping, overlapping clips on one track |

Render guards (in code, not prose): format+codec set via `SetCurrentRenderFormatAndCodec("mp4","H264")`;
`SetRenderSettings` ONE key per call, any False aborts; `AddRenderJob` retried ×3 on ""; `StartRendering([job], isInteractiveMode=False)`;
poll `GetRenderJobStatus` every 0.5 s until JobStatus ∈ {complete, completed} or {cancelled, canceled, failed, error}
(an error STRING variant exists and is normalized to failed); completion proven by a NEW or CHANGED (mtime,size) non-empty file starting with
`--name`; `DeleteRenderJob` in `finally`. Note: the bundled `render` renders VIDEO ONLY — for a deliverable use your own script (see 06) or extend the CLI.

## Recovery after a watchdog trip (exit 3) during `smoke`
1. `resolve.cmd projects --json` → find the editor's original project and any `_pp_smoke_*`.
2. `resolve.cmd load-project "<original>" --yes` FIRST (delete refuses on the loaded project).
3. `resolve.cmd delete-project "_pp_smoke_<id>" --yes`.
4. Tell the editor auto-backup is off until Resolve restarts.

## Golden samples [measured]
```
ssh <user>@editing-rig "cmd /c 'D:\Editing\ResolveTools\resolve.cmd status --json'"
{"product": "DaVinci Resolve Studio", "version": "21.0.4.5", "page": "cut", "project": "Untitled Project", "timeline": null, "cli": "resolve_cli build ..."}

resolve.cmd smoke --yes --timeout 180 --json     # SMOKE PASS end-to-end 2026-08-31 on the rig
```

## Promotion triggers for a sidecar/press (decision 7.11, not yet met)
>~15 commands · workstation agents must drive Resolve without SSH · a queryable SQLite mirror of
project/pool state is needed. Until then `resolve_cli.py` evolves by hand; add new verbs to
`COMMANDS` + `WRITE_COMMANDS`/`WRITE_HELPERS` and keep the read-safety gate green.

## Writing an ad-hoc script on the rig (preferred over many CLI calls on a slow link)
```
scp myjob.py <user>@editing-rig:%USERPROFILE%/myjob.py
ssh <user>@editing-rig "D:\Editing\ResolveTools\python312\python.exe %USERPROFILE%\myjob.py"
```
`python312._pth` puts `D:\Editing\ResolveTools` on `sys.path`, so `from connect import get_resolve` works.
Wrap every API call in the watchdog pattern from 02; print JSON to stdout; never call `Set*`/`Add*`/`Load*`
unless the job is explicitly a write the operator approved.
