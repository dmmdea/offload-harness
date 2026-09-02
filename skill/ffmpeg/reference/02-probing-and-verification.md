# 02 — Probing and verification

Every command below was run on the workstation (ffmpeg/ffprobe 8.1.2) on 2026-09-01 against
`src1080.mp4` (testsrc2 1920×1080 30 fps + 440 Hz sine, 20 s, libx264 crf18, GOP 60) and
`gaps.mp4` (same video, stereo audio with a 3 s tone / 1 s digital-silence pattern). Output
shown is the real output, trimmed.

## Test clips (regenerate anywhere, no footage needed) [measured]
```
ffmpeg -y -f lavfi -i "testsrc2=size=1920x1080:rate=30" -f lavfi -i "sine=frequency=440:sample_rate=48000" -t 20 -c:v libx264 -preset fast -crf 18 -g 60 -keyint_min 60 -sc_threshold 0 -pix_fmt yuv420p -c:a aac -b:a 192k src1080.mp4     # 3.2 s, 25.4 MB
ffmpeg -y -f lavfi -i "testsrc2=size=3840x2160:rate=30" -f lavfi -i "sine=frequency=440:sample_rate=48000" -t 10 -c:v libx264 -preset fast -crf 18 -g 60 -pix_fmt yuv420p -c:a aac -b:a 192k src4k.mp4                                   # 5.6 s, 47.6 MB
ffmpeg -y -f lavfi -i "testsrc2=size=1920x1080:rate=30" -f lavfi -i "aevalsrc='if(lt(mod(t,4),3),0.3*sin(2*PI*220*t),0)|if(lt(mod(t,4),3),0.3*sin(2*PI*330*t),0)':s=48000:c=stereo" -t 20 -c:v libx264 -preset fast -crf 18 -g 60 -pix_fmt yuv420p -c:a aac -b:a 192k gaps.mp4
ffmpeg -y -f lavfi -i "sine=frequency=110:sample_rate=48000" -t 20 -af "volume=0.5,aformat=channel_layouts=stereo" -c:a pcm_s16le music.wav
```
`-sc_threshold 0 -keyint_min 60 -g 60` gives keyframes at exactly 0,2,4,… s [measured]. Note
`lavfi` does not appear in `ffmpeg -formats` (it is a libavdevice input) but `-f lavfi` works.

## Streams + format as JSON [measured]
```
ffprobe -v error -show_streams -show_format -of json src1080.mp4
```
Fields worth reading (video stream): `codec_name h264, profile High, width 1920, height 1080,
pix_fmt yuv420p, r_frame_rate 30/1, avg_frame_rate 30/1, time_base 1/15360, nb_frames "600",
duration "20.000000", bit_rate "10017601", start_time "0.000000"`. Audio: `aac, LC, sample_rate
48000, channels 1, channel_layout mono, nb_frames "939"` (AAC frames, not samples). Format:
`format_name "mov,mp4,m4a,3gp,3g2,mj2", duration, size, bit_rate, nb_streams`.
- `nb_frames` is a container hint (mp4/mov have it; mkv, ts, webm, raw streams usually do not) [doc + measured]. The exact number is `-count_frames` → `nb_read_frames`.
- ffprobe prints numbers as strings in JSON; parse them.

Compact one-liners [measured]:
```
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 f.mp4            # 20.000000
ffprobe -v error -sexagesimal -show_entries format=duration -of default=nw=1:nk=1 f.mp4  # 0:00:20.000000
ffprobe -v error -select_streams v:0 -show_entries stream=width,height,r_frame_rate,pix_fmt -of csv=p=0 f.mp4   # 1920,1080,30/1,yuv420p
ffprobe -v error -select_streams a -show_entries stream=codec_name,channels,sample_rate -of csv=p=0 f.mp4
```
`-of default=nw=1:nk=1` = no wrappers, no keys; `-of csv=p=0` = no section prefix [doc].

## Frame-accurate duration and frame count [measured]
```
ffprobe -v error -count_frames -count_packets -select_streams v:0 -show_entries stream=nb_frames,nb_read_frames,nb_read_packets -of default=nw=1 src1080.mp4
nb_frames=600
nb_read_frames=600
nb_read_packets=600
```
`-count_frames` decodes the whole stream (slow on long files, exact). Frame-accurate duration
= `nb_read_frames / fps` when the rate is constant; for VFR sum packet durations
(`-show_entries packet=duration_time`). The audio stream's `duration` can differ from video by
one AAC frame (`joined.mp4`: video 20.000, mp3 output 20.0107 because of encoder padding).

## Keyframe positions [measured]
```
ffprobe -v error -select_streams v:0 -skip_frame nokey -show_entries frame=pts_time -of csv=p=0 src1080.mp4
0.000000 2.000000 4.000000 … 18.000000
ffprobe -v error -select_streams v:0 -show_entries packet=pts_time,flags -of csv=p=0 src1080.mp4 | grep ',K'
0.000000,K__  2.000000,K__ …
```
The packet form is fast (no decode); `flags` `K` = keyframe, `D` = discard-tagged (appears on the
negative-timestamp packets after a copy cut, see 03).

