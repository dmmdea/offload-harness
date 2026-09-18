# 06 — Render & delivery

## The proven render sequence [measured, in `resolve_cli.py`]
```python
resolve.OpenPage("deliver")                      # page switch before render config; sleep 0.5 s [community]
project.SetCurrentRenderFormatAndCodec("mp4", "H264")       # format = extension key from GetRenderFormats(); codec = ID from GetRenderCodecs()
for k, v in {"SelectAllFrames": True, "TargetDir": out_dir, "CustomName": stem,
             "ExportVideo": True, "ExportAudio": True, "FormatWidth": 1080, "FormatHeight": 1920,
             "FrameRate": 29.97, "VideoQuality": 12000, "AudioCodec": "aac", "AudioBitDepth": 16,
             "AudioSampleRate": 48000, "NetworkOptimization": True}.items():
    assert project.SetRenderSettings({k: v}), k               # ONE key per call; False = abort
job = project.AddRenderJob()                                  # may be "" → retry ≤3
assert job
assert project.StartRendering([job], isInteractiveMode=False)
# poll GetRenderJobStatus(job) every 0.5 s: JobStatus Complete|Failed|Cancelled (or an error STRING)
# verify: new/changed non-empty file in out_dir starting with stem; then DeleteRenderJob(job)
```
Timeline selection: the render uses the CURRENT timeline — `project.SetCurrentTimeline(tl)` first (switches the GUI, so do it in a supervised window). `MarkIn`/`MarkOut` (absolute timeline frames) with `SelectAllFrames: False` render a range. `SetCurrentRenderMode(1)` = single clip (default for delivery); 0 = individual clips.

Presets: `project.LoadRenderPreset("YouTube - 1080p")` then override single keys (TargetDir, CustomName, FormatWidth/Height for vertical). Preset names on 21.0.4.5 [measured]: `H.264 Master, HyperDeck, H.265 Master, ProRes 422 HQ, YouTube - 720p/1080p/1440p/2160p, Vimeo - 720p/1080p/2160p, TikTok - 720p/1080p, Presentations, Dropbox - …, Replay - …, IMF - …, FCP - Final Cut Pro 7/X, Premiere XML, Audio Only, AVID AAF, Pro Tools, Tencent - …, VR 180/360 - Meta Quest VR / YouTube VR`. Quick Export presets: `H.264 Master, HyperDeck, H.265 Master, ProRes 422 HQ, YouTube, Vimeo, TikTok, Presentations, Dropbox, Replay`.

## `SetRenderSettings` keys [doc]
`SelectAllFrames` (Bool; True ignores MarkIn/Out) · `MarkIn`/`MarkOut` (int) · `TargetDir` · `CustomName` · `UniqueFilenameStyle` (0 prefix, 1 suffix) · `ExportVideo` · `ExportAudio` · `FormatWidth`/`FormatHeight` · `FrameRate` (float 23.976, 24, 29.97…) · `PixelAspectRatio` ("square"|"cinemascope"; SD "16_9"|"4_3") · `VideoQuality` (0 auto; int = bitrate in **kb/s** for restrict-to-rate codecs; "Least"/"Low"/"Medium"/"High"/"Best" for quality-level codecs) · `AudioCodec` ("aac", "lpcm"…) · `AudioBitDepth` · `AudioSampleRate` · `ColorSpaceTag`/`GammaTag` ("Same as Project" …) · `ExportAlpha` · `AlphaMode` (0 premultiplied, 1 straight) · `EncodingProfile` ("Main", "High", "Main10" — H.264/H.265 only) · `MultiPassEncode` (H.264 only) · `NetworkOptimization` (QuickTime/MP4) · `ClipStartFrame` · `TimelineStartTimecode` · `ReplaceExistingFilesInPlace` · `ExportSubtitle` + `SubtitleFormat` ("BurnIn"|"EmbeddedCaptions"|"SeparateFile") · `UseFullExtents` · `AddFrameHandles` · `DataBurnIn` ("Same as project"|"None").
Not exposed: encoder (Native vs NVIDIA is chosen by the CODEC ID), rate-control mode, GOP/keyframe interval, CRF, preset speed, chroma subsampling for H.26x, audio channel layout. `Encoder` key exists only in `RemoveMotionBlur` options ("Native"|"MainConcept", H.265). A readback API does not exist — snapshot-by-file only.

## Format → codec ids on 21.0.4.5 (NVIDIA box) [measured; full map in the live dump]
| format key | codec description → **id** |
|---|---|
| `mp4` | H.264 → `H264` · H.264 NVIDIA → `H264_NVIDIA` · H.265 → `H265` · H.265 NVIDIA → `H265_NVIDIA` · AV1 8/10-bit NVIDIA → `AV1YUV420_8_NVIDIA` / `AV1YUV420_10_NVIDIA` · YUV 422 10-bit (APV) → `APVYUV422_10` |
| `mkv` | same H.26x/AV1 set + ProRes (`ProRes422`, `ProRes422HQ`, `ProRes422LT`, `ProRes422P`, `ProRes4444`, `ProRes4444XQ`) + FFV1 variants |
| `mov` (QuickTime) | **dict came back empty in the dump (`"QuickTime": {}`)** — the codec list for QuickTime is populated only when a project/timeline context exists or is page-dependent; expect `ProRes422HQ`, `ProRes4444`, `H264`, `H264_NVIDIA`, `DNxHR…`, `GoProCineForm…` once queried inside a real project. Query `GetRenderCodecs("mov")` at run time and pick from what comes back |
| `png`/`tif`/`dpx`/`exr`/`jpg` | image sequences (`RGB8`, `RGB16`, `RGBFloatDWAB`…) — set `SetCurrentRenderMode(0)` is NOT needed; sequences are frame files by nature |
| `wav` | Wave: {} in the dump — audio-only export goes through the "Audio Only" preset or `ExportVideo=False` on a QuickTime/MP4 job |

