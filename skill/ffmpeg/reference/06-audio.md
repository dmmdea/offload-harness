# 06 — Audio: loudness, silence, ducking, channels, joining

Measured 2026-09-01 on the workstation (8.1.2) with `gaps.mp4` (stereo tone with 1 s digital-silence
gaps, integrated −11.7 LUFS) and `music.wav` (110 Hz bed, mean −30.1 dB).

## loudnorm: what the filter is [doc]
EBU R128 normalizer with dynamic (single pass) and linear (two-pass) modes. Defaults `I=-24
LRA=7 TP=-2`. In dynamic mode it upsamples to 192 kHz for true-peak detection — "Use the -ar
option or aresample filter to explicitly set an output sample rate." `linear=true` needs all
four `measured_*` values; if the target LRA is lower than the source LRA or the gain would
exceed TP, it silently reverts to dynamic [doc]. `print_format=json|summary`.

## Two-pass to −14 LUFS (YouTube) [measured]
Pass 1:
```
ffmpeg -hide_banner -nostats -i in.mp4 -af "loudnorm=I=-14:TP=-1:LRA=11:print_format=json" -f null -
→ input_i -11.76  input_tp -6.95  input_lra 1.70  input_thresh -21.90  target_offset 0.08
```
Pass 2 (video copied, audio re-encoded; substitute the measured numbers):
```
ffmpeg -y -i in.mp4 -af "loudnorm=I=-14:TP=-1:LRA=11:measured_I=-11.76:measured_TP=-6.95:measured_LRA=1.70:measured_thresh=-21.90:offset=0.08:linear=true:print_format=summary" -ar 48000 -c:v copy -c:a aac -b:a 192k out.mp4
verify: ebur128 → I: -13.9 LUFS, LRA 1.6 LU
```
Single-pass for comparison (`-af loudnorm=I=-14:TP=-1:LRA=11`): I −14.0 LUFS on this clip. On
real speech the dynamic mode pumps; the two-pass linear mode is a plain gain change and is what
you want for a finished mix. Accept |I − target| ≤ 0.5 LU; otherwise re-run pass 2 with the new
measurement (the offset is the correction).

## Two-pass to −16 LUFS (podcast) [measured]
```
pass 1 with I=-16:TP=-1.5:LRA=11 → feed into
ffmpeg -y -i in.mp4 -vn -af "loudnorm=I=-16:TP=-1.5:LRA=11:measured_I=…:measured_TP=…:measured_LRA=…:measured_thresh=…:offset=…:linear=true" -ar 48000 -c:a libmp3lame -b:a 128k out.mp3
verify: I: -16.4 LUFS, LRA 1.6 LU, duration 20.0107 s (mp3 encoder padding)
```
Targets [community, platform docs not re-fetched]: YouTube −14 LUFS (it attenuates louder
uploads, never boosts quiet ones — a podcast/YouTube channel we run.5 LUFS and sounded 10 LU quiet),
Spotify/Apple podcasts −16 LUFS stereo (−19 mono), true peak −1 dBTP (−2 for lossy delivery).

## Dead-air detection and removal
Detect (02): `silencedetect=noise=-50dB:d=0.5` → five 1.0 s gaps at 3,7,11,15,19 s.

### `silenceremove` one-pass, audio only [measured]
```
-af "silenceremove=stop_periods=-1:stop_duration=0.3:stop_threshold=-60dB"                  → 20 s → 16.60 s, each gap becomes 0.32 s
-af "silenceremove=stop_periods=-1:stop_duration=0.3:stop_threshold=-60dB:stop_silence=0.2"  → 17.60 s, each gap 0.52 s
-af "silenceremove=…:stop_duration=0.5:stop_silence=0.2"                                    → 18.60 s, each gap 0.72 s
-af "silenceremove=stop_periods=-1:stop_duration=0.3:stop_threshold=0"                       → nothing removed (threshold 0 = only exact zero, and the detector window never hits it)
start_periods=1:start_silence=0.1 additionally trims the head.
```
Measured rule: **remaining gap ≈ `stop_duration` + `stop_silence`** (+ ~0.02 s window). `stop_periods=-1`
= remove all mid-file silences [doc]. Use dB thresholds (−50…−60 dB) not `0`. `window=0.01` made the
detection miss two gaps — leave the default window.