## Loudness: EBU R128 measurement [measured]
```
ffmpeg -hide_banner -nostats -i gaps.mp4 -af "ebur128=peak=true" -f null -
  Integrated loudness:  I: -11.7 LUFS   Threshold: -21.8 LUFS
  Loudness range:       LRA: 1.6 LU
  True peak:            Peak: -6.9 dBFS   (printed further down the summary)
ffmpeg -hide_banner -nostats -i gaps.mp4 -af volumedetect -f null -     # mean_volume: -14.7 dB  max_volume: -6.9 dB  (sample peak, not LUFS)
```
Loudnorm pass 1 (the JSON you feed into pass 2, see 06):
```
ffmpeg -hide_banner -nostats -i gaps.mp4 -af "loudnorm=I=-14:TP=-1:LRA=11:print_format=json" -f null -
{ "input_i":"-11.76", "input_tp":"-6.95", "input_lra":"1.70", "input_thresh":"-21.90", "output_i":"-14.08", "output_tp":"-9.63", "output_lra":"1.80", "output_thresh":"-24.21", "normalization_type":"dynamic", "target_offset":"0.08" }
```
The JSON is the last thing on stderr; grab it with `sed -n '/^{/,/^}/p'` (bash) or a regex.

## Dead air / silence [measured]
```
ffmpeg -hide_banner -nostats -i gaps.mp4 -af "silencedetect=noise=-50dB:d=0.5" -f null -
silence_start: 3 | silence_end: 4.000021 | silence_duration: 1.000021   (×5, at 3,7,11,15,19)
```
`noise` is the threshold (dB or ratio), `d` the minimum duration. Digital silence measures
`Peak level dB: -inf` with `astats`. For a "silent open" check, look for a `silence_start: 0`.

## Repeated / frozen frames [measured]
```
ffmpeg -hide_banner -nostats -i frozen.mp4 -vf "freezedetect=n=0.001:d=1" -an -f null -
lavfi.freezedetect.freeze_start: 1.966667   freeze_duration: 2   freeze_end: 3.966667
```
(`frozen.mp4` = testsrc2 with frame 60 looped 59× via `loop=loop=59:size=1:start=60`.)
`n` is the noise tolerance (0.001 ≈ −60 dB), `d` the minimum freeze. To count unique frames:
`-vf mpdecimate,showinfo` and count `showinfo` lines (a 0.5× slow-mo of 60 frames stretched to
120 kept 125 "unique" frames after decoding noise; use `mpdecimate=hi=…` thresholds on real content).

## Comparing two renders [measured]
```
ffmpeg -hide_banner -nostats -i a.mp4 -i b.mp4 -lavfi "[0:v][1:v]ssim" -f null -      # SSIM All:1.000000 for identical
ffmpeg -hide_banner -nostats -i dist.mp4 -i ref.mp4 -lavfi "[0:v][1:v]libvmaf=feature=name=psnr|name=float_ssim" -f null -   # VMAF score: 60.96
```
libvmaf's first input is the DISTORTED, second the REFERENCE [doc]. Absolute VMAF on testsrc2 is
meaningless (~60 even at 15 Mbps — synthetic content); use it for RELATIVE comparisons on real
footage. Region/pixel check for overlays and text: crop the region and read `signalstats` YAVG,
or diff two frames:
```
ffmpeg -hide_banner -nostats -i out.png -i plain.png -lavfi "[0:v][1:v]blend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-" -f null -
lavfi.signalstats.YAVG=0.104633     # 0 = pixel-identical = nothing was drawn
```

## Stills for eyes-on QA [measured]
```
ffmpeg -y -ss 7.5 -i src1080.mp4 -frames:v 1 -q:v 2 still_7.5.jpg     # exact frame (input seek + decode) — 130 KB
ffmpeg -y -ss 7.5 -i src1080.mp4 -frames:v 1 still_7.5.png             # lossless, 319 KB
ffprobe -v error -show_entries frame=pts_time -of csv=p=0 -read_intervals "7.5%+#1" src1080.mp4   # prints the keyframe before (5.93), not 7.5 — read_intervals seeks by keyframe
```
Hand stills to `offload_vqa` / `offload_video_describe` for semantic checks; the media lanes
exist for this.

## The verification gate (run after EVERY render; a return code is not verification)
1. `ffprobe -show_streams -show_format -of json` → codec/size/pix_fmt/fps/channels are what was asked. Compare `format.duration` to the expected value (±1 frame + audio priming ≈ ±0.05 s).
2. `-count_frames` → `nb_read_frames == round(expected_seconds × fps)`; for a cut, compare with `-t × fps` (a `-t 4` copy cut gave 152 frames instead of 120 — see 03).
3. Audio: `ebur128` integrated within ±0.5 LU of target, true peak ≤ target; `silencedetect` shows no gap longer than the editorial allowance and no `silence_start: 0`.
4. Video: `freezedetect` reports nothing (or only intended holds); stills at start/middle/end and at every seam; for text/overlay renders a pixel diff > 0 in the region.
5. Container: `moov` before `mdat` for web delivery (`-movflags +faststart`; measured `moov@36`), metadata/language tags/dispositions as intended, chapters present if written.
6. Only then say "done", and quote the numbers.
