# 08 — Failure modes, traps, and the fix for each

Ordered by how often they bit us. Tags: [measured] on our boxes · [community] · [doc].

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | Every write returns None/False; reads fine; `status.page == null` [measured ×2 machines] | Resolve on the Project Manager / no project page | `load-project NAME --yes` (works from page-null), then retry. Zero projects in DB → `CreateProject` from script also works from page-null [measured workstation 2026-09-01]; or run Resolve `-nogui` (comes up on page `media`, never null) |
| 2 | Command exits 3 / script hangs forever [measured] | GUI modal (auto-backup, media offline, license prompt, save dialog) stalls all IPC | Watchdog every call; back off; ask the editor / look at the screen; `DisableBackgroundTasksForCurrentResolveSession()` at session start for unattended runs (kills auto-backup until restart) |
| 3 | `import DaVinciResolveScript` crashes 0xC0000005 in `PyInit_fusionscript` [measured on 21.0.4] | wrong CPython ABI for this machine (Dell needed 3.12.10, workstation 3.14.7). **Not reproduced on 21.1**: 3.11/3.14 system, uv and the bundled `ResolvePython.exe` all bind (09 §2) | run `_probe.py` under candidates; pin via `deploy.ps1 -PythonPin` (21.1: prefer `ResolvePython.exe`); scrub PYTHONHOME/PYTHONPATH |
| 4 | `scriptapp("Resolve")` returns None | Resolve not running / free edition / scripting pref not Local / launched in a session you can't see | check process + session id, license log line, prefs; launch via COM task (02) |
| 5 | `AddRenderJob()` returns "" [measured/community] | first call in a fresh project / page not Deliver | `OpenPage("deliver")`, small sleep, retry ≤3 |
| 6 | Render "done" but settings ignored (wrong size/no audio) [measured 21.0.4] | `SetRenderSettings` partial-applies a dict; single Bool for the dict | one key per call; abort on any False; verify with ffprobe |
| 7 | `SetCurrentRenderFormatAndCodec("mp4","H.264")` False | codec DESCRIPTION passed instead of ID | use `GetRenderCodecs("mp4")["H.264"]` → `H264` |
| 8 | `GetRenderCodecs("mov")` empty | queried outside a project context (dump was projectless) or page-dependent | query inside the target project on the Deliver page |
| 9 | `GetRenderJobStatus` returns a string | failed job reports an error string, not a dict | normalize: str ⇒ failed with `error` |
| 10 | `SetSetting("timelineFrameRate", …)` False | project already has media/timelines | set rate on an EMPTY project first (build refuses otherwise) |
| 11 | Timeline resolution settings ignored | `useCustomSettings` not "1", or set AFTER the keys (setting it resets several keys to defaults) [community] | `useCustomSettings` first, then width/height/output width/height, then read back |
| 12 | Clip placed at the wrong time / duration off by one | recordFrame relative vs absolute confusion; end treated as inclusive; float frames | absolute record = start_frame + spec.record; end exclusive; `int()` everything; read back `GetStart/GetDuration` and fail on mismatch |
| 13 | `AppendToTimeline` returns None/[] | no current timeline; trackIndex > track count; audio-only mode validating video indices (fixed 20.2.2); item locked track; page null | grow tracks first; check `GetTrackCount`; unlock tracks; verify page |
| 14 | Moving/trimming a clip lost its grade/markers | there is no move; delete+re-append creates a new item | re-apply from the spec; never rely on readback of a deleted item |
| 15 | Transition/fade/speed requested | **21.1 has them**: `AddTransition({type, category, position, alignment, duration})`, `SetFades`, `SetSpeed` (09 §5). `AddTransition` returns None without media handles on the consumed side or when position/alignment/duration are omitted | pass all four options; `'right'` at the cut needs the outgoing clip's tail handle (`GetRightOffset()>0`); `'left'/'center'` need the incoming head handle. Pre-21.1: Fusion Blend keyframes / FFmpeg upstream |
| 16 | `InsertFusionTitleIntoTimeline` lands at 0 or on the wrong track | inserts at PLAYHEAD on the first empty track, 5 s | `SetCurrentTimecode` first; or append a pool Text+ with clipInfo; or pre-rendered title media |
| 17 | `SetInput("StyledText")` no visible change | wrong tool (comp has several), comp not locked, Inspector cache | pick the TextPlus by RegID; Lock/Unlock; `tool.Refresh()`; read `GetInput` back |
| 18 | `SetInput` on an animated input does nothing | keyframes override static values | pass time (`tool.Size[frame] = v`) or delete the spline (`inp.ConnectTo(None)`) |
| 19 | `SetLUT` False | LUT not discovered / path wrong / node index 0 | copy to the LUT root (`…\Support\LUT\MyTools\`), `project.RefreshLUTList()`, path relative to root, 1-based node |
| 20 | `ApplyGradeFromDRX` False / no DRX to apply | Gallery export page-dependent; `ExportCurrentFrameAsStill(.drx)` needs Color page + a current clip | grab in GUI once, or `ExportStills(...,"drx")` with the gallery open; the DRX is reusable forever |
| 21 | `TranscribeAudio` True but no text (21.0.4) / False instantly (21.1) | 21.0.4 had no readback; **21.1 has `GetTranscription()`** and `TranscribeAudio` returns False in ~0 ms on the fusion/color/deliver pages, True (synchronous, 20 s cold) on media/cut/edit/fairlight (09 §5) | switch to the Edit page, budget ≥120 s, then `GetTranscription()` → segments + per-word timecodes; harness STT still owns karaoke timing |
| 22 | AI method returns False instantly | Studio-only / Extras pack missing (IntelliSearch Faster/Better, Slate ID, Speech Generator, language models) | Extras Download Manager in the GUI; check by invoking from the GUI once |
| 23 | GetSelectedClips returns many more items than selected | linked audio items are included | filter by `GetTrackTypeAndIndex()[0]` |
| 24 | `MediaPool.GetSelectedClips()` returns None | empty selection | treat None as [] |
| 25 | Render OK but "GPU memory full" mid-way on the 5060 | 8 GB VRAM: NR, Magic Mask, 4K, Fusion | lower timeline res, cache heavy items, drop NR, free VRAM from ComfyUI/llama-swap, render in ranges |
| 26 | `smoke` exit 3 left `_pp_smoke_*` and the editor's project switched | watchdog kills without cleanup | `load-project <orig> --yes` THEN `delete-project _pp_smoke_* --yes`; tell the editor about auto-backup |
| 27 | `delete-project` refuses | it is the loaded project | load another one first |
| 28 | `load-project X` reports "no project with that name" but X is open | loading the current project returns None by design | ignore if `status.project == X` |
| 29 | Two Studio activations exhausted / "Activate" prompt on launch | key moved without deactivating | deactivate on the other box first; read the license log line |
| 30 | SSH command mangled (`$_` empty, quotes broken) [measured] | Git Bash → PowerShell 5.1 quoting | scp a script file and run it by path |
| 31 | ssh returns but Resolve op keeps running / orphans | ssh timeout kills only the client | remote-side watchdog (Start-Process/Wait-Process/kill) |
| 32 | `ArchiveProject` — **CRASHES Resolve 21.1.0.14** (API stalls, then "Problem Report for DaVinci Resolve", 775 MB dump) [measured 2026-09-09]; returned False on 20.3 | scripted archive is broken | NEVER call it from a script; the bridge refuses `archive-project`. Use `ExportProject` (.drp) + `ImportProject`, or archive from the GUI. `RestoreProject` expects a `.dra` |
| 33 | `SetKeyframeMode(current)` False (20.3) | on 21.1 it returns **True** when already set; `GetKeyframeMode()` is None only in the page-null state | not an error either way |
| 34 | `ExportRenderPreset(name, path)` produced a DIRECTORY | `exportPath` is treated as a folder: `<path>\<name>.xml` is written inside it (21.1) | pass a folder, read `<name>.xml`; stock presets may still refuse — save a custom one first |
| 35 | Wrong FPS interpretation (59.94 source on 29.97 tl looks fast) | mismatch behaviour | `SetClipProperty("FPS", "29.97")` to reinterpret, or conform upstream |
| 36 | VFX/alpha clip appears transparent/black | alpha mode auto | `SetClipProperty("Alpha mode", "None")` or "Straight"/"Premultiplied" as intended [community] |
| 37 | Word timestamps drift on long files (harness) | STT alignment | captions from a ≤30 s stem; assert the es alignment model |
| 38 | Output copied to Google Drive | rule violation | never; MyTools outputs stay on the rig (`D:\Editing\Exports`) |
| 39 | `.drt` generated by hand fails to import | undocumented, version-stamped zip of XML | never author DRT; use FCPXML 1.10 / OTIO / EDL for interchange, spec→append for builds |
| 40 | OTIO/FCPXML import loses links/effects | interchange only carries clips/tracks/timing/markers/simple transitions; clips may arrive unlinked | import timeline then `RelinkClips` to the folder; treat effects as lost |
| 41 | `Timeline.Export(path, EXPORT_FCPXML_1_10)` made a folder; re-export False [21.1] | FCPXML 1.10 is written as a **bundle directory** `path\Info.fcpxml`; Export will not overwrite a directory | import `path\Info.fcpxml`; delete the bundle before re-exporting (the bridge reports the inner file) |
| 42 | `ImportTimelineFromFile(otio)` → None, log "Operation canceled" [21.1] | media already in the pool + default `importSourceClips=True` cancels | pass `{"importSourceClips": False}` (the bridge retries with it) |
| 43 | Media pool fills with duplicate clips after imports [21.1] | timeline imports with `importSourceClips=True` re-import files already in the pool | `delete-clips --all-matches NAME` / dedupe by `File Path` |
| 44 | `MediaStorage.GetFileList/GetSubFolderList/AddItemListToMediaPool/StartCloneMedia` return `[]`/False [21.1] | forward-slash paths; the log says `no mapped volume found for path D:/…` | pass backslash paths (`os.path.normpath`) |
| 45 | `AppendToTimeline` returns `[None]` [21.1] | a list holding None, not `[]`: measured for a multicam clip whose creation moved its sources into `Original Clips` (`createBinForSourceClips=True`, the API default) — it stays un-appendable until another multicam is created | create multicams with `createBinForSourceClips: False` (bridge default), or `CreateTimelineFromClips`; always guard `items[0] is None` |
| 46 | `DeleteVersionByName` False | it is the item's current grade version | `LoadVersionByName` another one first |
| 47 | `ExportLUT` False | Edit page | switch to the Color page (True there; 17-pt cube 66,895 bytes), switch back |
| 48 | `GenerateSpeech` "succeeds" with a string | returns `"Required Package, 'AI Speech Generator' is not Installed."` (a truthy str) without the Extras pack | `isinstance(result, str)` ⇒ error; install via Extras Download Manager |
| 49 | `timelineFrameRate` SetSetting False | the timeline already holds clips; on an EMPTY custom-settings timeline it works on 21.1 | set the rate before appending anything |
| 50 | Native MCP client hangs after the first big reply | `ResolveMCP.exe` logs every message at DEBUG to stderr; an undrained stderr pipe deadlocks it (31,920 bytes for eight calls) | drain/redirect stderr; set `BMD_RESOLVE_APP_PATH` when running the exe directly |

## Diagnostic ladder (when something "doesn't work")
1. `resolve.cmd status --json` — reachable? page? project? timeline?
2. Is Resolve the Studio process in the console session (`Get-Process Resolve | select SessionId`)? Any modal on screen (ask, or `offload_vqa` a screenshot if remote capture is available)?
3. Re-run the failing call in isolation in a 5-line script with the watchdog; print the raw return.
4. Check the log tail: `Select-String -Path "<user>\…\logs\davinci_resolve.log" -Pattern 'ERROR|WARN' | Select -Last 30`.
5. Only then hypothesize. Record the measured fact in this folder (tag + date) so it is never re-derived.
