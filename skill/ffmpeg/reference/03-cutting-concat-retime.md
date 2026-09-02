# 03 — Cutting, concatenation, retime, pitch

All measured 2026-09-01 on the workstation, ffmpeg 8.1.2, `src1080.mp4` (30 fps, keyframes every
2.0 s), `gaps.mp4` (stereo, 1 s silences at 3,7,11,15,19 s). Source recipes are in 02.

## Seeking semantics (the rules) [doc, ffmpeg.html]
- `-ss` **before** `-i` (input option): seeks in the input; with `-accurate_seek` (default) the segment between the previous keyframe and the requested position is decoded and discarded — **only when transcoding**. "When doing stream copy or when `-noaccurate_seek` is used, it will be preserved."
- `-ss` **after** `-i` (output option): decodes but discards input until the timestamps reach position (slow, exact for transcodes).
- `-to` is an END position "in the output"; after an input `-ss` the output timeline starts at 0, so `-to` behaves as a duration unless `-copyts` is set [measured below]. `-t` is a duration. `-sseof -N` seeks relative to end of file.
- `-copyts` keeps input timestamps (no reset to zero); `-start_at_zero` shifts them back to 0 while keeping the copyts semantics.

What the trac Seeking page adds [doc-trac, digested by the Linux node seat 2026-09-01 and spot-checked]:
"As of FFmpeg 2.1, when transcoding: `-ss` is also frame-accurate even as input option" (old
keyframe-only behaviour via `-noaccurate_seek`); "when `-ss` used before `-i` only, the timestamps
will be reset according to 0, so `-t` and `-to` shall be the same; to keep original timestamps
unmodified add `-copyts`"; "`-copyts` takes precedence over `-avoid_negative_ts` when both
specified"; "Enable `-avoid_negative_ts` when `-c copy` alike (stream copy) if intended to reuse with
the concat demuxer"; "Using `-ss` with `-c copy` may not be accurate since ffmpeg may only split on
I-frames … it may auto-adjust the stream's start time to negative to compensate — so be careful
when splitting and doing codec copy." Its reference commands:
`ffmpeg -ss 1:00 -i video.mp4 -to 2:00 -c copy cut.mp4` (= 1:00→3:00, fast/inaccurate),
`… -to 2:00 -c copy -copyts cut.mp4` (= 1:00→2:00), `ffmpeg -i video.mp4 -ss 1:00 -to 2:00 -c copy cut.mp4`
(slow/accurate), `ffmpeg -ss 3:00 -i video.mp4 -t 60 -c copy -avoid_negative_ts 2 cut.mp4`.

## Cut WITH re-encode = frame accurate [measured]
```
ffmpeg -y -ss 3 -i src1080.mp4 -t 4 -c:v libx264 -preset veryfast -crf 18 -c:a aac cut_reenc.mp4
start_time=0.000000  duration=4.000000  nb_frames=120   first frames pts 0.000000, 0.033333
```
Fast (input seek to keyframe 2.0, decodes 30 discarded frames) and exact. Use this whenever the
cut point matters. With NVENC: same command, `-c:v h264_nvenc -preset p4 -cq 23 -b:v 0`.

## Cut WITHOUT re-encode = keyframe trap [measured]
`-ss 3 -i … -t 4 -c copy` (input seek):
```
format.start_time=0.000000  duration=4.100000  video nb_frames=152 (expected 120)
packets: -1.000000,KD_  -0.933333,_D_  -0.966667,_D_ …   ← the whole GOP from 2.0 s, timestamps shifted so 3.0 = 0, packets before 0 flagged D(iscard)
first decoded frame: 0.000000,P
```
So the file physically contains 1 s of extra video with negative pts; players honour the edit
list/discard flags and start at 0, but frame counts, concat and some players do not. Variants:
- `-avoid_negative_ts make_zero` shifts everything positive: `duration=5.146680`, `nb_frames=152` → the clip visibly starts at 2.0 s content and runs 5.1 s. Predictable but early.
- `-ss` **after** `-i` with `-c copy` (`-i … -ss 3 -t 4 -c copy`): `nb_frames=92`, `duration=4.058`, first video packet `1.000000,K` → the container starts at 0 with audio, and video only appears at 1.0 s (next keyframe 4.0). A hole, not a cut.
- Accept the trap only when cut points are keyframes: put them there with `-force_key_frames` at encode time (05) or cut by keyframe list (02, keyframe positions).

