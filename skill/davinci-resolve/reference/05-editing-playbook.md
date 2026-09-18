# 05 — Editing playbook: editor instruction → API operations

The API is an **assembly** API, not an **editing** API. Everything an editor calls a trim, roll,
slip, slide, ripple, razor, or keyframe must be computed by us and expressed as fresh
`AppendToTimeline` geometry (or as a Fusion/FFmpeg operation). Internalize this and stop looking
for the missing method. **21.1 exceptions (09 §5): `AddTransition` (needs media handles),
`SetFades`, `SetSpeed`, `SetProperties`, multicam create/smart-switch/flatten, and transcript
readback via `GetTranscription` — the rows below say "21.1:" where that changes the answer.**

## Frame math (integer frames, never float seconds)
- Timeline rate R from `project.GetSetting("timelineFrameRate")` (string like "29.97" → rate
  30000/1001). Conventional integer rate for specs: 23.976→24, 29.97→30, 59.94→60.
- Timeline start S = `tl.GetStartFrame()` (Resolve default 01:00:00:00 → **108000 @30/29.97,
  86400 @24/23.976, 90000 @25, 216000 @60**; brand-new timelines may start at 0 if the
  project's start timecode is 00:00:00:00). Every `recordFrame` and every item `GetStart()` is
  ABSOLUTE (includes S). Spec `record` values are relative to `spec.start_frame` or to S when absent.
- Timecode ↔ frames (non-drop): `frames = ((hh*60+mm)*60+ss)*R_int + ff`. Drop-frame (29.97 DF,
  59.94 DF): skip 2 frames per minute except every 10th minute (`timelineDropFrameTimecode`).
  Prefer non-drop for YouTube work (`"29.97"` not `"29.97 DF"`).
- Source in/out: `startFrame`/`endFrame` are SOURCE frames of the media pool item (0-based from the
  clip's first frame, NOT source timecode). `endFrame` behaves as exclusive/half-open: a clip with
  startFrame=0,endFrame=90 yields `GetDuration()==90` [measured by the bridge readback]. Cast to
  `int()` — floats are accepted but drift.
- Duration placed = endFrame − startFrame. Next clip's recordFrame = previous record + duration
  (no gap) — pre-compute the whole EDL before touching Resolve.
- Mixed-rate sources: Resolve conforms by `timelineFrameRateMismatchBehavior` (resolve = retime
  to timeline rate). A 59.94 source on a 29.97 timeline plays real-time with frame skipping;
  source frames still index the SOURCE rate. For a slow-mo intent see "Retime" below.
- Audio-only placement uses `mediaType: 2` and audio `trackIndex`; video-only `mediaType: 1`.
  Omitting `mediaType` places both video and its linked audio (linked pair).

## Building a timeline from scratch (the proven order) [measured via `build`]
1. `status --json` → page not null, correct project. If not: `load-project`.
2. `project.SetSetting("timelineFrameRate", "29.97")` BEFORE any timeline/media exists (locked afterwards).
3. Set the media pool folder (`mp.SetCurrentFolder(bin)`), `ImportMedia([...])`, re-index by `File Path`.
4. `tl = mp.CreateEmptyTimeline(name)`; `tl.SetSetting("useCustomSettings","1")` then width/height + output width/height (order matters).
5. Grow tracks: `while tl.GetTrackCount("video") < n: tl.AddTrack("video")`; audio likewise (`AddTrack("audio","stereo")` for stereo beds).
6. Append every item with full clipInfo; after EACH append verify `GetStart()`/`GetDuration()`; abort on mismatch (do not continue placing on top of a wrong base).
7. Per-item static props (`ZoomX/ZoomY` for fill, `Pan/Tilt`, `RotationAngle`, `Opacity`, `Scaling`, `CompositeMode`) via `SetProperty`.
8. Grades: `item.GetNodeGraph().SetLUT(1, "MyTools/Cinematic 10.cube")` (or `SetCDL`, `ApplyGradeFromDRX`).
9. Titles/overlays: see 07 (Text+ via Fusion comp, or pre-rendered ProRes 4444 media appended like any clip).
10. `pm.SaveProject()`; then render (06); verify by file; then `video-editor-review` on the output.

## Editor vocabulary → what to do
| Editor says | Meaning | API/pipeline operation |
|---|---|---|
| "cut/razor at 00:01:12:05" | split a placed clip | No razor. Delete the item (`tl.DeleteClips([item])`), re-append as two clipInfos: A = (src in, src in+k, record r), B = (src in+k, src out, record r+k). Grades/markers/props on the original are LOST — re-apply from your spec, never from readback |
| "trim the head/tail 10 frames" | change in/out at one edge, keep position (tail trim leaves a gap unless ripple) | Delete + re-append with new start/end; for ripple, recompute record of every later item on that track and re-append them too |
| "ripple delete this" | remove and close the gap | `tl.DeleteClips([item], True)` — the ONE native edit op with ripple; verify neighbours moved by reading `GetStart()` |
| "lift" | remove, leave gap | `tl.DeleteClips([item], False)` |
| "roll the edit 6 frames later" | move the cut point between A and B | Re-append A with out+6 and B with in+6/record+6 |
| "slip the shot 1s later" | same position/duration, different source range | Re-append with start/end shifted by +R frames, same record |
| "slide" | same content, new position, neighbours adjust | Re-append with new record; recompute neighbours |
| "move to V2 / stack B-roll over" | vertical placement | `trackIndex: 2` in clipInfo (video tracks 1 = bottom). Upper tracks composite over lower |
| "J-cut / L-cut" | audio leads/lags video | Place video and audio as SEPARATE clipInfos (`mediaType 1` and `2`) with different startFrame/record; there is no link-trim |
| "cross-dissolve 12 frames" | transition | **21.1:** `incoming.AddTransition({"type":"Cross Dissolve","category":"simple","position":"start","alignment":"right","duration":12})` — `'right'` at the cut only needs the OUTGOING clip's tail handle (`GetRightOffset()>0`); `'center'`/`'left'` also need the incoming head handle; all four keys are required (measured 09 §5). The transition is an item of `GetType()=='transition'` in `GetItemListInTrack`. Pre-21.1 / no handles: (a) 2-item overlap with a Fusion `Merge.Blend` keyframe (07); (b) FFmpeg `xfade` upstream; (c) a marker `note="XDISSOLVE 12"` for the editor |
| "fade in/out" | opacity ramp | **21.1:** `item.SetFades({"FadeIn": 6, "FadeOut": 6})` (frames; reads back as floats). Older builds: Fusion `Merge.Blend` keyframes, or FFmpeg `fade` upstream; `SetProperty("Opacity", …)` is STATIC only |
| "speed up 2x / slow-mo 50%" | retime | **21.1:** `item.SetSpeed({"Percentage": 50.0, "RippleTimeline": True})` (duration stays unless RippleTimeline; `PitchCorrection` for linked audio). Older builds: pre-retime with FFmpeg (`setpts`, `minterpolate`) and append that; or place a 59.94 source on 29.97 with a halved range. `RetimeProcess=3` (optical flow) still only picks the interpolation |
| "punch in / energy zoom / push" | scale animation | Static `ZoomX/ZoomY` only. Motion = Fusion comp Transform `Size` keyframes (`BezierSpline`) or a published `.setting` motion template; simplest robust path: FFmpeg `zoompan` upstream for short reels |
| "reframe to vertical / follow the car" | Smart Reframe | `item.SmartReframe()` after setting the timeline to 1080×1920 and the project's reframe mode via GUI/settings; result is a Resolve-side crop; verify visually. For dual-fisheye Insta360 use the ffmpeg `v360` recipe from memory (per-clip yaw/roll) instead |
| "stabilize this" | `item.Stabilize()` (uses the clip's current stabilization mode; async-ish, check the GUI) |
| "remove silence / dead air" | pacing cut | Harness STT → silence intervals → generate MANY clipInfos (each kept speech span = one clip); keep 120–200 ms pads, ≥500–750 ms min silence; place contiguous |
| "add a marker at every cut / beat" | `tl.AddMarker(frame_abs - S, "Blue", name, note, 1, customData)` — frameId is OFFSET from timeline start (S-relative) |
| "put the title at 3 seconds for 2 seconds" | overlay | Text+ via Fusion (07). Item duration can't be set by the API for inserted titles (5 s default) — for exact durations pre-render the title (ProRes 4444 w/ alpha) and append with exact geometry, or create the Text+ inside a 2-item timeline and trim by re-append is impossible… so: pre-rendered media is the deterministic path; Fusion titles are the editable path |
| "captions / burn subtitles" | short-form kinetic captions = ASS via FFmpeg burn as the LAST step after grade; long-form soft captions = SRT (`ExportSubtitle` + `SubtitleFormat: SeparateFile`) or import an .srt through the GUI/`ImportTimelineFromFile`? (no subtitle import API — GUI) |
| "match the audio / sync" | `mp.AutoSyncAudio([...], {waveform})` — content dependent; fallback = harness STT + cross-correlation to compute the offset, then place with computed record |
| "duck the music under VO" | Fairlight has no automation API — bake ducking in FFmpeg (`sidechaincompress`) upstream; place the mixed bed |
| "normalize to -14 LUFS" | FFmpeg two-pass `loudnorm` upstream; Resolve `ExportAudio` passes it through (do NOT re-normalize in Resolve) |
| "color it like the reel / apply the look" | `SetLUT(1, cube)` on node 1, or `ApplyGradeFromDRX(still.drx, 0)`, or `CopyGrades([targets])` from a hero item; groups: `AddColorGroup` + `AssignToColorGroup` and grade the group's post-clip graph once |
| "make it a compound clip" | `tl.CreateCompoundClip([items], {"name": …})` |
| "duplicate the timeline for a v2" | `tl.DuplicateTimeline("name v2")` (then `SetCurrentTimeline` if you must work on it) |
| "export an XML / EDL for me" | `tl.Export(path, resolve.EXPORT_FCPXML_1_10)` / `EXPORT_EDL` with `EXPORT_NONE`; `EXPORT_OTIO`; `EXPORT_DRT` (Resolve-native, versioned, no public schema — not for authoring) |
| "which clips did I select?" | `tl.GetSelectedClips()` — filter `GetTrackTypeAndIndex()[0]=="video"` |
| "what's on the timeline?" | walk `GetItemListInTrack` for every track; capture name, start, end, duration, source range, `GetMediaPoolItem().GetClipProperty("File Path")` |

## Rebuild-not-mutate discipline
Because every "edit" is delete + re-append, keep the **edit-spec JSON as the single source of
truth** and rebuild a NEW timeline (`name v3`) rather than mutating the editor's timeline. Readback
(`export-spec`) is for verification and for capturing a human's cut, not the master.
Grades/markers/props are re-applied from the spec on rebuild.

## Reading the editor's own timeline safely
`resolve.cmd clips --timeline "Name" --json` and `export-spec --timeline "Name"` never switch the
GUI. They give integer frames at the timeline rate with `start_frame` and `end_frame` so you can
diff two versions or convert a human rough cut into a spec.

## Media ingest details
- `ImportMedia` returns items in order; match by `GetClipProperty("File Path")` normalized
  (case-insensitive, `/`↔`\`). Re-importing an existing path returns the existing item (no dup) in
  most cases — still index by path.
- Image sequences: `[{FilePath:"frame_%04d.png", StartIndex:1, EndIndex:120}]`; a folder path
  imports everything in it.
- Bins: `mp.AddSubFolder(root, "A-roll")`, `mp.SetCurrentFolder(bin)` BEFORE import; move later with `MoveClips`.
- Clip metadata for the editor: `SetMetadata({"Description": "...", "Keywords": "car,exterior", "Shot": "…", "Scene": "…"})`; markers with `customData` for machine tags; `SetClipColor("Orange")` for flags.
- Multicam: **21.1:** `mp.CreateMulticamClip([clips], {"name": …, "angleSyncMode": resolve.MULTICAM_ANGLE_SYNC_TIMECODE|_AUDIO|_IN|_OUT|_MARKER, "multicamAudioMode": …})` → `[MediaPoolItem]` (`Type` 'Multicam'); append it like any clip (lands as `"<name> - Angle 1"`, `trackIndex` ignored in the measurement); `item.PerformMulticamSmartSwitch({"minEditDuration": 1.0, "analysisMode": resolve.SMART_SWITCH_ANALYSIS_MODE_AUDIO_ONLY})`; `item.FlattenMulticam(resolve.FLATTEN_MULTICAM_COPY_GRADE)` turns it into the chosen angle's clip. Pre-21.1: synced stacked timeline + GUI "Convert Compound Clips to Multicam Clips".
- Proxies/optimized media: `LinkProxyMedia(path)` per clip; project `perfProxyMediaMode` 1/2; on the 8 GB rig prefer proxies for 4K/5.7K sources.
- Insta360 `.insv` dual-fisheye: Resolve cannot stitch; FFmpeg `v360` recipe (memory) → flat 1080×1920 ProRes/H.264 → import.

## What you cannot get from Resolve (plan around it)
Waveform/loudness values, Fairlight mixer/automation, keyframe values on Edit-page properties, Dynamic Zoom rectangles, IntelliCut/Music Remixer/Dialogue Separator/ducking (GUI only), IntelliSearch queries (indexing only), a razor/split primitive, a "move" primitive, render settings readback (`GetRenderSettings` is not present — verify by output file).
**No longer on this list since 21.1 (09):** transcript text (`GetTranscription`, per-word timing), transitions (`AddTransition`, `GetType()=='transition'`), fades/speed (`SetFades`/`SetSpeed`), multicam (`CreateMulticamClip`/`FlattenMulticam`/`PerformMulticamSmartSwitch`), audio normalization (`NormalizeAudioLevel`), per-item audio volume/pan as STATIC properties (`SetProperties({"AudioVolume": -6.0})`).
