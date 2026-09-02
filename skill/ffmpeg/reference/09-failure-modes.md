# 09 — Failure modes, traps, and the fix for each

Ordered by how likely they are to bite an autonomous pipeline. Tags: [measured] on our boxes
2026-09-01 · [doc] · [community] · [inferred].

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | drawtext renders nothing, exit 0, stderr "Stray % near …" [measured] | any `%` in `text=`/`textfile=` under `expansion=normal` on 8.1.2 Windows (`%%` too) | `expansion=none`, or split into two drawtext instances; pixel-diff the frame |
| 2 | "No option name near '/Windows/Fonts/…'" [measured] | `fontfile=C\:/…` unquoted | `fontfile='C\:/…'` (quoted inside the graph) or `C\\:/…` or drive-less `/Windows/…`; see 08 |
| 3 | ffmpeg crashes 0xC0000005 / exit 139 after "Fontconfig error: Cannot load default config file" [measured] | `font=`/no fontfile, or an unparsable fontfile path falling back to fontconfig | always `fontfile=` with a valid path; libass (`subtitles`/`ass`) is unaffected (DirectWrite) |
| 4 | Subtitles/ASS not visible, or at the wrong time, after `-ss` [measured] | input seek resets pts to 0; cue times are absolute | `-copyts` (+ `setpts=PTS-STARTPTS` after the filter), or output-side `-ss`, or `setpts=PTS+<ss>/TB` before the filter |
| 5 | `-c copy` cut has 152 frames for `-t 4`, negative pts, or starts on the wrong second [measured] | keyframe-aligned copy; `-ss` before `-i` keeps the GOP, after `-i` leaves a hole | re-encode for exact cuts; copy only on keyframes; `-avoid_negative_ts make_zero` if the early material is acceptable |
| 6 | `-to` gives the wrong length [measured] | after an input `-ss`, `-to` counts from 0 of the output | use `-t duration`, or `-copyts` with `-to` |
| 7 | Concat demuxer output "plays" but ffprobe reports the first file's size, warning "Non-monotonic DTS" [measured] | inputs differ (size/codec/timebase) | ffprobe every input and compare; else concat FILTER with scale/setsar/fps/aformat |
| 8 | Git Bash: "No such filter: 'C:Program FilesGit…'" or "Unable to parse original_size" [measured] | MSYS path conversion rewrote a `/…` or `X\\:` argument | relative path, `MSYS_NO_PATHCONV=1`, `-filter_script`, or call from Python argv |
| 9 | PowerShell: `$LASTEXITCODE` −1 or 0 on a run that failed/succeeded [measured] | `\| Select-Object -First N` stopped the pipeline early | `2>$null` / `2>&1 \| Out-String` / stderr to file |
| 10 | PowerShell: text missing words, `$5` gone, `$t:fontsize` empty [measured] | double-quoted PS string expanded `$…` / scoped variable | single-quoted PS strings for filtergraphs |
| 11 | PS 5.1 drops `"` from the text [measured] | 5.1 native argument passing | use `'…'` inside the graph, or pwsh 7 |
| 12 | `-n` "already exists" and exit **0** [measured] | by design in 8.1.2 | check output mtime; use `-y` with your own existence check |
| 13 | `Unrecognized option 'map_channel'` [measured] | removed in 8.x | `pan=`, `channelsplit`, `-ac` |
| 14 | `pattern_type glob` "not supported by this libavformat build" [measured] | Windows build lacks glob | `%03d` sequences, `-start_number`, or concat list |
| 15 | `scale_npp` "No option name near…" [measured] | Gyan build has no NPP (the error is a parser error, not "no such filter") | `scale_cuda` (nearest/bilinear/bicubic/lanczos `interp_algo`), `libplacebo` via Vulkan, or CPU `scale` |
| 16 | `overlay_cuda`: "Unsupported overlay input format: rgba" [measured] | CUDA overlay wants yuv420p/nv12/yuva420p frames | `format=yuva420p,hwupload_cuda` on the overlay, or composite on CPU |
| 17 | `av1_nvenc`: "No capable devices found" [measured Linux node] | Ampere (RTX 30xx) has no AV1 encoder; encoder is listed anyway | h264/hevc_nvenc, or CPU libsvtav1; probe with a 1-frame test per host |
| 18 | ffprobe on animated WebP: "image data not found", 0×0 [measured] | no animated-webp demuxer | verify with PIL (`n_frames`, `is_animated`) |
| 19 | `libx264 width not divisible by 2` [measured] | odd scale result (`scale=1281:-1`) | `-2` in scale, or `trunc(iw/2)*2` |
| 20 | Loudness lands 0.4 LU off target [measured] | two-pass linear + AAC/MP3 recoding | re-measure, re-run pass 2 with the new offset; ±0.5 LU acceptable |
| 21 | `silenceremove` left 0.7 s gaps [measured] | kept = `stop_duration + stop_silence` | lower `stop_duration` (0.3) and `stop_silence` (0–0.2); or the select/aselect cut list (06) |
| 22 | Duration longer than expected after joining audio [measured] | no `-shortest`; muxer runs to the longest stream | `-shortest` (+ `-fflags +shortest`) or `-t` |
| 23 | `-t` cut ends up 4.0 s on a retimed output [measured] | `-t` before `-i` limits the INPUT read | put `-t` after `-i` for output duration |
| 24 | `showinfo`/`mpdecimate` counts more unique frames than expected [measured 125 of 60] | encoding noise defeats the default thresholds | tune `mpdecimate=hi:lo:frac`, or `freezedetect=n=0.001` for real holds |
| 25 | libvmaf ≈ 60 on a 15 Mbps encode [measured] | synthetic testsrc2 content | use VMAF only relatively, on real footage; inputs order distorted, reference |
| 26 | GPU encode slower than the table [measured context] | NVENC on GPU 0 shared with llama-swap/ComfyUI models | `-gpu 1` (5070 Ti idle) or wait; the harness reclaims seats on media jobs |
| 27 | "moov atom not found", exit −1094995529/183 [measured] | interrupted encode / truncated download | ffprobe every cached segment before reuse; re-render; for MP4 encodes use `-movflags +faststart` only at the end (it rewrites) |
| 28 | ffmpeg ran, output present, but the wrong ffmpeg [measured path facts] | workstation has 8.1.2 on PATH and 9.0.1 in `D:\Dev\tools` (harness lane) | print `ffmpeg -version` in logs; pin the path in scripts |
| 29 | laptop node/editing rig unreachable over Tailscale [measured 2026-09-01] | hosts off (last seen hours ago) | `tailscale status`; do not conclude a build fact from an offline box — dump when up |
| 30 | Delegated doc digests came back about "Go versions" or timed out [measured 2026-09-01] | 27B seat stuck in llama-swap `starting` (429 on every call); Linux node 4B answered off-goal to generic `offload_research` contracts | `agent_delegate` with the file named in the goal + page-only acceptance tokens works on the Linux node; fetch trac with `curl -A "Go-http-client/1.1"` (Anubis only challenges browser UAs); read ffmpeg.org sections directly |
| 31 | 2-pass x264 pass 1 "fails" writing output [doc + measured] | needs `-f null NUL` (Windows) or `-f null -`; log file name via `-passlogfile` | as measured in 05 |
| 32 | Timestamps jump / A-V drift after concat demuxer of cut clips [measured 8.08 s for 4+4.06] | each file's own priming/hole carried over | re-encode cuts first (exact durations), then concat |
| 33 | Rendered text at wrong y on a 9:16 output | `h`/`w` refer to the frame AFTER the previous filter; drawtext placed before `scale` measures the source | put drawtext last in the chain, after scale/crop/pad [inferred from expression semantics, doc] |

## Diagnostic ladder
1. `ffmpeg -version` (which build?) and the exact argv the process received (`argv.py` trick in 08).
2. Re-run with `-v warning` (not `-loglevel error`): DTS, keyframe, "Stray %", fontconfig lines only show there.
3. Reproduce on the lavfi test clip from 02 — if it passes there, the input file is the problem: ffprobe it fully.
4. Verify the output in its medium (02 gate). If the file is fine and the exit code is nonzero, read the code as a low byte / negative AVERROR (08), don't trust either as a category.
5. Record the measured fact here with a tag and date so it is never re-derived.
