# 04 — Scaling, aspect, images, GIF/WebP, intermediates, delivery recipes

Measured 2026-09-01 on the workstation (8.1.2) unless tagged otherwise.

## Scale rules [doc + measured]
- `scale=W:-2` keeps aspect and forces an even height (`-1` may produce odd sizes; libx264 refuses `1281x721`: "width not divisible by 2" — and that failure returned exit **0** through a `| head` pipe, another reason to verify the file).
- `flags=` picks the scaler: measured SSIM vs lanczos on a 4K→1080p downscale: bicubic 0.99908, spline 0.99914, bilinear 0.99622, neighbor 0.98720. Use `lanczos` for downscales, `bicubic`/`spline` are equivalent in practice, never `neighbor` for video.
- `force_original_aspect_ratio=decrease` (fit inside) / `increase` (cover, then crop) [doc].
- Always end with `setsar=1` after crop/pad so the DAR is what the pixels say (measured outputs show `1:1,16:9` / `9:16`).
- yuv420p targets need even dimensions; when in doubt add `scale=trunc(iw/2)*2:trunc(ih/2)*2`.

## YouTube 16:9 (1920×1080 or 3840×2160), any input → letterbox/pillarbox [measured]
```
-vf "scale=1920:1080:force_original_aspect_ratio=decrease:flags=lanczos,pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1"
→ 1920,1080,1:1,16:9  (from a 1440×1080 source)
```

## Shorts / Reels / TikTok 9:16 (1080×1920) [measured]
Center-crop of a 16:9 source (loses the sides):
```
-vf "scale=-2:1920:flags=lanczos,crop=1080:1920:(iw-1080)/2:0,setsar=1"   → 1080,1920,9:16
```
Whole 16:9 frame on a blurred copy of itself (the "podcast clip" look):
```
-filter_complex "[0:v]split[a][b];[a]scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920,gblur=sigma=30[bg];[b]scale=1080:-2[fg];[bg][fg]overlay=(W-w)/2:(H-h)/2,setsar=1"   → 1080,1920
```
Add the caption/lower-third from 07 after the overlay in the same chain.

## Thumbnails [measured]
```
ffmpeg -y -ss 7.5 -i src.mp4 -frames:v 1 -vf "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:-1:-1:color=black" -q:v 2 thumb.jpg   # 1280×720; -1:-1 centers the pad
ffmpeg -y -ss 2 -i src4k.mp4 -frames:v 1 -vf scale=1280:720 -q:v 80 thumb.webp     # 25.5 KB (libwebp still, -q:v 0–100)
ffmpeg -y -ss 2 -i src4k.mp4 -frames:v 1 -vf scale=1280:720 thumb.png              # 159 KB
ffmpeg -y -i src.mp4 -vf "thumbnail=100" -frames:v 1 thumb_auto.jpg                  # picks the most representative frame of each 100
ffmpeg -y -skip_frame nokey -i src.mp4 -vsync vfr -frame_pts 1 -vf scale=320:-2 -frames:v 3 kf_%d.jpg   # keyframes only, file name = pts (kf_0, kf_60, kf_120)
```
JPEG quality: `-q:v 2` ≈ best (scale 2–31). YouTube thumbnail limit is 2 MB, 1280×720 recommended [community].

## Frame-rate conversion [measured]
`-vf fps=24` on 2 s of 30 fps → 48 frames (drops); `-r 60` → 120 frames (duplicates). For
motion-interpolated conversion use `minterpolate=fps=60:mi_mode=mci` (slow). Verify with
`-count_frames` and `r_frame_rate`.

## Video → image sequence → video [measured]
```
ffmpeg -y -i src.mp4 -t 5 -vf fps=1 -q:v 2 frames/f_%03d.jpg                # 5 files
ffmpeg -y -framerate 1 -i frames/f_%03d.jpg -c:v libx264 -r 30 -pix_fmt yuv420p -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" from_frames.mp4   # 150 frames, 30/1, 5.000 s
```
- Input rate for images is `-framerate` (before `-i`); `-r` on the output sets the container rate [doc].
- `-pattern_type glob` is **not supported** by the Windows build ("globbing is not supported by this libavformat build") — use `%03d` patterns (with `-start_number N` if needed) or a concat list [measured].
- Alpha sequences: `-i seq/f_%04d.png -c:v prores_ks -profile:v 4444 -pix_fmt yuva444p10le` → ffprobe reports `yuva444p12le` (prores_ks writes 12-bit alpha), 30 frames [measured].