Empty codec dicts (`BRAW, Cineon, HLS, JPEG 2000, MTS, MXF OP-Atom/OP1A, Panasonic AVC, QuickTime, Wave`) in a projectless dump mean "not queryable here", not "unavailable" — re-probe inside the target project. Counts are per machine/license/plugins (99 pairs on one build, 271 on another).

## Delivery numbers (2026 guidance, YouTube/Shorts) [community consensus + platform docs]
| Target | Container/codec | Resolution & rate | Video bitrate (VBR restrict) | Audio |
|---|---|---|---|---|
| YouTube 1080p | MP4 · H.264 High (or H.265 for 4K) | 1920×1080 @ source fps (29.97/30 or 59.94/60) | 8–12 Mb/s @30, 12–16 @60 (Resolve's YouTube preset ≈ 10 Mb/s) | AAC 48 kHz, 320–384 kb/s stereo |
| YouTube 4K | MP4 · **H.265** (forces VP9/AV1 re-encode, better than avc1) | 3840×2160 | 35–56 Mb/s @30, 53–85 @60 (some use 60/110) | same |
| Shorts / Reels / TikTok | MP4 · H.264 High | **1080×1920**, ≤ 3 min (Shorts), 29.97/30 | 10–16 Mb/s | AAC 48 kHz 320 kb/s |
| Master / archive | MOV · ProRes 422 HQ (`ProRes422HQ`) or DNxHR HQX | timeline res | — | LPCM 24-bit |
| Titles with alpha | MOV · ProRes 4444 (`ProRes4444`) + `ExportAlpha: True` | timeline res | — | none |
Keyframe interval (GUI only): half the frame rate (15 @30, 30 @60). Closed GOP, CABAC, 4:2:0, Rec.709 Gamma 2.4 tags; set `ColorSpaceTag`/`GammaTag` "Same as Project" unless HDR. HDR needs 10-bit + Rec.2020 + PQ/HLG + H.265/AV1.
Loudness: deliver −14 LUFS integrated / −1.0 dBTP / LRA≈11 (two-pass loudnorm upstream, measured ±0.5 LU gate). Do not let Resolve re-normalize.

## Encoder choice on the RTX 5060 [community + log]
- `H264_NVIDIA` / `H265_NVIDIA` (NVENC) render several× faster than `H264`/`H265` (Native/MainConcept software) at visually equal quality for ≥8 Mb/s targets; NVENC on Blackwell supports 4:2:2 and 10-bit. Use NVIDIA ids for iteration renders and social deliverables; use Native only when squeezing very low bitrates or when a partner demands a specific profile.
- Hardware DECODE on NVIDIA is active for H.264/HEVC/AV1/VP9 on this box (log), so H.264 camera sources scrub fine; Puget's public table shows blanks for NVIDIA decode — our log is the ground truth for this driver.
- `MultiPassEncode: True` only for Native H.264 (slower, marginal gain). `EncodingProfile: "High"` for H.264 delivery, `"Main10"` for 10-bit HEVC.
- For a 30-s vertical short on this rig expect well under a minute wall time with NVENC; the 1-s Solid Color smoke took "tens of seconds" end-to-end because queueing dominates.

## Verify-by-file checklist (never trust status alone)
1. File exists, size > 0, mtime after job start.
2. `ffprobe -v error -show_entries stream=codec_name,width,height,r_frame_rate,bit_rate,duration -of json <file>` — codec/resolution/fps/duration match the intent (±1 frame).
3. Audio present when `ExportAudio` was True (`a:0` stream, 48 kHz, 2 ch); loudness `ffmpeg -af ebur128` within ±0.5 LU of target.
4. Frame sanity: extract 3 frames (first, middle, last) + a contact sheet; OCR burned captions; run `video-editor-review` for a real deliverable (offload_video_describe / offload_transcribe for dead air & repeated phrases).
5. Remove the job from the queue (`DeleteRenderJob`) — leftover jobs confuse the editor and later `StartRendering()` (all) calls.

## Quick Export [doc]
`project.RenderWithQuickExport("YouTube", {"TargetDir": d, "CustomName": n, "VideoQuality": "High", "EnableUpload": False})` renders the current timeline immediately and returns a status dict or an error string; `EnableUpload: True` uploads to the linked account — never enable from automation.

## Remote rendering / render farm
Resolve's remote-render workers need a shared PostgreSQL project library; the Disk database on the rig doesn't support it. If a second render node is ever needed, the offline-authoring + `ImportTimelineFromFile`/`ImportProject` route on the workstation is the realistic path, or FFmpeg for non-Resolve encodes.
