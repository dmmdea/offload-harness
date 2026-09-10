# Re-test the measured 8.1.2 traps on THIS box's ffmpeg. Zero GPU, ~1 s per case.
$ff = (Get-Command ffmpeg).Source
$fp = (Get-Command ffprobe).Source
$w  = Join-Path $env:TEMP "ff9traps"
New-Item -ItemType Directory -Force -Path $w | Out-Null
Set-Location $w
$NL = [Environment]::NewLine
function Flat($path, $max) {
  if (-not (Test-Path $path)) { return "" }
  $t = Get-Content $path -Raw
  if ($null -eq $t) { return "" }
  $t = ($t -replace "`r"," " -replace "`n"," ").Trim()
  if ($t.Length -gt $max) { $t = $t.Substring(0, $max) }
  return $t
}
"HOST $env:COMPUTERNAME"
$ver = & $ff -hide_banner -version
$ver[0]
$cfg = ($ver | Select-String "^configuration:" | Out-String).Trim()
"CONFIG_LEN " + $cfg.Length
$libass = ($ver | Select-String "libavfilter" | Out-String).Trim()
"LIBS " + $libass

# fixtures
& $ff -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=1280x720:rate=30" -f lavfi -i "sine=frequency=440:sample_rate=48000" -t 6 -c:v libx264 -preset veryfast -crf 20 -g 60 -keyint_min 60 -sc_threshold 0 -pix_fmt yuv420p -c:a aac src.mp4 2>$null
$srt = "1" + $NL + "00:00:00,500 --> 00:00:02,500" + $NL + "Hola mundo" + $NL + $NL
Set-Content -Path test.srt -Value $srt -Encoding ASCII
& $ff -hide_banner -loglevel error -y -ss 0.5 -i src.mp4 -frames:v 1 plain05.png 2>$null
& $ff -hide_banner -loglevel error -y -ss 1.5 -i src.mp4 -frames:v 1 plain15.png 2>$null
$fx = (Get-Item src.mp4).Length
"FIXTURES src=$fx plain05=$(Test-Path plain05.png) plain15=$(Test-Path plain15.png)"

function Get-Diff($a, $b) {
  if (-not (Test-Path $a)) { return "NOFILE" }
  & $ff -hide_banner -nostats -i $a -i $b -lavfi "[0:v][1:v]blend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-" -f null - 2>$null 1>diff.txt
  $raw = Get-Content diff.txt -Raw
  if ($raw -match "YAVG=([0-9.]+)") { return $Matches[1] }
  return "n/a"
}
function Test-Case($name, $a, $out, $ref) {
  Remove-Item $out -ErrorAction SilentlyContinue
  & $ff -hide_banner -loglevel error -y @a $out 2>err.txt
  $rc = $LASTEXITCODE
  $err = Flat "err.txt" 110
  $d = Get-Diff $out $ref
  "CASE $name rc=$rc drawn=$d err=$err"
}
$AR = "'C\:/Windows/Fonts/arial.ttf'"

