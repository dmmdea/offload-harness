# 01 — Hosts, builds, encoders, throughput

Check `hostname` first. Re-verified live 2026-09-01 unless dated otherwise. The two live
dumps (`live-dump-<host>-2026-09-01.json`) are the exact availability lists; this file is
the digest.

## workstation (workstation) — the measurement/authoring box

| Item | Value |
|---|---|
| ffmpeg on PATH | **8.1.2-full_build-www.gyan.dev** (winget `Gyan.FFmpeg`), `%LOCALAPPDATA%\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.2-full_build\bin\{ffmpeg,ffprobe,ffplay}.exe`; same path resolves from PowerShell 5.1, pwsh 7.6.5 and Git Bash **[measured]** |
| Second ffmpeg | **9.0.1-full_build-www.gyan.dev** at `<tools dir>\ffmpeg-9.0.1` — this is what the local-offload harness media lane uses (`offload_status.media.routes.media.ffmpeg_path`). NOT on PATH. Nothing in this folder was measured on 9.0.1; re-measure before assuming parity **[measured path + version; behaviour unmeasured]** |
| Build flags (8.1.2) | gpl, nonfree-free static; libx264, libx265, libsvtav1 (v4.1.0), libaom, librav1e, libvpx, libvvenc, libwebp, libass 0.17.5 (font provider **DirectWrite**), libfreetype+harfbuzz+fribidi, libplacebo+vulkan+shaderc, libvmaf, librubberband, libsoxr, libopus, libmp3lame, whisper; hw: nvenc/nvdec/cuvid, cuda-llvm, amf, libvpl (qsv), d3d11va/d3d12va/dxva2, vaapi, opencl **[measured from `-version`]** |
| `-hwaccels` | cuda vaapi dxva2 qsv d3d11va opencl vulkan d3d12va amf **[measured]** |
| Counts | 243 encoders, 557 decoders, 576 filters, 419 formats **[measured]** |
| GPUs | 0: RTX 5060 Ti 16 GB · 1: RTX 5070 Ti 16 GB · 2: RTX 5060 Ti 16 GB, driver 616.56. NVENC picks GPU 0 by default; `-gpu 1` selects the 5070 Ti (measured identical speed for h264 p4). GPUs 0 and 2 were carrying ~11 GB of llama-swap models at ~45 % util during the benchmarks below — numbers are "shared box" numbers **[measured]** |
| Python | `C:\Program Files\Python314\python.exe` (PIL available — used to verify animated WebP) **[measured]** |
| Fonts | `C:\Windows\Fonts\arial.ttf`, `arialbd.ttf`, `consola.ttf` used in the tests **[measured]** |
| Fontconfig | **no default config** — `Fontconfig error: Cannot load default config file` and `drawtext font=` segfaults; `subtitles`/`ass` are fine because libass uses DirectWrite **[measured]** |

### Encoders / decoders / filters that matter (workstation 8.1.2) [measured]
- Encoders present: h264_nvenc, hevc_nvenc, av1_nvenc, libx264, libx265, libsvtav1, libaom-av1, librav1e, libvpx-vp9, libvvenc, h264/hevc/av1_amf, h264/hevc/av1_qsv, h264/hevc_mf, prores_ks, prores, dnxhd, png, apng, gif, libwebp, libwebp_anim, mjpeg, aac, libopus, libmp3lame, flac, pcm_*, ass, subrip, mov_text, webvtt, ffv1.
- Decoders present: h264(+cuvid), hevc(+cuvid), av1(+cuvid, libdav1d), vp9(+cuvid), prores, dnxhd, mjpeg, aac, opus, mp3, flac, ass/srt/subrip/webvtt/mov_text.
- Filters present: scale_cuda, overlay_cuda, hwupload_cuda, libplacebo, scale_vulkan, subtitles, ass, drawtext, loudnorm, ebur128, silencedetect, silenceremove, sidechaincompress, rubberband, atempo, minterpolate, xfade/acrossfade, palettegen/paletteuse, freezedetect, mpdecimate, libvmaf, ssim/psnr, signalstats, whisper, …
- **Missing:** `scale_npp` (no NPP in the Gyan build — "No option name near…" is what you get, not "no such filter"); `lavfi` is a device, not in `-formats` (use `-f lavfi -i …`, it works).