### Cut list → `select`/`aselect` (keeps video and audio together) [measured]
1. Run `silencedetect`, parse `silence_start`/`silence_end` pairs.
2. Build keep ranges leaving `pad` (0.15 s) inside each gap: `between(t,0,3.15)+between(t,3.85,7.15)+…`.
3. Cut both streams with the same expression:
```
ffmpeg -y -i gaps.mp4 -filter_complex "[0:v]select='EXPR',setpts=N/FRAME_RATE/TB[v];[0:a]aselect='EXPR',asetpts=N/SR/TB[a]" -map "[v]" -map "[a]" -c:v libx264 -preset veryfast -crf 18 -c:a aac out.mp4
→ 16.533 s video, 16.533 s audio, 0 silences > 0.5 s remaining
```
`setpts=N/FRAME_RATE/TB` and `asetpts=N/SR/TB` re-time the kept frames/samples contiguously
[doc]. For speech, snap cut points to sentence ends (lesson from a shipped podcast: cutting mid-phrase clips words) — the STT word timings from `offload_transcribe` give you those.

## Ducking music under voice (sidechaincompress) [measured]
```
ffmpeg -y -i voice.mp4 -i music.wav -filter_complex "[1:a][0:a]sidechaincompress=threshold=0.02:ratio=8:attack=20:release=300:makeup=1[bed];[0:a][bed]amix=inputs=2:duration=first:normalize=0[a]" -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 192k ducked.mp4
```
First input = the signal being compressed (music), second = the sidechain (voice) [doc].
Measured on the bed alone (`-map "[bed]"`): −49.6 dB while voice plays, −30.8 dB in the gaps
(raw bed −30.1) → ~19 dB of ducking with a 300 ms release. `threshold` is linear (0.000976–1;
0.02 ≈ −34 dBFS, 0.05 ≈ −26 dBFS gave a gentler 13 dB), `ratio` 1–20, `attack`/`release` ms,
`detection=rms|peak`, `link=average|maximum` [doc]. `amix … normalize=0` keeps levels (default
normalize=1 halves each input); `duration=first` ends with the voice. Then loudnorm the mix.
Tip: referencing `[0:a]` twice in the graph worked in 8.1.2 (ffmpeg feeds the stream to both);
older versions need `asplit`.

## Channel mapping [measured]
```
-ac 1                                             stereo → mono (downmix)          → 1,unknown (layout tag not written to WAV; fine)
-af "pan=stereo|c0=c0|c1=c0"                      mono → dual-mono stereo           → 2
-af "pan=stereo|c0=c1|c1=c0"                      swap L/R
-af "pan=mono|c0=0.5*c0+0.5*c1"                   explicit downmix
-af "channelsplit=channel_layout=stereo:channels=FL"   keep only the left channel   → 1
-map_channel                                      **REMOVED in 8.x**: "Unrecognized option 'map_channel'" — use pan/channelsplit
```
Dual-mono voice recordings (same signal both sides) → `-ac 1` before loudnorm, or `loudnorm=dual_mono=true` when the file must stay mono [doc].

## Joining / replacing audio [measured]
```
ffmpeg -y -i video.mp4 -i music.wav -map 0:v:0 -map 1:a:0 -c:v copy -c:a aac -b:a 192k -shortest out.mp4   # ends with the shorter; measured duration 20.000 (video was the shorter)
```
Without `-shortest` the container runs to the longest stream: measured `video 20.0 / audio 2.0`
in the reverse case (audio shorter than video) — the video keeps playing silent. `-shortest`
needs the streams interleaved; add `-fflags +shortest -max_interleave_delta 100M` if it overruns
[community]. Offsetting audio: `-itsoffset 0.5 -i audio.wav` before that input, or `adelay=500|500`.
Second language track / music stem as extra streams: `07-subtitles-text-overlays.md` §Streams and metadata.

## Fades [measured]
`-af "afade=t=in:d=1,afade=t=out:st=4:d=1" -vf "fade=t=in:d=1,fade=t=out:st=4:d=1"` on a 5 s clip →
5.000 s; `st` is the fade-out START. Compute `st = duration − d` from ffprobe, never guess.

## Verification for audio (always)
`ebur128=peak=true` → I, LRA, true peak; `silencedetect` → gaps; `astats` → `Peak level`,
`RMS level`, DC offset; channels/sample rate via ffprobe. Listen with `offload_transcribe` when
the content matters (repeated phrases, clipped words).
