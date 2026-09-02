# 07 — Subtitles, drawtext, overlays, stream mapping, metadata, timecode

Measured 2026-09-01 on the workstation (8.1.2, libass 0.17.5 with the DirectWrite font provider,
no fontconfig). Every "drawn" claim below was verified by a pixel diff against the same frame
without the filter (02 §Comparing) — exit code 0 does NOT mean text was drawn on this build.

## Subtitle files used
`test.srt` (cue 1 at 0.5–2.5 s "Hola mundo <b>bold</b>", cue 2 at 3–5 s two lines) and a
1920×1080 `test.ass` with a `Default` Arial 64 style and inline `{\b1}` / `{\c&H0000FF&}` tags.

## Burn-in [measured]
```
ffmpeg -y -i in.mp4 -vf "subtitles=test.srt:force_style='FontName=Arial,FontSize=28,PrimaryColour=&H00FFFF&,Outline=2'" -c:v libx264 -preset veryfast -crf 18 -c:a copy out.mp4   # drawn (diff 2.43 at 1.5 s)
ffmpeg -y -i in.mp4 -vf "ass=test.ass:fontsdir=fonts" -c:v libx264 … out.mp4                                                                              # drawn (diff 0.84)
```
- `subtitles=` converts SRT/VTT/etc. to ASS internally and takes `force_style` (ASS style
  KEY=VALUE pairs separated by `,` — `Fontname`, `FontSize`, `PrimaryColour=&HAABBGGRR`, `Outline`,
  `Shadow`, `Alignment`, `MarginV`…); `ass=` renders a native ASS file and ignores `force_style` [doc].
- `fontsdir=` adds a directory of .ttf/.otf (copy brand fonts there; no install needed) [doc + measured].
- `original_size=WxH` when the ASS was authored for another resolution [doc].
- Fonts on Windows come from DirectWrite (system fonts) — no fontconfig needed for libass [measured].
- Picture-based subs (PGS/DVB) use `overlay`, not `subtitles` [doc].

### The `-ss` trap [measured]
Input seeking resets timestamps to 0, and the subtitles filter matches cue times against those:
```
-ss 1.5 -i in.mp4 -frames:v 1 -vf subtitles=test.srt out.png            → NOT drawn (diff 0)
-ss 1.5 -copyts -i in.mp4 … -vf subtitles=test.srt                        → drawn
-i in.mp4 -ss 1.5 … -vf subtitles=test.srt                                → drawn (output seek, decodes from 0)
-ss 1.5 -i in.mp4 … -vf "setpts=PTS+1.5/TB,subtitles=test.srt"            → drawn
```
For a cut WITH burned subs: `-ss 1.5 -copyts -i in.mp4 -t 2 -vf "subtitles=test.srt,setpts=PTS-STARTPTS"`
→ `start_time 0, duration 2.0`; without the `setpts` reset the file starts at 1.5 s and
`duration` reads 0.5 — a player-dependent mess. Same applies to `ass=` and to `enable=between(t,…)`
in drawtext (`t` is seek-relative: `-ss 5 … enable='between(t,1,2)'` drew at file time 6–7 s).

The trac page prescribes exactly this: `ffmpeg -ss 5:00.00 -copyts -i video.avi -ss 5:00.00 -vf subtitles=subtitles.srt out.avi`
(input seek + `-copyts` + the same output `-ss`) [doc-trac]. It also lists `ffmpeg -i subtitle.srt subtitle.ass`
for conversion and `-filter_complex "[0:v][0:s]overlay[v]" -map "[v]" -map 0:a` for picture-based streams.

### Path forms inside `subtitles=` (see 08 for the shell layer) [measured]
Working from PowerShell/cmd: `subtitles='C\:/dir/test.srt'`, `subtitles=C\\:/dir/test.srt`,
`subtitles=/dir/test.srt` (cwd on C:), `subtitles=../test.srt`, `subtitles=filename='C\:\\dir\\test.srt'`.
From Git Bash the `/dir/…` and `C\\:/…` forms are rewritten by MSYS path conversion (08); use a
relative path or a `-filter_script`.

## Soft subtitles (separate track) [measured]
```
ffmpeg -y -i in.mp4 -i test.srt -map 0 -map 1 -c copy -c:s mov_text -metadata:s:s:0 language=spa -disposition:s:0 default soft.mp4
→ stream 2: mov_text, subtitle, language spa, default=1
ffmpeg -y -i in.mp4 -i test.ass -map 0 -map 1 -c copy -metadata:s:s:0 language=spa soft.mkv        → stream 2: ass
ffmpeg -y -i soft.mp4 -map 0:s:0 extracted.srt                                                       → round-trips, tags kept
```
MP4 only carries `mov_text` (styling lost); MKV carries srt/ass/vtt natively. YouTube ignores
embedded tracks — upload the SRT separately.

## drawtext [doc + measured]
Options that matter: `fontfile` (mandatory on this build), `text` | `textfile`, `fontsize`
(default 16), `fontcolor` (default black), `x`/`y` expressions (`w`,`h`,`text_w`/`tw`,
`text_h`/`th`, `line_h`, `n`, `t`), `box=1 boxcolor=black@0.6 boxborderw=16`, `borderw`/`bordercolor`,
`shadowx`/`shadowy`/`shadowcolor`, `line_spacing`, `text_align=TL|MC|…`, `alpha` (expression),
`enable='between(t,1,3)'`, `expansion=normal|none`, `timecode='01\:00\:00\:00':rate=30`,
`start_number`, `reload` (re-read textfile every N frames), `fix_bounds` [doc].

