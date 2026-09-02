---
name: ffmpeg
description: Use for any FFmpeg/ffprobe work in the video pipeline — probing a media file, cutting/trimming/concatenating, retiming, scaling for YouTube 16:9 or Shorts/Reels 9:16, thumbnails, NVENC or CPU encoding choices, loudness normalization (-14/-16 LUFS), silence/dead-air removal, ducking music, burning or muxing subtitles, drawtext lower thirds, alpha overlays, GIF/WebP, image sequences, stream mapping/metadata/timecode, ffmpeg quoting in PowerShell/cmd/Git Bash, progress and exit-code handling, and verifying a rendered output. Triggers on "ffmpeg", "ffprobe", "transcode", "convert this video", "normalize the audio", "burn captions", "cut the clip", "make a Short", "why is my filtergraph failing", "is the render correct".
---

# FFmpeg

Drive ffmpeg/ffprobe autonomously and prove every output. The reference folder holds measured
facts (2026-09-01, ffmpeg 8.1.2 on the workstation, 8.0.1 on the Linux node); read `reference/README.md`
first — its ten rules are the contract — then only the file the task needs.

## Routing

| Task | Read |
|---|---|
| Which box / which encoder / how fast | `reference/01-hosts-builds.md` + the live dump JSON for that host |
| Read a file, count frames, keyframes, loudness, silence, frozen frames, **verify a render** | `reference/02-probing-and-verification.md` |
| Cut, trim, `-ss` placement, concat, crossfade, speed, pitch | `reference/03-cutting-concat-retime.md` |
| Aspect/scale, 16:9, 9:16, thumbnails, GIF/WebP, image sequences, ProRes/DNxHR, delivery recipes | `reference/04-scaling-formats-delivery.md` |
| Encoder settings, NVENC presets/CQ, CRF, two-pass, GOP, throughput | `reference/05-encoding-nvenc-cpu.md` |
| Loudness, dead air, ducking, channels, joining audio | `reference/06-audio.md` |
| Subtitles, drawtext, overlays, mapping, metadata, chapters, timecode | `reference/07-subtitles-text-overlays.md` |
| Typing a filtergraph in any Windows shell, exit codes, `-progress` | `reference/08-windows-quoting-and-scripting.md` |
| It "worked" but the output is wrong | `reference/09-failure-modes.md` |

## Operating rules (short form)
1. `hostname` + `ffmpeg -version` before promising an encoder; the workstation also has a 9.0.1 off-PATH that the harness uses.
2. Build commands as argv lists from Python (no shell) or `-filter_script`; when a shell is unavoidable follow 08 (single-quote font/subtitle paths inside the graph, `expansion=none` for `%`).
3. Frame-exact cuts re-encode; `-c copy` only on keyframes. Input `-ss` resets time — subtitles/enable/`-to` need `-copyts`.
4. Loudness: two-pass loudnorm, then `ebur128` re-measure. Dead air: `silencedetect` → cut list → `select`/`aselect`.
5. Every output passes the 02 verification gate (ffprobe JSON, frame count, loudness, silence, freeze, pixel diff for text) before it is called done. Exit 0 proves nothing.
6. Test on lavfi clips (02 has the recipes) before touching footage; clean temp files after.
7. Media judgment (does the video look/sound right) goes through the harness eyes/ears (`offload_video_describe`, `offload_transcribe`, `offload_vqa`) — never a single sampled frame.

Gaps still unmeasured are listed at the end of `reference/01-hosts-builds.md` and `05`; re-measure and update the tags rather than guessing.
