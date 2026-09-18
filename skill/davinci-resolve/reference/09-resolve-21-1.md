# 09 — Resolve 21.1 (Studio 21.1.0.14): what changed, what was measured, what it overturns

Measured 2026-09-09 on the workstation (Studio 21.1.0.14, Windows 11, Disk DB "Local Database",
project `_ref_scratch`, synthetic media under `D:\Editing\_pp_smoke\ref\`). Tags as in the rest of
the library: **[measured]** / **[doc]** (the 21.1 `README.md` + `CHANGELOG.md` + `.pyi` stubs) /
**[inferred]**. Everything below supersedes older chapters where they disagree; the older
chapters carry a pointer here at each overturned claim.

## 1. The scripting docs moved and are now typed [measured]
`C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Developer\Scripting\` no longer has
`README.txt`. It has:
- `README.md` (33 KB, "Last Updated 31 Aug 2026") — prose rules (settings semantics, audio mapping
  JSON, export types, item properties, Extras).
- `CHANGELOG.md` (4 KB, "Last Updated 1 Sep 2026") — per-version API changes back to 20.0.
- `DaVinciResolveScript.pyi` (119 KB) — **the authoritative API catalog**: every class, method,
  signature, return type, docstring, plus TypedDicts for every settings/options dict and
  `Literal` enums for colours/track types. `grep -n "def Name" DaVinciResolveScript.pyi`.
  A deterministic method catalog extracted from it lives at `api-catalog-21.1.md` in this folder.
- `Modules\DaVinciResolveScript.py` unchanged (loads `fusionscript.dll`).
- The MCP server bundle (section 3) also ships `fusion_api.pyi` (76 KB) and `ui_api.pyi` (63 KB)
  for the Fusion and UIManager APIs — the only typed reference for those that exists anywhere.

## 2. Interpreters: the per-machine CPython pin is gone on 21.1 [measured]
21.1 ships a **built-in Python 3.14.4** at
`C:\Program Files\Blackmagic Design\DaVinci Resolve\ResolvePython\ResolvePython.exe`
(`python314._pth` → `lib/python314.zip`, `lib-dynload`, `lib/modules`; `import
DaVinciResolveScript` works with NO env vars; no pip/Tk/IDLE) and drops Python 2 [doc].

Interpreter matrix on 21.1.0.14 (workstation), `_probe.py` against a running Studio:

| Interpreter | Result |
|---|---|
| `C:\Program Files\Python314\python.exe` 3.14.7 | works |
| bundled `ResolvePython.exe` 3.14.4 | works |
| uv/Astral CPython 3.11.15 (`%APPDATA%\uv\python\...`) | **works** (was "known-bad" on 21.0.4) |
| `C:\Program Files (x86)\Python314-32` | `DLL load failed ... not a valid Win32 application` (32-bit, expected) |
| `C:\Python27` | Python 2 dropped: the probe cannot even import `faulthandler`; `DaVinciResolveScript.py` still carries the `imp` fallback but nothing else is 2.x |
| `fuscript.exe -l py3` | reports **3.14.7** — Resolve's own script host binds the SYSTEM 3.14 when present (`davinci_resolve.log`: `Fusion | Using Python 3.14.7 at: C:\Program Files\Python314\`) |

Conclusion: on 21.1 every 64-bit CPython ≥3.11 tested binds `fusionscript.dll`; the
0xC0000005-per-machine class of failure (02 §preconditions, 08 #3) is **not reproduced on 21.1**.
Keep `_probe.py` in the deploy gate anyway (it is cheap, and the Dell has not been re-measured on
21.1). Preferred pin for new deploys: the bundled `ResolvePython.exe` (no env vars, upgraded with
Resolve) or the system 3.14.

## 3. The native "MCP server" is a stdio zipapp, not a listener [measured]
Nothing new listens. A GUI 21.1 Resolve owns TCP `0.0.0.0:15000` (scripting) and `0.0.0.0:49152`
(not HTTP: every GET/POST and a raw banner read time out — the README says successful scripting
connections get a dynamic return port in 49152..65535, which is what this is [doc+inferred]).

What Blackmagic shipped in `C:\Program Files\Blackmagic Design\DaVinci Resolve\`:
- `DaVinciResolve.mcpb` (70 KB) — an Anthropic **MCP Bundle** for Claude Desktop: `manifest.json`
  (manifest_version 0.3, name `davinci-resolve`, 14 tools) + `server/index.js`, a Node shim that
  spawns `ResolveMCP.exe` on the first `tools/call`, kills it after 5 min idle, and answers
  `tools/list` from a cached `--dump-tools`. Install by double-clicking the `.mcpb` in Claude
  Desktop (Extensions); nothing else registers it.
- `ResolveMCP.exe` (312 KB, unsigned metadata) — a "pylauncher" stub: runs
  `ResolvePython\ResolvePython.exe` (override with `BMD_RESOLVE_PYTHON_PATH`) on a **zipapp
  appended at byte 143360** (`resolve_mcp_server/{server,tools_script,tools_docs,tools_dctl_lut}.py`
  + `resources/{api/*.pyi, docs/README.md, docs/DCTLReadme.txt, ResolveChangelog.json}`).
  Extract with any zip tool from that offset to read the source.
- Log: `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\logs\mcp.log` (`mcpb.log` for the shim).

Protocol: JSON-RPC 2.0 over **stdio, one JSON object per line**, protocolVersion `2024-11-05`,
serverInfo `davinci_resolve 21.1`, `tools`/`resources` (empty)/`logging` capabilities. Flags:
`--test` (handshake + exit), `--dump-tools`, `--pretty`. **Requires `BMD_RESOLVE_APP_PATH`**
(asserted at import; the `.mcpb` shim sets it — set it yourself when driving the exe directly).

The 14 tools (measured via `tools/list`):
`launch_resolve` · `get_resolve_status` · `get_whats_new(since)` (bundled changelog JSON, 75
entries, **last entry 21.0.4** — it has no 21.1 entry, so `since=21.0.4` answers "up to date") ·
`get_scripting_api(api, types, outline, as_file)` · `search_scripting_api(pattern, api)` ·
`run_script(script, timeout≤60)` · `run_script_unsafe` · `get_scripting_docs(document, section,
as_file)` · `list_dctls` · `list_luts` · `update_dctl` · `delete_dctl` · `delete_lut` ·
`generate_lut(path, size, transform)`.

`run_script` semantics (from source + measured): a **child** `ResolvePython` process connects to
Resolve, installs an audit hook (blocks `open`, imports of os/sys/subprocess/socket/pathlib/…,
`subprocess.Popen`, sockets), strips `open/exec/eval/compile/globals/…` from builtins, injects
`resolve` and `project`, runs your text, returns `{"result": <json-able value of result>,
"output": <prints>}` or `{"error": traceback}`. `import os` → `PermissionError: import not
allowed`; `open()` → `NameError`. `run_script_unsafe` runs in the server process on a daemon
thread with full access (a timed-out script keeps running and holds the connection).
Measured `run_script` on the GUI instance at the Project Manager: `{"page": null, "proj":
"Untitled Project", "studio": true, "kfm": null}` — the page-null trap is visible through it too.

**Client trap [measured]:** the server logs every message at DEBUG to **stderr** (31,920 bytes for
eight calls). A client that pipes stderr and never drains it deadlocks after the first large reply
(our first probe hung on `tools/list`). Drain or redirect stderr.

Verdict for us: the native server is a **thin `run_script` + docs + LUT/DCTL surface** with no
domain tools; our sidecar (03/10) keeps its value. It is, however, the cheapest way to get the
typed API stubs and to script Resolve from Claude Desktop with zero setup.

## 4. New scripting API in 21.1 (CHANGELOG §21.1, all present in the .pyi) [doc]
- Utility shortcuts: `resolve.GetCurrentProject() / GetCurrentTimeline() / GetMediaPool() / GetGallery()`.
- Keyboard presets: `Get/Load/Delete/Import/Export KeyboardPreset`, `GetCurrentKeyboardPreset` (measured: `"DaVinci Resolve"`; list = DaVinci Resolve, Adobe Premiere Pro, Apple Final Cut Pro X, Avid Media Composer, Pro Tools).
- Project settings presets: `Project.GetProjectSettingsPresetList()` → `[{Name, Width, Height}]`, `SetProjectSettingsPreset / DeleteProjectSettingsPreset / SaveCurrentProjectSettingsAsNewPreset / UpdateProjectSettingsPreset / ExportProjectSettingsPreset / ImportProjectSettingsPreset`. (`GetPresetList/SetPreset` still exist.)
- Render presets: `UpdateRenderPreset(name)`, `SetQuickExportEnabledForRenderPreset(name, bool)`.
- Audio render: `GetAudioRenderFormats()` → `{FLAC: flac, MP3: mp3, MP4: mp4, MXF OP-Atom: mxf, MXF OP1A (IMF): imf, QuickTime: mov, Wave: wav}` [measured], `GetAudioRenderCodecs(ext)` (`wav` → `{Linear PCM: lpcm}`).
- **`MediaPoolItem.GetTranscription(useNestedClipTranscription=False)`** → `{language, segments:[{start, end, text, speaker, words:[{start, end, text}]}]}` — transcript readback EXISTS now (section 5).
- Multicam: `MediaPool.CreateMulticamClip(clips, MulticamOptions)`, `TimelineItem.FlattenMulticam(grade)`, `PerformMulticamSmartSwitch(SmartSwitchSettings)`, `Timeline.AutoAlignClips(items, AutoAlignOptions)`.
- TimelineItem: `GetProperties()/SetProperties(dict)` (dict forms; audio properties + `*Enabled` states included), `GetFades()/SetFades({FadeIn, FadeOut})`, `GetSpeed()/SetSpeed({Percentage, PitchCorrection, StretchKeyframesToFit, RippleTimeline})`, `Get/SetOutputBlanking`, `Get/SetUseTimelineForOutputBlanking`, `GetType()` → `video|audio|generator|transition`, **`AddTransition(TransitionOptions)`**.
- Timeline: `Get/SetOutputBlanking({Top,Bottom,Left,Right})`, `GetNormalizeAudioModes()` (14 modes [measured]), `NormalizeAudioLevel(items, {normalizationMode, targetLevel, targetLoudness, setLevelMode})`, `GetSettings()/SetSettings(dict)`.
- Audio mapping setters: `MediaPoolItem.SetAudioMapping(json)`, `TimelineItem.SetSourceAudioChannelMapping(json)` (exactly one track).
- Media storage clone tool: `StartCloneMedia(src, targets)`, `SetCloneToolSettings({PreserveFolderName, ChecksumType})`, `StopCloneMedia()`, `GetCloneStatus()` → `{JobStatus, CompletionPercentage, Error}` (idle reports `Complete/100`).
- DCTL: `resolve.ValidateDCTL(source)` → `None` when valid, else the compiler error string (measured: `"DCTL Error: cannot find main DCTL function.\n"`); `EncryptDCTL(path, {Name, Expiry, OutputFolder})`.
- "Overloaded function signatures are deprecated" [doc]: prefer the dict/list forms (`SetProperties`, `SetSettings`, `ImportMedia([{FilePath…}])`), not the multi-positional variants.
- "Advanced scripting now requires Studio" [doc]: `resolve.IsStudio()` exists; our boxes are Studio.

## 5. Rules the measurements overturned (edit the older chapters accordingly)
| Old claim (chapter) | 21.1 measurement |
|---|---|
| "No transcript readback exists; word timing comes from harness STT" (README rule 9, 05, 07, 08 #21) | `GetTranscription()` returns per-word timecodes. `TranscribeAudio()` is **synchronous**: 20.7 s cold (model load) / 0.7–0.9 s warm for a 7 s clip; returns `True` on **media/cut/edit/fairlight**, `False` in ~0 ms on **fusion/color/deliver**. A pure sine tone transcribes to one empty segment. Whisper-quality: "twenty one point one" came back as "21.1". Harness STT stays the karaoke source (hotwords), Resolve's is now a real cross-check. |
| "No transitions/fades/speed via the API" (README rule 5, 05 table, 08 #15) | `AddTransition` places a 'transition' item (`GetType()=='transition'`, appears in `GetItemListInTrack` between the clips, 12 frames at the cut). It **consumes media handles**: `alignment 'right'` at the cut needs `GetRightOffset()>0` on the outgoing clip; `'left'/'center'` also need `GetLeftOffset()>0` on the incoming clip; the bare call (no position/alignment/duration) returned `None`. `SetFades({FadeIn:6, FadeOut:6})` reads back as floats. `SetSpeed({Percentage:50})` → True, `GetSpeed` 50, duration unchanged (retime within the item; use `RippleTimeline`). |
| "timelineFrameRate is fixed at timeline creation" (04 §Timeline settings) | On an **EMPTY** timeline with `useCustomSettings='1'`, `SetSetting('timelineFrameRate','25')` → True; start frame moved 86400→90000. Once clips exist it returns False. |
| "SetKeyframeMode(current) returns False" (04, 08 #33) | Returns **True** when already set. `GetKeyframeMode()` is `None` while no project page is open (page-null); on edit/color it returned 0/1/2 as expected. |
| "Multicam: no API" (05 §Media ingest) | `CreateMulticamClip([a,b], {name, angleSyncMode: MULTICAM_ANGLE_SYNC_TIMECODE})` → `[MediaPoolItem]` (`Type` 'Multicam', synthetic clips accepted, 42 ms). `AppendToTimeline([{mediaPoolItem: mc}])` → item `"<name> - Angle 1"`; `PerformMulticamSmartSwitch({minEditDuration: 1.0, analysisMode: SMART_SWITCH_ANALYSIS_MODE_AUDIO_ONLY})` → True in ~1 s (with `minEditDuration` alone → False: the default wide-angle detection needs an analysed item); `FlattenMulticam(FLATTEN_MULTICAM_COPY_GRADE)` → True and the item becomes the angle's source clip. A `trackIndex` on a multicam append was ignored (landed on V1). **Trap:** with the API default `createBinForSourceClips=True` the sources are MOVED into an `Original Clips` bin and that multicam answers `AppendToTimeline` with **`[None]`** — re-fetching the handle, `RefreshFolders`, `SaveProject`, page flips and waiting 2 min do not help; creating ANOTHER multicam (or any multicam from sources already in `Original Clips`) makes it appendable, and `CreateTimelineFromClips(name, [mc])` works regardless. Pass `createBinForSourceClips: False` (the bridge's default) and it appends immediately. |
| "ExportRenderPreset refuses stock presets" (04, 08 #34) | A **custom** preset exported fine, but `exportPath` is treated as a **folder**: a directory is created at the path and `<name>.xml` is written inside it. |
| "Timeline.Export(FCPXML) writes a file" (05) | `EXPORT_FCPXML_1_10` writes a **bundle directory** at the path containing `Info.fcpxml`; `ImportTimelineFromFile` wants that inner file. A second export to the same path returns False (will not overwrite the directory). |
| "ArchiveProject returns False for plain paths" (04, 08 #32) | **`ArchiveProject(<loaded project>, 'x.dra')` stalled the API and CRASHED Resolve** (Problem Report dialog, `Resolve.exe.<pid>.dmp` 775 MB). Never call it from a script on 21.1; the bridge refuses it. |
| "Voice isolation / normalization are GUI-only" (07 §Audio) | `Timeline.SetVoiceIsolationState(1, {isEnabled, amount:40})` → True, reads back; `NormalizeAudioLevel([items], {targetLevel:-12.0})` → True. Still no volume automation/pan/EQ/bus readback. |

## 6. Other measured behaviours (new entries for 08)
- `MediaStorage.GetSubFolderList/GetFileList/AddItemListToMediaPool/StartCloneMedia` want **backslash paths**; a forward-slash path answers `[]`/False (log: `no mapped volume found for path D:/…`).
- `ImportTimelineFromFile(otio)` for media already in the pool logs `Import Log - Operation canceled` and returns None unless `{"importSourceClips": False}` is passed; then it imports (Text+ included). `sourceClipsPath` did not help. FCPXML `Info.fcpxml` imported either way (4.7 s with source import, 0.6 s without).
- Timeline imports with `importSourceClips=True` **duplicate media-pool entries** for files already in the pool (7 × `ref_a.mp4` after a few imports). Clean up by File Path; our `delete-clips --all-matches` exists for this.
- `AppendToTimeline` returns **`[None]`** (a list holding None, not `[]`) for an unusable clip — measured for the moved-source multicam case above (the "no current timeline" reading of the first occurrence was wrong: `status` showed `_ref_tl` current). Guard `items[0] is None`.
- `DeleteVersionByName` returns False on the item's CURRENT version — load another first.
- `ExportLUT` returns False on the Edit page, True on the Color page (17-pt cube = 66,895 bytes). `SetLUT`/`GetLUT` and `GrabStill` (returns a `GalleryStill`) work from Edit and Color.
- `GetCurrentClipThumbnailImage()` returned None on Edit and Color at the playhead in the GUI instance, but in the **headless** instance on the Color page returned `288x162 RGB 8 bit`.
- `GenerateSpeech` without the Extras pack returns the **string** `"Required Package, 'AI Speech Generator' is not Installed."` (truthy) — check `isinstance(result, str)`.
- `SetProperties({...})` on a timeline item: the dict form worked (ZoomX/ZoomY/Opacity applied and read back). `GetProperties()` has 32 keys on 21.1 (21 on 21.0.4): the audio keys (`AudioVolume`, `AudioPan`, pitch, voice isolation, dialogue leveler) and `*Enabled` states were added.
- `Timeline.GetSettings()` returns 69 keys with `useCustomSettings='1'` (the project's 158 when '0') — the 2026-09-01 "identical 158" note described the '0' case.
- `SetOutputBlanking({Top:20, Bottom:20})` reads back with floats and `Right: 1280.0` filled in from the timeline width.
- Render: `SetCurrentRenderFormatAndCodec('mp4','H.264 NVIDIA')` (a description) returns False; the bridge translates descriptions to IDs. A 270-frame 1280×720 NVENC job renders in 1.8 s; `RenderWithQuickExport('H.264 Master', {...})` → `{JobStatus: 'Render Complete', TimeTakenToRenderInMs: 1495}` and writes `.mov`.
- Headless `-nogui` on 21.1: up in <90 s, page `media`, `LoadProject` → page `cut`, every write above works EXCEPT the GUI-state ones: `SaveLayoutPreset` → True in the GUI instance, **False headless**; `GetCurrentClipThumbnailImage` → `288x162 RGB 8 bit` once (headless, Color page) and None on later attempts at other playhead positions (treat as best-effort). Layout/burn-in/prefs preset lists are `[]` and `GetFairlightPresets()` is `{}` on this box (nothing saved yet); `LoadKeyboardPreset("DaVinci Resolve")` → True headless.
- `MediaStorage.StartCloneMedia(src, [dst])` returned **False** with backslash paths, a real source folder and a non-existent target (after `SetCloneToolSettings({ChecksumType: MD5})` → True); `GetCloneStatus()` idles at `Complete/100`. Unverified beyond that — the Clone Tool may need the Media page or an existing target.
- `CreateMulticamClip` with the default `createBinForSourceClips=True` creates `<bin>/Original Clips` and MOVES the sources there; `EncryptDCTL(path, {OutputFolder})` → True (writes the encrypted DCTL into the folder).
- Krokodove: `resolve.Fusion().GetRegList(fu.CT_Tool)` lists **544** registered tools, **135** of them `KD_*` (Krokodove: 3D, Image, Image Pixel, Image Warp, Image Create, Image Color, Image Position categories). `fusion.Version` → 21.1.

## 7. Bridge / sidecar on 21.1
The bridge (`engine/resolve_bridge`, branch `feat/resolve-http-sidecar`) grew from 17 to 146
commands covering the absorb manifest; every measurement above is encoded as a guard or an error
message in the command that hit it. Route table: `GET http://127.0.0.1:18800/v1/commands`
(includes each command's argument contract). See 03 for the command reference and 10 for the
printed CLI that wraps it.