Lower third that works (Git Bash shown; PowerShell/cmd identical string in double quotes):
```
-vf "drawtext=fontfile='C\:/Windows/Fonts/arialbd.ttf':text='Jane Doe':fontsize=56:fontcolor=white:box=1:boxcolor=black@0.6:boxborderw=16:x=80:y=h-200:enable='between(t,1,3)'"
```
(In bash the `\:` needs to reach ffmpeg as one backslash: inside double quotes write `C\\:` or,
simpler, put the whole `-vf` argument in **single** quotes.)

Timecode / frame counter / pts burn-in:
```
drawtext=fontfile=/Windows/Fonts/consola.ttf:text='%{pts\:hms} f%{n}':fontsize=36:fontcolor=white:x=20:y=20     # drawn (diff 0.47)
drawtext=fontfile=/Windows/Fonts/consola.ttf:timecode='01\:00\:00\:00':rate=30:fontsize=36:x=20:y=70
```

### Escaping inside `text=` (what ffmpeg must receive) [measured]
| text value reaching ffmpeg | result |
|---|---|
| `'Hi'`, `Hi`, `"Hi there"` | drawn |
| `a\,b`, `a\;b`, `x\=y`, `Hola\: mundo` (colon escaped) | drawn |
| `a,b` unescaped comma, `Hola: mundo` unescaped colon | **parse error** ("No option name near…") |
| `It's` (apostrophe, arrived unquoted) | drawn |
| `100%`, `100%%`, `100\%` with default `expansion=normal` | **"Stray %" and NOTHING drawn, exit 0** |
| `100%:expansion=none`, `100%%:expansion=none` | drawn (literal `%`, `%%`) |
| `textfile=… :expansion=none` with `%` and `"` in the file | drawn |
Rule: any `%` in text → `expansion=none` (you lose `%{pts}`/`%{n}` in that instance; use two
drawtext instances if you need both). The docs say `%%` is a literal percent in normal
expansion; on 8.1.2 Windows it is not — measured, so tagged as a build quirk to re-test on 9.0.1.

### Fonts [measured]
- `fontfile=` with a real path always works (Arial, Arial Bold, Consolas tested).
- `font=Arial` / no font option → `Fontconfig error: Cannot load default config file` then **segfault** (exit −1073741819 / 139). Never rely on fontconfig on the Gyan build.
- Brand fonts: point `fontfile=` at the .ttf directly (no install), e.g. `<brand fonts dir>\LeagueGothic-Regular.ttf` on the editing rig.

## Overlays with alpha [measured]
PNG with alpha (logo/watermark), top-right with 40 px margin, last 3 s only:
```
ffmpeg -y -i in.mp4 -i logo.png -filter_complex "[1:v]format=rgba[l];[0:v][l]overlay=W-w-40:40:format=auto" -c:v libx264 -crf 18 -c:a copy out.mp4
```
(`enable='gte(t,17)'` on the overlay for timing; `[1:v]scale=200:-1` to resize the logo.)
ProRes 4444 with alpha (motion graphics from Resolve/AE): generate + composite:
```
ffmpeg -y -f lavfi -i "color=c=blue@0.4:s=600x300:r=30,format=yuva444p10le" -t 3 -c:v prores_ks -profile:v 4444 -pix_fmt yuva444p10le alpha4444.mov   → prores, profile 4444, yuva444p12le
ffmpeg -y -i in.mp4 -i alpha4444.mov -filter_complex "[0:v][1:v]overlay=100:100:shortest=1:format=auto" -c:v libx264 -crf 18 -c:a copy out.mp4
pixel check, 600×300 region at (100,100): YAVG 128.6 (source) → 93.3 (overlaid) = alpha applied
```
`format=auto` picks the working pixel format; `shortest=1` ends when the overlay ends (else the
main continues, fine for a bug-free layout). Alpha PNG sequences → ProRes 4444: 04.
GPU overlay: `overlay_cuda` refuses `rgba` overlays ("Unsupported overlay input format: rgba") —
convert the overlay to `yuva420p` or `nv12` and `hwupload_cuda` it, or overlay on the CPU and
`hwupload` afterwards [measured].

## Streams and metadata [measured]
```
ffmpeg -y -i in.mp4 -i music.wav -map 0:v -map 0:a -map 1:a -c:v copy -c:a aac \
  -metadata title="Ref clip" -metadata:s:a:0 language=spa -metadata:s:a:0 title="Voice" -metadata:s:a:1 language=eng -metadata:s:a:1 title="Music" \
  -disposition:a:0 default -disposition:a:1 0 -movflags +faststart multi.mp4
→ 0 video und default | 1 audio spa "Voice" default | 2 audio eng "Music" non-default | format title "Ref clip"; moov@36 (faststart)
ffmpeg -y -i multi.mp4 -map 0 -map_metadata -1 -map_chapters -1 -c copy stripped.mp4     # only the muxer's own tags remain (major_brand/encoder)
```
Mapping syntax [doc]: `-map 0:v` all video of input 0, `-map 0:a:1` second audio, `-map -0:s`
exclude, `-map 0:a?` optional. Streams not mapped are dropped; without any `-map`, ffmpeg picks
one "best" stream per type [doc].
Chapters via ffmetadata (`;FFMETADATA1` header, `[CHAPTER] TIMEBASE=1/1000 START END title`):
`-i chapters.txt -map_metadata 1` → `ffprobe -show_chapters` lists them [measured].

## Timecode [measured]
```
ffmpeg -y -i in.mp4 -c copy -timecode 01:02:03:04 tc.mov      → tmcd data track, stream_tags timecode=01:02:03:04 on video and data streams
same with .mp4                                                → works too (data track)
-timecode "01:00:00;00" at 30000/1001                         → drop-frame 01:00:00;00 written and read back
```
Read: `ffprobe -show_entries stream_tags=timecode`. drawtext can burn it (`timecode=` option) —
start value from the tag, `rate=` = the video rate rounded [doc].