### Measured throughput, workstation, 2026-09-01 (testsrc2 source, `-an`, wall clock incl. decode) [measured]
1080p30, 600 frames (20 s):

| Encoder / settings | fps | output MB |
|---|---|---|
| libx264 veryfast crf20 | 355 | 18.5 |
| libx264 medium crf20 | 196 | 20.9 |
| libx264 slow crf20 | 112 | 20.2 |
| libx265 medium crf24 | 61 | 18.4 |
| libsvtav1 preset 8 crf32 | 147 | 19.1 |
| libsvtav1 preset 6 crf32 | 72 | 16.3 |
| libaom-av1 cpu-used 8 crf32 | 10 | 12.5 |
| h264_nvenc p4 hq vbr cq23 | 372 | 26.9 |
| h264_nvenc p7 + multipass fullres + lookahead 32 + AQ | 195 | 26.5 |
| hevc_nvenc p4 hq cq26 | 306 | 19.4 |
| hevc_nvenc p7 + multipass + lookahead + AQ | 102 | 20.0 |
| av1_nvenc p4 hq cq30 | 365 | 22.8 |
| av1_nvenc p7 + multipass + lookahead | 164 | 22.3 |
| full GPU path `-hwaccel cuda -hwaccel_output_format cuda` → h264_nvenc p4 | 388 | — |

4K30 (3840×2160), 300 frames (10 s):

| Encoder / settings | fps |
|---|---|
| libx264 medium crf20 | 61 |
| libx265 medium crf24 | 23 |
| libsvtav1 preset 8 crf32 | 34 |
| h264_nvenc p4 cq23 | 107 |
| hevc_nvenc p4 cq26 | 105 |
| hevc_nvenc p7 + multipass + lookahead + AQ | 28 |
| av1_nvenc p4 cq30 | 103 |
| 4K→1080p full GPU (nvdec + scale_cuda + hevc_nvenc p4) | 258 |

## Linux node (Linux node M720q) `<linux-node>` — Linux fleet node

| Item | Value |
|---|---|
| Reach | `ssh <node>` (the node user, key auth; `<other user>@<node>` is refused). the tailnet name **[measured]** |
| OS / CPU / RAM | Ubuntu, kernel 7.0.0-30, i9-9900 8C/16T, 62 GB **[measured]** |
| GPU | RTX 3050 6 GB, driver 610.43.02 **[measured]** |
| ffmpeg | **8.0.1-3ubuntu2** (`/usr/bin/ffmpeg`, gcc 15), python3 present **[measured]** |
| `-hwaccels` | vdpau cuda vaapi qsv drm opencl vulkan **[measured]** |
| Encoders | h264_nvenc, hevc_nvenc, av1_nvenc (listed, **but "No capable devices found" on the RTX 3050** — Ampere has no AV1 NVENC), libx264, libx265 (4.1), libsvtav1 (2.3.0), libaom-av1, librav1e, libvpx-vp9, *_qsv (listed; iGPU untested), prores_ks, dnxhd, libwebp(_anim), aac, libopus, libmp3lame, flac. No AMF/MF (Windows-only), no libvvenc **[measured]** |
| Filters | scale_cuda, overlay_cuda, libplacebo, subtitles, ass, drawtext, loudnorm, ebur128, silencedetect, sidechaincompress, rubberband present; **no scale_npp, no libvmaf, no whisper** **[measured]** |
| Counts | 226 encoders, 544 decoders, 558 filters, 419 formats **[measured]** |

