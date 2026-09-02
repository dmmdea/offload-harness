# 05 — Encoding: NVENC, libx264/x265, SVT-AV1, two-pass, GOP

Measured 2026-09-01 on the workstation (8.1.2, RTX 5060 Ti default NVENC device shared with
llama-swap at the time) and the Linux node (8.0.1, RTX 3050). Throughput tables: 01. Sources: 02.
Quality numbers below are on synthetic `testsrc2` content — VMAF absolutes (~60) are
meaningless there; sizes and RELATIVE ordering are what to take away.

## NVENC option surface on 8.1.2 [measured from `-h encoder=…`]
| Option | Values (h264_nvenc / hevc_nvenc / av1_nvenc) | Default |
|---|---|---|
| `-preset` | `p1` fastest … `p4` medium … `p7` slowest/best (legacy `default/slow/medium/fast/hq/ll/…` still accepted) | p4 |
| `-tune` | `hq`, `uhq` (hevc/av1), `ll`, `ull`, `lossless` | hq |
| `-rc` | `constqp`, `vbr`, `cbr` (override the preset's rc) | preset's |
| `-cq` | 0–51 (av1: 0–63); constant-quality target inside VBR; needs `-b:v 0` | 0 = off |
| `-qp` | 0–51 for constqp | −1 |
| `-multipass` | `disabled`, `qres` (quarter-res first pass), `fullres` | disabled |
| `-rc-lookahead` | frames (32 is the common "quality" value) | 0 |
| `-spatial-aq` / `-temporal-aq` | 0/1; `-aq-strength` 1–15 (default 8) | off |
| `-b_ref_mode` | `disabled`, `each`, `middle` | −1 |
| `-lookahead_level` | −1…15 | −1 |
| `-highbitdepth` | 1 = 10-bit encode from 8-bit input; or `-pix_fmt p010le -profile:v main10` (measured → Main 10 yuv420p10le) | off |
| `-gpu` | device index (0 = 5060 Ti, 1 = 5070 Ti on the workstation) | any |
| `-profile:v` | h264: baseline/main/high/high444p; hevc: main/main10/rext; av1: main | main |
| `-forced-idr 1` | make `-force_key_frames` produce IDR frames | off |
Pixel formats accepted (av1_nvenc): yuv420p nv12 p010le yuv444p p016le … bgra rgba x2rgb10le gbrp… cuda d3d11 [measured].

## NVENC rate control, measured sizes (1080p, 5 s, h264 p4) [measured]
| Mode | Result |
|---|---|
| `-rc constqp -qp 23` | 8.8 Mbps — fixed quantizer, size follows content |
| `-rc cbr -b:v 6000k` | 6.4 Mbps — streaming only |
| `-rc vbr -cq 23 -b:v 0` | 11.0 Mbps — constant quality, no cap (the CRF analogue) |
| `-rc vbr -b:v 6000k -maxrate 9000k -bufsize 12000k` | 7.5 Mbps — capped VBR for size budgets |
Use `vbr -cq N -b:v 0` for quality-first delivery, capped VBR when a platform/size ceiling exists.

## NVENC CQ sweep (1080p30 testsrc2, preset p6 tune hq, 10 s) [measured; sizes real, VMAF relative only]
| Encoder | CQ | Mbps | VMAF (synthetic) |
|---|---|---|---|
| h264_nvenc | 19 | 15.5 | 61.35 |
| h264_nvenc | 23 | 11.4 | 60.96 |
| h264_nvenc | 27 | 7.9 | 60.20 |
| h264_nvenc | 31 | 5.0 | 58.95 |
| hevc_nvenc | 22 | 9.9 | 60.80 |
| hevc_nvenc | 26 | 7.8 | 60.36 |
| hevc_nvenc | 30 | 5.1 | 59.26 |
| hevc_nvenc | 34 | 3.1 | 57.66 |
| av1_nvenc | 26 | 11.9 | 61.33 |
| av1_nvenc | 30 | 9.1 | 61.03 |
| av1_nvenc | 34 | 6.8 | 60.61 |
| av1_nvenc | 38 | 4.6 | 59.82 |
CPU reference points (same clip): libx264 medium crf18 9.75 Mbps / crf23 6.19 / crf28 2.93;
libx265 medium crf22 8.78 / crf28 4.62; libsvtav1 preset 6 crf30 7.49 / crf38 4.04; libx264
two-pass 5000k → 5.00 Mbps (2.3 s for 10 s).
Reading: hevc_nvenc cq 26 ≈ h264_nvenc cq 27 ≈ x264 crf 23 in bitrate on this content; av1_nvenc
needs cq ≈ 34 to reach the same bitrate. Widely used starting points on real footage
[community]: h264_nvenc cq 19–23, hevc_nvenc cq 22–28, av1_nvenc cq 28–34, libx264 crf 18–23,
libx265 crf 20–26, libsvtav1 crf 28–38 with preset 4–8. Confirm on real footage with VMAF
(02) before locking a value.

## REAL FOOTAGE: a real-camera review shoot DJI clip, quality vs bitrate [measured 2026-09-01, workstation]
Source: `<DJI clip>.MP4` (HEVC Main 10, 3840×3840, 59.94 fps, 99.8 Mbps) from
`<operator footage folder>`. Reference = 8 s copy-cut → `fps=30,scale=1920:-2,format=yuv420p`,
libx264 crf 8 (155 Mbps, 240 frames). VMAF = libvmaf `n_subsample=2`, distorted vs that reference.
These numbers ARE representative (real camera content); the testsrc2 table below is not.

| Encoder / setting | Mbps | enc fps | VMAF |
|---|---|---|---|
| h264_nvenc p6 cq19 | 35.0 | 107 | 99.96 |
| h264_nvenc p6 cq23 | 19.9 | 110 | 99.90 |
| h264_nvenc p6 cq27 | 11.0 | 107 | 99.55 |
| h264_nvenc p6 cq31 | 5.9 | 109 | 97.64 |
| hevc_nvenc p6 cq22 | 20.2 | 79 | 99.94 |
| hevc_nvenc p6 cq26 | 11.8 | 78 | 99.80 |
| hevc_nvenc p6 cq30 | 6.3 | 79 | 99.18 |
| hevc_nvenc p6 cq34 | 3.4 | 79 | 96.28 |
| hevc_nvenc p7 + multipass fullres + lookahead 32 + AQ, cq26 | 11.9 | 59 | 99.61 (no gain over p6 here) |
| av1_nvenc p6 cq26 | 17.8 | 115 | 99.96 |
| av1_nvenc p6 cq30 | 11.5 | 112 | 99.91 |
| av1_nvenc p6 cq34 | 7.5 | 114 | 99.69 |
| av1_nvenc p6 cq38 | 4.9 | 113 | 99.01 |
| libx264 medium crf18 | 28.0 | 46 | 99.90 |
| libx264 medium crf23 | 9.7 | 59 | 99.12 |
| libx264 medium crf28 | 4.4 | 68 | 94.39 |
| libx265 medium crf22 | 10.0 | 27 | 99.33 |
| libx265 medium crf28 | 3.6 | 39 | 95.00 |
| libsvtav1 p6 crf30 | 8.7 | 25 | 99.70 |
| libsvtav1 p6 crf38 | 4.5 | 26 | 98.86 |
| native 3840×3840 10-bit → hevc_nvenc p4 cq26 (nvdec+nvenc) | 48.0 | 66 | 99.59 |

Reading (VMAF ≥ 99 ≈ transparent for delivery, ≥ 97 fine for social):
- **YouTube 1080p master: hevc_nvenc cq 26–28 (≈10 Mbps) or av1_nvenc cq 32–34 (≈8 Mbps) or libx264 crf 20–21**; h264_nvenc needs cq ≤ 25 for the same quality (≈16 Mbps).
- **Shorts / proxies: hevc_nvenc cq 30 (6.3 Mbps, 99.2) or h264_nvenc cq 29–31**.
- At equal VMAF ≈ 99.1: x264 crf23 9.7 Mbps ≈ hevc_nvenc cq30 6.3 Mbps ≈ av1_nvenc cq38 4.9 Mbps ≈ svtav1 crf38 4.5 Mbps → AV1 halves the bitrate of x264 crf23 on this footage.
- The p7 "delivery" profile bought nothing over p6 on this clip; keep p6 unless a VMAF check on the actual project says otherwise. Real-footage encode fps here is lower than the testsrc2 table (1920×1920 square frames, GPU 0 shared).
- 4K-class native HEVC 10-bit → hevc_nvenc cq26 keeps VMAF 99.6 at 48 Mbps, 66 fps on the shared GPU.

## What the trac encoding guides say [doc-trac, fetched 2026-09-01]
- libx264: "CRF scale is 0–51, 0 is lossless (8-bit only; 10-bit use -qp 0), 23 is the default"; "a subjectively sane range is 17–28. Consider 17 or 18 to be visually lossless"; "+6 results in roughly half the bitrate"; "the 0–51 scale only applies to 8-bit x264"; presets ultrafast…veryslow, "use the slowest preset you have patience for", "placebo – ignore"; tune film/animation/grain/stillimage/fastdecode/zerolatency; omit `-profile:v` unless a device needs it (High is what modern devices support; matching profiles matters when the concat demuxer joins files); `-x264-params keyint=123:min-keyint=20`; two-pass `-pass 1 -an -f null /dev/null && -pass 2`; CBR for TS: `-x264-params "nal-hrd=cbr" -b:v 1M -minrate 1M -maxrate 1M -bufsize 2M`; CRF with a cap: `-crf 23 -maxrate 1M -bufsize 2M`. NVENC example from the same page: `-c:v h264_nvenc -qp 15 -profile:v high444p -pix_fmt yuv444p -tune hq -preset p7 -rc constqp -rc-lookahead 32`.
- libx265: "the default is 28, and it should visually correspond to libx264 video at CRF 23, but result in about half the file size"; max is always 51 even for 10-bit; two-pass uses `-x265-params pass=1|2`, not `-pass`; lossless `-x265-params lossless=1`.
- AV1 (libaom): "A CRF value of 23 yields a quality level corresponding to CRF 19 for x264"; `-b:v 0` needed on old ffmpeg for pure CRF; `-cpu-used` default 1, 7–10 realtime only (ffmpeg caps at 8); keyframe interval: "anything up to 10 seconds is considered reasonable" (`-g 300` at 30 fps), `-keyint_min` ignored unless equal to `-g`; libsvtav1 `-crf 35 -preset 8` reference command, CRF is the default rate control; AMF (`h264/hevc/av1_amf`) quality knobs `-quality speed|balanced|quality`, `-usage transcoding|lowlatency`.
- YouTube page: `ffmpeg -i input -c:v libx264 -preset slow -crf 18 -c:a aac -b:a 192k -pix_fmt yuv420p output.mkv`; "`-movflags +faststart` is a no-op for Matroska/WebM — use `-cues_to_front 1` instead"; still image + audio: `-loop 1 -framerate 2 -i img.png -i audio.m4a -c:v libx264 -tune stillimage -crf 18 -c:a copy -shortest -pix_fmt yuv420p`; upscale 2× with `scale=iw*2:ih*2:flags=neighbor` for "higher peak quality" on YouTube.
- HWAccelIntro: hardware encoders "require a higher bitrate to make output with the same perceptual quality" as software; "scale_npp has been replaced by scale_cuda" (matches the missing NPP here); Windows AMD path = DXVA2/D3D11VA decode + AMF encode; `-noautoscale` is needed when a `*_cuda` filter changes the size inside `-filter_complex`.

## The delivery profiles (copy these) [measured to run; quality per above]
```
# GPU, quality-first ("p7 delivery")
-c:v hevc_nvenc -preset p7 -tune hq -rc vbr -cq 24 -b:v 0 -multipass fullres -rc-lookahead 32 -spatial-aq 1 -temporal-aq 1 -b_ref_mode middle -g 60 -pix_fmt p010le -profile:v main10
# GPU, fast proxy / previews
-c:v h264_nvenc -preset p4 -tune hq -rc vbr -cq 27 -b:v 0 -g 60
# GPU AV1 (Blackwell/Ada only)
-c:v av1_nvenc -preset p6 -tune hq -rc vbr -cq 32 -b:v 0 -g 60
# CPU H.264 master
-c:v libx264 -preset slow -crf 18 -pix_fmt yuv420p -g 60 -keyint_min 60 -sc_threshold 0
# CPU HEVC
-c:v libx265 -preset medium -crf 22 -pix_fmt yuv420p10le -x265-params keyint=60:min-keyint=60
# CPU AV1
-c:v libsvtav1 -preset 6 -crf 32 -pix_fmt yuv420p10le -svtav1-params tune=0:keyint=60
```
Measured cost (1080p, workstation): p7 profile ≈ 100 fps hevc / 195 fps h264; p4 ≈ 300–370 fps; libx264
slow 112 fps; libx265 medium 61 fps; svtav1 p6 72 fps. On the Linux node (RTX 3050): h264/hevc p4
≈ 300 fps, x264 medium 104 fps, no AV1 NVENC.

## Two-pass (CPU, target bitrate) [measured]
```
ffmpeg -y -i in.mp4 -c:v libx264 -preset medium -b:v 5000k -pass 1 -passlogfile x264pass -an -f null NUL      # Windows (PS/cmd/bash all fine); Git Bash also accepts /dev/null and -
ffmpeg -y -i in.mp4 -c:v libx264 -preset medium -b:v 5000k -pass 2 -passlogfile x264pass -c:a aac out.mp4
→ 5.003 Mbps; log files x264pass-0.log, x264pass-0.log.mbtree (delete after)
```
libx265: same flags plus `-x265-params pass=1` / `pass=2` [doc]. SVT-AV1: `-svtav1-params pass=1|2`
or just CRF. Two-pass is for a bitrate budget; for quality use CRF/CQ.

## GOP / keyframes [measured]
```
libx264:  -g 60 -keyint_min 60 -sc_threshold 0 -force_key_frames "expr:gte(t,n_forced*2)"  → keyframes 0,2,4,6 s
nvenc:    -g 60 -forced-idr 1 -force_key_frames "expr:gte(t,n_forced*2)"                   → keyframes 0,2,4,6 s
```
2 s GOP is what YouTube and HLS/DASH expect [community]; it is also what makes `-c copy` cuts
land on whole seconds (03). Verify with the keyframe probe (02).

## Full-GPU pipelines [measured]
```
-hwaccel cuda -hwaccel_output_format cuda -i in.mp4 -c:v h264_nvenc …                          # nvdec → nvenc, no CPU frames: 388 fps 1080p
-hwaccel cuda -hwaccel_output_format cuda -i 4k.mp4 -vf "scale_cuda=1920:1080:interp_algo=lanczos" -c:v hevc_nvenc …   # 258 fps (workstation), 43 fps (Linux node)
-hwaccel cuda -i in.mp4 -vf "subtitles=x.srt" -c:v h264_nvenc …                                  # GPU decode, CPU filter (frames downloaded automatically), GPU encode — works
-init_hw_device vulkan -i in.mp4 -vf "hwupload,libplacebo=w=1920:h=1080:downscaler=lanczos,hwdownload,format=yuv420p" -c:v libx264 …   # libplacebo works on the workstation (Vulkan)
```
Rules: with `-hwaccel_output_format cuda` frames stay on the GPU and only `*_cuda` filters
(`scale_cuda`, `overlay_cuda`, `hwdownload`, `hwupload_cuda`) apply; drop that option when you
need CPU filters (decode still on GPU). `scale_npp` is absent; `overlay_cuda` refuses rgba (07).
Decoder names `h264_cuvid` etc. exist but `-hwaccel cuda` is the modern form [doc HWAccelIntro].

## ProRes / DNxHR / lossless [measured]
`prores_ks -profile:v 3` → ProRes 422 HQ, 79.9 Mbps at 1080p30 (testsrc2); `-profile:v 4444
-pix_fmt yuva444p10le` for alpha; `dnxhd -profile:v dnxhr_hq -pix_fmt yuv422p`. Lossless
H.264: `libx264 -qp 0` (huge, exact) [doc]; NVENC `-tune lossless`.

## Verify an encode (always)
ffprobe codec/profile/pix_fmt/size/fps, `-count_frames` equals the source, bitrate in the
expected band, keyframes where planned, VMAF/SSIM against the source on real footage when the
setting is new, audio untouched (`-c:a copy`) or re-normalized (06).

## Gaps (not measured here)
- laptop node (RTX 3070) and editing rig (RTX 5060) throughput and encoder lists — hosts offline; expected h264/hevc NVENC on both, AV1 only on the 5060 [inferred].
- Quality on real footage is measured above on ONE DJI clip (square 360-camera frame, daylight); re-run `measurements-2026-09-01/real.sh` on a talking-head / night clip before locking a CQ for those.
- ffmpeg 9.0.1 (`D:\Dev\tools`) behaviour — nothing re-run there.
- AMF/QSV/MediaFoundation encoders on the workstation (present, unused — NVIDIA-only box) and QSV on the Linux node iGPU.
- `-tune uhq` and `-lookahead_level` effect; NVENC 4:2:2 / 4:4:4 modes; hevc_nvenc B-frame counts per preset.