Rule: copy-cut only on keyframe-aligned times, or accept ≤ 1 GOP of early material; otherwise re-encode.

## `-to` after an input `-ss` [measured]
```
-ss 3.5 -i src1080.mp4 -to 7.5 …            → duration 7.500000  (7.5 s from the seek point, i.e. content 3.5–11.0)
-ss 3.5 -i src1080.mp4 -to 7.5 -copyts …    → start_time=3.478000 duration=4.032000 (content 3.5–7.5, timestamps preserved)
```
Prefer `-t <duration>` and compute it yourself; if you must use absolute `-to`, add `-copyts`
and then `setpts=PTS-STARTPTS`/`asetpts=PTS-STARTPTS` (or `-start_at_zero`) so the output starts at 0.

## Concat demuxer (no re-encode) [measured + doc]
List file (`list.txt`), relative paths resolve against the list file's directory [doc]:
```
file cut_reenc.mp4
file cut_copy_after.mp4
```
(Quote with single quotes when names have spaces/specials: `file 'my clip.mp4'`.)
```
ffmpeg -y -f concat -safe 0 -i list.txt -c copy concat_demux.mp4
nb_read_frames=212  duration=8.079688     (120 + 92 frames; the second clip's 1 s hole came along)
```
`-safe 0` is required for absolute paths or paths with special characters [doc]. Requirement:
"All files must have the same streams (same codecs, same time base, etc.)" [doc]. What happens when
they don't (720p + 1080p, both h264 yuv420p):
```
[aost#0:1/copy] Non-monotonic DTS; previous: 96256, current: 96000; changing to 96257. This may result in incorrect timestamps…
ffprobe says 1280,720 for the whole file; decoding it produced no error either
```
i.e. a silently wrong file (header says 720p, second half is 1080p). Gate: ffprobe every input
first and refuse the demuxer unless codec, width, height, pix_fmt, fps, sample_rate, channels
match. The demuxer also supports `inpoint`/`outpoint`/`duration` directives per file
(keyframe-accurate only for inter-coded input [doc]).

Trac Concatenate page [doc-trac]: the demuxer works across different containers as long as the
streams match; the concat **protocol** (`-i "concat:a.ts|b.ts"`) is for MPEG-TS/raw level formats
only — for MP4 inputs go through `-c copy -bsf:v h264_mp4toannexb -f mpegts` intermediates and
back with `-bsf:a aac_adtstoasc`; "Filters are incompatible with stream copying"; "for the concat
filter to work, the inputs have to be of the same frame dimensions and should have the same
framerate"; "all segments must have the same number of streams of each type".

## Concat filter (re-encode, anything goes) [measured]
```
ffmpeg -y -i small720.mp4 -i cut_reenc.mp4 -filter_complex "[0:v]scale=1920:1080,setsar=1[v0];[1:v]setsar=1[v1];[v0][0:a][v1][1:a]concat=n=2:v=1:a=1[v][a]" -map "[v]" -map "[a]" -c:v libx264 -preset veryfast -crf 18 -c:a aac concat_filter.mp4
width=1920 height=1080 nb_read_frames=180 duration=6.016000   (60 + 120 frames)
```
Inputs must reach `concat` with identical size/SAR/pix_fmt/fps (normalize with `scale`, `setsar=1`,
`fps=30`, `format=yuv420p`, audio `aformat=sample_rates=48000:channel_layouts=stereo`). Input
order in the pad list is v,a,v,a… per segment [doc].