Test-Case "pct_normal_single"    @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$AR`:text='100%':fontsize=80:fontcolor=white:x=100:y=100") t1.png plain05.png
Test-Case "pct_normal_double"    @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$AR`:text='100%%':fontsize=80:fontcolor=white:x=100:y=100") t2.png plain05.png
Test-Case "pct_expansion_none"   @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$AR`:text='100%':expansion=none:fontsize=80:fontcolor=white:x=100:y=100") t3.png plain05.png
Test-Case "font_unquoted"        @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") t4.png plain05.png
Test-Case "font_quoted"          @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$AR`:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") t5.png plain05.png
Test-Case "font_driveless"       @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") t6.png plain05.png
Test-Case "font_fontconfig"      @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=font=Arial:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") t7.png plain05.png
Test-Case "pts_expansion"        @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$AR`:text='%{pts\:hms}':fontsize=80:fontcolor=white:x=100:y=100") t8.png plain05.png
Test-Case "subs_input_ss"        @('-ss','1.5','-i','src.mp4','-frames:v','1','-vf','subtitles=test.srt') s1.png plain15.png
Test-Case "subs_input_ss_copyts" @('-ss','1.5','-copyts','-i','src.mp4','-frames:v','1','-vf','subtitles=test.srt') s2.png plain15.png
Test-Case "subs_output_ss"       @('-i','src.mp4','-ss','1.5','-frames:v','1','-vf','subtitles=test.srt') s3.png plain15.png

"--- feature/version deltas"
& $ff -hide_banner -loglevel error -y -i src.mp4 -vn -map_channel 0.1.0 mc.wav 2>mc.txt
$mc = Flat "mc.txt" 90
"FEAT map_channel rc=$LASTEXITCODE msg=$mc"
& $ff -hide_banner -loglevel error -y -framerate 1 -pattern_type glob -i "*.png" -frames:v 1 -f null - 2>gl.txt
$gl = Flat "gl.txt" 110
"FEAT glob rc=$LASTEXITCODE msg=$gl"
$filters = & $ff -hide_banner -filters
$encoders = & $ff -hide_banner -encoders
$fnpp = ($filters | Select-String " scale_npp ").Count
$fcuda = ($filters | Select-String " scale_cuda ").Count
$fovl = ($filters | Select-String " overlay_cuda ").Count
$fpl = ($filters | Select-String " libplacebo ").Count
$fwh = ($filters | Select-String " whisper ").Count
"FEAT filters scale_npp=$fnpp scale_cuda=$fcuda overlay_cuda=$fovl libplacebo=$fpl whisper=$fwh"
$eav1 = ($encoders | Select-String " av1_nvenc ").Count
$ehev = ($encoders | Select-String " hevc_nvenc ").Count
$esvt = ($encoders | Select-String " libsvtav1 ").Count
$evv = ($encoders | Select-String " libvvenc ").Count
$ewa = ($encoders | Select-String " libwebp_anim ").Count
"FEAT encoders av1_nvenc=$eav1 hevc_nvenc=$ehev libsvtav1=$esvt libvvenc=$evv libwebp_anim=$ewa"
& $ff -hide_banner -loglevel error -y -i src.mp4 -t 2 -vf "fps=10,scale=320:-1" -c:v libwebp_anim -loop 0 anim.webp 2>$null
& $fp -v error -show_entries stream=codec_name,width,height,nb_frames -of csv=p=0 anim.webp 2>wp_err.txt 1>wp_out.txt
$wpo = Flat "wp_out.txt" 60
$wpe = Flat "wp_err.txt" 60
"FEAT webp_probe rc=$LASTEXITCODE out=$wpo err=$wpe"

"--- copy-cut keyframe trap (-ss 1 -t 2 -c copy on a 2 s GOP)"
& $ff -hide_banner -loglevel error -y -ss 1 -i src.mp4 -t 2 -c copy cut.mp4 2>$null
$cutinfo = (& $fp -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -show_entries format=start_time,duration -of default=nw=1 cut.mp4) -join ' '
"CUT $cutinfo"
$pk = (& $fp -v error -select_streams v:0 -show_entries packet=pts_time,flags -of csv=p=0 cut.mp4)
$pk3 = ($pk | Select-Object -First 3) -join ' '
"CUT first_packets $pk3"

"--- exit codes (PowerShell 5.1 sees raw AVERROR)"
& $ff -hide_banner -loglevel error -i nope.mp4 -f null - 2>$null
"RC missing_input=$LASTEXITCODE"
& $ff -hide_banner -loglevel error -y -i src.mp4 -t 1 -vf "nosuchfilter=1" -f null - 2>$null
"RC bad_filter=$LASTEXITCODE"
& $ff -hide_banner -loglevel error -n -i src.mp4 -t 1 -c copy src.mp4 2>$null
"RC n_existing_output=$LASTEXITCODE"
& $ff -hide_banner -loglevel error -y -i src.mp4 -t 1 -f null - 2>$null
"RC ok=$LASTEXITCODE"
"DONE"