Measured throughput, Linux node, 2026-09-01 (same clips; the fleet agent seat was resident on the GPU) [measured]:

| 1080p (600 f) | fps | 4K (300 f) | fps |
|---|---|---|---|
| libx264 medium crf20 | 104 | libx264 medium crf20 | 29 |
| libx264 veryfast crf20 | 196 | h264_nvenc p4 cq23 | 84 |
| libx265 medium crf24 | 32 | hevc_nvenc p4 cq26 | 81 |
| libsvtav1 p8 crf32 | 43 | 4K→1080p full GPU hevc | 43 (6.9 s) |
| h264_nvenc p4 cq23 | 304 | | |
| hevc_nvenc p4 cq26 | 295 | | |
| av1_nvenc | **refused** | | |

## laptop node (laptop node 15P) `<laptop-node>` — NOT MEASURED (offline on Tailscale during the whole session, last seen 5–6 h earlier)
Known from the Resolve reference: Windows, RTX 3070 8 GB (Ampere → h264/hevc NVENC, **no AV1 NVENC** [inferred from the RTX 3050 result]), 64 GB, `ssh <node>` (user <user>, PowerShell 5.1 remote shell, no `&&`). ffmpeg version and build **unknown** — run `ffdump.py` (below) when it is up.

## editing rig (Dell editing rig 7060) `<editing-rig>` — NOT MEASURED (offline on Tailscale, last seen 2–3 h earlier)
Known from the Resolve reference [doc-of-ours, 2026-09-01]: Windows 11, RTX 5060 8 GB (Blackwell → h264/hevc/av1 NVENC expected [inferred]), i7-9700T, 64 GB, "ffmpeg on machine PATH" (version unknown), SSH as <user> (PowerShell 5.1, arrives elevated), 8 GB VRAM shared with Resolve/ComfyUI/llama-swap. Editing outputs stay on `<editing exports>`.

## Re-measuring a host (what produced the dumps)
The dump script lives at `<skill dir>\reference\ffdump.py` (copied
from the session). Windows: `python ffdump.py "<path>\ffmpeg.exe" live-dump-<host>-<date>.json`;
Linux: `python3 ffdump.py ffmpeg out.json`. It records version/config/libs, hwaccels, the
subset tables for encoders/decoders/filters/formats, NVENC/x265/SVT option enums, and the full
encoder and filter name lists. Throughput = generate the lavfi clips (03/05 have the exact
commands) and time `ffmpeg -i src -c:v … -an -f mp4 out` with frames ÷ wall.

## Harness state during this build (why the digests were partly manual) [measured 2026-09-01]
- The workstation's own agent seat `qwen3.8-27b` sat in llama-swap state **`starting`** for the whole evening (GPUs 0 and 2 at 100 % / 16 GB each, `--spec-type draft-mtp` load); every request to it answered `429 concurrency_limit` and contracts ran to the 600/900 s wall with no output. That is an infrastructure fault to raise, not a slow seat.
- The Linux node seat `qwen3.5-4b-agent` produced on-goal digests (Seeking, YouTube, subtitles) once the goal named the file and the acceptance was anchored to page-only tokens; the earlier "Go version" answers came from `offload_research`'s generic contracts. Five of eight contracts still hit the 420 s budget.
- trac.ffmpeg.org sits behind Anubis: browser user agents (WebFetch, curl with a Firefox UA) get the challenge page; `curl -A "Go-http-client/1.1"` gets the wiki page — that is how the harness fetcher passes. ffmpeg.org is plain HTML.

## Windows shells on the workstation [measured]
- Windows PowerShell 5.1.26100, pwsh 7.6.5 (`$PSNativeCommandArgumentPassing=Windows`), cmd, Git Bash (MSYS2). Quoting rules per shell: `08-windows-quoting-and-scripting.md`.
- `bc` is not on the Git Bash PATH; do arithmetic in Python.
