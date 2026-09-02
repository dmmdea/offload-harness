# FFmpeg / ffprobe autonomous-pipeline reference (skill documentation)

Built 2026-09-01 from: the official ffmpeg.org documentation (ffmpeg.html, ffprobe.html,
ffmpeg-filters.html, ffmpeg-formats.html, ffmpeg-utils.html, downloaded and read that day),
`ffmpeg -h encoder=…` / `-filters` / `-encoders` dumps of the live builds, and ~180 commands
run live on the workstation (ffmpeg 8.1.2 Gyan full build) and the Linux node (ffmpeg 8.0.1 Ubuntu) on
lavfi-generated clips (`testsrc2` + `sine` / `aevalsrc`, no private footage). Every claim is
tagged: **[measured]** = observed live on our machines (date given when it matters),
**[doc]** = official ffmpeg.org text, **[community]** = widely reported third-party
practice not re-verified here, **[inferred]** = my reasoning, verify before relying.

Purpose: stop re-researching filter syntax, stop guessing quoting, make every media output
verifiable by ffprobe/ebur128/frame counts instead of "exit code 0".

## Reading order (load only what the task needs — this folder is NOT auto-loaded)

| File | Load when |
|---|---|
| `01-hosts-builds.md` | any session: which box has which ffmpeg, encoders, GPU, throughput numbers, the second ffmpeg 9.0.1 on the workstation |
| `02-probing-and-verification.md` | reading a file (streams/format JSON, exact frame counts, keyframes, loudness, silence, frozen frames) and the verification gate every output must pass |
| `03-cutting-concat-retime.md` | cutting (copy vs re-encode, the keyframe trap, `-ss` placement), concat demuxer vs filter, xfade, speed/slow, pitch |
| `04-scaling-formats-delivery.md` | 16:9 / 9:16 / thumbnails, fit+pad, scale flags, fps, GIF/WebP, image sequences, stills, intermediates, YouTube/Shorts delivery recipes |
| `05-encoding-nvenc-cpu.md` | NVENC presets/tune/rc/CQ, libx264/x265/SVT-AV1 CRF, two-pass, measured fps and size/quality points, GOP control |
| `06-audio.md` | -14/-16 LUFS two-pass loudnorm, silence removal, dead-air cut list, ducking with sidechaincompress, channel mapping, joining audio |
| `07-subtitles-text-overlays.md` | burn SRT/ASS, soft-mux, drawtext lower thirds, alpha PNG / ProRes 4444 overlays, the `%` and `-ss` traps |
| `08-windows-quoting-and-scripting.md` | any filtergraph typed in PowerShell / cmd / Git Bash; exit codes per shell; `-progress` parsing; `-filter_script` |
| `09-failure-modes.md` | anything that "ran fine" but is wrong; the catalog of traps with the fix |
| `live-dump-<workstation>-2026-09-01.json`, `live-dump-<linux-node>-2026-09-01.json` | exact encoder/decoder/filter/format availability, hwaccels, NVENC option enums per host |

## The ten rules (memorize; the rest of the folder is detail)