## GIF [measured, 3 s, 480 px, 15 fps]
```
ffmpeg -y -i src.mp4 -t 3 -filter_complex "[0:v]fps=15,scale=480:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" -loop 0 out.gif
→ 436 KB, 45 frames, 480×270
ffmpeg -y -i src.mp4 -t 3 -vf "fps=15,scale=480:-1" naive.gif   → 322 KB (smaller here because testsrc2 is flat colour; on real footage the palette path is smaller AND far better)
```
`stats_mode=diff` favours moving areas; `dither=bayer:bayer_scale=5` = fine ordered dither,
`dither=sierra2_4a` = smoother/larger; `-loop 0` = infinite [doc + community]. Verify with
`ffprobe -count_frames` (works for GIF).

## Animated WebP [measured]
```
ffmpeg -y -i src.mp4 -t 3 -vf "fps=15,scale=480:-1" -c:v libwebp_anim -lossless 0 -q:v 75 -loop 0 out.webp   → 216 KB
```
**ffprobe cannot read the result** (`image data not found`, reports 0×0). Verify with Python:
`from PIL import Image; im=Image.open('out.webp'); im.size, im.n_frames, im.is_animated` → `(480,270) 45 True`. Or `-c:v libwebp` for a still.

## Intermediates / mezzanine [measured]
```
-c:v prores_ks -profile:v 3 -vendor apl0 -pix_fmt yuv422p10le -c:a pcm_s16le out.mov   # ProRes 422 HQ (profile 0 proxy,1 LT,2 std,3 HQ,4 4444,5 4444XQ)
-c:v prores_ks -profile:v 4444 -pix_fmt yuva444p10le out.mov                            # with alpha (see 07)
-c:v dnxhd -profile:v dnxhr_hq -pix_fmt yuv422p -c:a pcm_s16le out.mov                 # DNxHR HQ
```
Resolve/Premiere ingest both. 10-bit HEVC delivery: `-c:v hevc_nvenc -pix_fmt p010le -profile:v main10` → `hevc, Main 10, yuv420p10le` [measured].

## Delivery recipes (assembled from measured pieces; numbers in 05/06)
YouTube 1080p/4K upload (quality-first, GPU):
```
ffmpeg -y -i in.mp4 -c:v hevc_nvenc -preset p7 -tune hq -rc vbr -cq 24 -b:v 0 -multipass fullres -rc-lookahead 32 -spatial-aq 1 -temporal-aq 1 -b_ref_mode middle -pix_fmt p010le -profile:v main10 -g 60 -c:a aac -b:a 192k -ar 48000 -movflags +faststart out.mp4
```
(YouTube re-encodes everything; upload high bitrate H.264/HEVC. Community guidance: 1080p ≥ 8–12 Mbps, 4K ≥ 35–45 Mbps, AAC 48 kHz, keyframe interval 2 s, `+faststart`. YouTube's own "recommended upload encoding settings" page is the authority; not re-fetched here [community].)
YouTube CPU equivalent: `-c:v libx264 -preset slow -crf 18 -pix_fmt yuv420p -g 60 -c:a aac -b:a 192k -movflags +faststart`.
Shorts: the 9:16 chain above + `-c:v h264_nvenc -preset p6 -cq 23 -b:v 0` or `libx264 -crf 20`, ≤ 60 s (`-t 59.9`), audio −14 LUFS (06).
Podcast audio: `-vn -ar 48000 -c:a libmp3lame -b:a 128k` (mono voice: `-ac 1 -b:a 96k`) after two-pass loudnorm to −16 LUFS (06).
Verify each with the 02 gate; a delivery is not done until the numbers are printed.