## Crossfade between two clips [measured]
```
ffmpeg -y -i a.mp4 -i b.mp4 -filter_complex "[0:v][1:v]xfade=transition=fade:duration=1:offset=3[v];[0:a][1:a]acrossfade=d=1[a]" -map "[v]" -map "[a]" -c:v libx264 -preset veryfast -crf 18 -c:a aac xfade.mp4
duration=7.021000   (4 s + 4.06 s − 1 s overlap)
```
`offset` = time in the FIRST clip where the transition starts (= first duration − fade duration).
Both inputs need the same size/fps/pix_fmt and constant frame rate [doc]. Transition names
include fade, wipeleft/right/up/down, slideleft/…, dissolve, pixelize, radial, circleopen/close,
smoothleft/…, fadeblack, fadewhite [doc].

## Speed up / slow down [measured]
```
# 2× (video PTS halved, audio tempo doubled; drops every other frame, keeps 30 fps)
ffmpeg -y -i gaps.mp4 -filter_complex "[0:v]setpts=0.5*PTS[v];[0:a]atempo=2.0[a]" -map "[v]" -map "[a]" -c:v libx264 -preset veryfast -crf 18 -c:a aac speed2x.mp4
r_frame_rate=30/1 nb_read_frames=302 duration=10.066667

# 0.5× of a 4 s cut (frames duplicated to keep 30 fps: 120 → 60 unique; audio tempo halved)
ffmpeg -y -i src1080.mp4 -t 4 -filter_complex "[0:v]setpts=2.0*PTS[v];[0:a]atempo=0.5[a]" -map "[v]" -map "[a]" … slow05.mp4
nb_read_frames=60  duration=4.000000   ← `-t 4` was applied as an OUTPUT limit here, so only the first 2 s of source survived at 0.5×; put `-t` before `-i` (input limit) to slow a whole 4 s cut into 8 s / 240 frames
```
Caveats [doc + measured]: `atempo` accepts 0.5–100 (chain two for 4×: `atempo=2.0,atempo=2.0`);
`setpts` alone drops/duplicates frames — for true slow-motion interpolate:
`setpts=2.0*PTS,minterpolate=fps=30:mi_mode=mci` (CPU heavy; 4 s of 1080p took tens of seconds on
the workstation). To keep every source frame when speeding up, raise the output rate: `-r 60` with `setpts=0.5*PTS`.
Verify: expected frames = source frames × (1/factor) at the same fps; duration = source/factor.

## Pitch vs tempo [measured, 4 s of gaps.mp4]
| Filter | Effect | Output duration |
|---|---|---|
| `asetrate=48000*1.25,aresample=48000` | pitch AND speed up 25 % (chipmunk) | 3.2 s expected — measured **4.000** because `-t 4` was applied after the filter; without `-t`, duration shrinks |
| `rubberband=pitch=1.25` | pitch up, same length | 4.000 |
| `rubberband=tempo=2.0` | 2× speed, pitch preserved (higher quality than atempo) | measured 4.000 with `-t 4` on the output; 2.0 without |
Rule: put `-t` BEFORE `-i` (input option) when you want "take N seconds of source", after when you want "produce N seconds".
`rubberband` is present on the workstation and the Linux node [measured].

## Trim by frame numbers [doc]
`-vf trim=start_frame=90:end_frame=210,setpts=PTS-STARTPTS` (+ `atrim=start=3:end=7,asetpts=PTS-STARTPTS`) — exact, re-encode only. Or `select='between(n,90,209)'`.

## Dead-air cut list (video+audio in sync) — see 06 §Dead air
`select`/`aselect` with a `between(t,a,b)+between(t,c,d)…` expression built from `silencedetect`;
measured 20 s → 16.533 s, video and audio durations identical, zero remaining silences.