1. **Exit code 0 is not verification.** `drawtext` with a `%` in the text renders NOTHING and exits 0; `subtitles=` after an input `-ss` renders nothing and exits 0; a `-c copy` cut starts on a keyframe up to 2 s early and exits 0. Verify every output in its own medium: ffprobe frame count and duration, ebur128 for loudness, `silencedetect` for dead air, `freezedetect` for repeated frames, a pixel diff for text/overlays. **[measured 2026-09-01]**
2. **Never write `fontfile=C\:/…` unquoted.** On 8.1.2 it is a parse error in every shell. Working forms: `fontfile='C\:/Windows/Fonts/arial.ttf'` (quoted inside the graph), `fontfile=C\\:/…` (two backslashes reaching ffmpeg), or a drive-less `/Windows/Fonts/arial.ttf` when cwd is on C:. `font=Arial` (fontconfig) **segfaults** this build (0xC0000005). **[measured on bash, PS 5.1, pwsh 7, cmd]**
3. **Git Bash rewrites arguments that look like paths.** `subtitles=/Users/x.srt` becomes `C:/Program Files/Git/Users/x.srt`; `C\\:/Users/…` becomes `C\\;C:\Program Files\Git\Users\…`. Use relative paths, `-filter_script`, or `MSYS_NO_PATHCONV=1`. PowerShell and cmd pass them untouched. **[measured]**
4. **Frame-accurate cut = re-encode; `-c copy` cut = keyframe-aligned.** `-ss` before `-i` with copy keeps the whole GOP and emits negative timestamps (first packet pts −1.0 s for `-ss 3` on a 2 s GOP); `-ss` after `-i` with copy drops the packets but leaves a hole with no video until the next keyframe. `-ss` before `-i` + re-encode is exact (120 frames for `-t 4` at 30 fps). **[measured]**
5. **Input `-ss` resets timestamps to 0.** Anything keyed to absolute time (`subtitles`, `ass`, `enable=between(t,…)`, `-to`) then refers to seek-relative time. Add `-copyts` (and `setpts=PTS-STARTPTS` after the filter) or seek on the output side. `-to` after an input `-ss` is a duration, not an end time (`-ss 3.5 … -to 7.5` gave 7.5 s). **[measured]**
6. **Loudness is a two-pass job and a measured result.** Pass 1 `loudnorm=…:print_format=json`, pass 2 with `measured_*`+`offset`+`linear=true`, then re-measure with `ebur128`. Two-pass to −14 landed at −13.9, to −16 at −16.4, single-pass at −14.0 on the synthetic clip; accept ±0.5 LU, fail outside. **[measured]**
7. **Real-footage CQ points (a DJI 360 clip, VMAF vs near-lossless reference): hevc_nvenc cq 26–28 ≈ 10 Mbps at VMAF 99.8–99.6, av1_nvenc cq 34 ≈ 7.5 Mbps at 99.7, h264_nvenc cq 23 ≈ 20 Mbps at 99.9, libx264 crf 23 ≈ 9.7 Mbps at 99.1; p7+multipass gained nothing over p6.** NVENC quality knobs that matter: `-preset p1…p7` (p4 default, p7 slowest/best), `-tune hq`, `-rc vbr -cq N -b:v 0` for constant quality, `-multipass fullres -rc-lookahead 32 -spatial-aq 1 -temporal-aq 1 -b_ref_mode middle` for the "p7 delivery" profile. Measured 1080p on the workstation: h264 p4 372 fps, p7 195 fps; hevc p4 306 fps, p7 102 fps; av1 p4 365 fps. `av1_nvenc` does not exist on the RTX 3050 (Linux node). **[measured]**
8. **Concat demuxer only for identical streams; otherwise the concat filter.** Mismatched sizes through the demuxer produce a file whose header lies (ffprobe says 1280×720 for a stream that switches to 1920×1080) with only a "Non-monotonic DTS" warning. `concat=n=…:v=1:a=1` with `scale`+`setsar=1` per input is the safe path. **[measured]**
9. **Silence removal in one pass is `silenceremove`, but the kept silence = `stop_duration + stop_silence`.** For voice, generate a cut list from `silencedetect` and cut with `select`/`aselect` (keeps A/V in sync; measured 20 s → 16.53 s with zero remaining gaps). **[measured]**
10. **Exit codes are shell-dependent and low-information.** PowerShell reports the raw AVERROR (−22 EINVAL, −2 ENOENT, −1094995529 INVALIDDATA, −1073741819 segfault); Git Bash reports the low byte (127, 183, 8, 139). `| Select-Object -First N` on ffmpeg terminates it early and reports −1. `-n` on an existing output prints "already exists" and exits **0**. Test `!= 0`, never a value, and always verify the file. **[measured]**
