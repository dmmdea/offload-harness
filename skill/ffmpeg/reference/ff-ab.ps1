param([Parameter(Mandatory=$true)][string]$Ff)
# A/B the version-sensitive cases with ONE ffmpeg binary. Run from a cwd on C:.
$ffp = (Resolve-Path $Ff).Path
$fpp = Join-Path (Split-Path $ffp) "ffprobe.exe"
$w = Join-Path $env:TEMP ("ffab_" + [IO.Path]::GetFileNameWithoutExtension((Split-Path (Split-Path $ffp) -Parent)))
New-Item -ItemType Directory -Force -Path $w | Out-Null
Set-Location $w
function Flat($p, $m) {
  if (-not (Test-Path $p)) { return "" }
  $t = Get-Content $p -Raw
  if ($null -eq $t) { return "" }
  $t = ($t -replace "`r"," " -replace "`n"," ").Trim()
  if ($t.Length -gt $m) { $t = $t.Substring(0, $m) }
  return $t
}
$v = (& $ffp -hide_banner -version)[0]
"=== $v"
"CWD $(Get-Location)  FONT_EXISTS $(Test-Path 'C:\Windows\Fonts\arial.ttf')"
& $ffp -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=1280x720:rate=30" -t 4 -c:v libx264 -preset veryfast -crf 20 -g 60 -keyint_min 60 -sc_threshold 0 -pix_fmt yuv420p src.mp4 2>$null
& $ffp -hide_banner -loglevel error -y -ss 0.5 -i src.mp4 -frames:v 1 plain05.png 2>$null
function Get-Diff($a, $b) {
  if (-not (Test-Path $a)) { return "NOFILE" }
  & $ffp -hide_banner -nostats -i $a -i $b -lavfi "[0:v][1:v]blend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-" -f null - 2>$null 1>d.txt
  $raw = Get-Content d.txt -Raw
  if ($raw -match "YAVG=([0-9.]+)") { return $Matches[1] }
  return "n/a"
}
function T($name, $a, $out) {
  Remove-Item $out -ErrorAction SilentlyContinue
  & $ffp -hide_banner -loglevel error -y @a $out 2>e.txt
  $rc = $LASTEXITCODE
  $err = Flat "e.txt" 95
  $d = Get-Diff $out plain05.png
  "  $name rc=$rc drawn=$d err=$err"
}
$Q = "'C\:/Windows/Fonts/arial.ttf'"
T "pct_normal"      @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$Q`:text='100%':fontsize=80:fontcolor=white:x=100:y=100") a1.png
T "font_quoted"     @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=$Q`:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") a2.png
T "font_driveless"  @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100") a3.png
T "font_plain_colon" @('-ss','0.5','-i','src.mp4','-frames:v','1','-vf',"drawtext=fontfile='C:/Windows/Fonts/arial.ttf':text='Hi':fontsize=80:fontcolor=white:x=100:y=100") a4.png
& $ffp -hide_banner -loglevel error -y -i src.mp4 -t 2 -vf "fps=10,scale=320:-1" -c:v libwebp_anim -loop 0 anim.webp 2>$null
& $fpp -v error -show_entries stream=codec_name,width,height -of csv=p=0 anim.webp 2>we.txt 1>wo.txt
"  webp_probe rc=$LASTEXITCODE out=$(Flat 'wo.txt' 50) err=$(Flat 'we.txt' 50)"
