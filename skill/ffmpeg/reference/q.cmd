@echo off
setlocal EnableExtensions
set "PATH=%LOCALAPPDATA%\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.2-full_build\bin;%PATH%"
cd /d "%~dp0"
set "PY=C:\Program Files\Python314\python.exe"
echo --- V1 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q1_cmd.png 2>q_err.txt
echo V1 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V2 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile='C\:/Windows/Fonts/arial.ttf':text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile='C\:/Windows/Fonts/arial.ttf':text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q2_cmd.png 2>q_err.txt
echo V2 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V3 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q3_cmd.png 2>q_err.txt
echo V3 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V4 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q4_cmd.png 2>q_err.txt
echo V4 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V5 cmd argv:
"%PY%" argv.py -vf "drawtext=font=Arial:text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=font=Arial:text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q5_cmd.png 2>q_err.txt
echo V5 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V6 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile='C\:\Windows\Fonts\arial.ttf':text='Hi':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile='C\:\Windows\Fonts\arial.ttf':text='Hi':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q6_cmd.png 2>q_err.txt
echo V6 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V7 cmd argv:
"%PY%" argv.py -filter_script:v vf.txt
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -filter_script:v vf.txt -frames:v 1 q7_cmd.png 2>q_err.txt
echo V7 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V8 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hello\, world\: 100%% done':expansion=none:fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hello\, world\: 100%% done':expansion=none:fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q8_cmd.png 2>q_err.txt
echo V8 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V9 cmd argv:
"%PY%" argv.py -ss 1.5 -vf "subtitles='C\:/<scratch>/test.srt'"
ffmpeg -hide_banner -loglevel error -y  -i ..\src1080.mp4 -ss 1.5 -vf "subtitles='C\:/<scratch>/test.srt'" -frames:v 1 q9_cmd.png 2>q_err.txt
echo V9 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V10 cmd argv:
"%PY%" argv.py -ss 1.5 -vf "subtitles=C\:/<scratch>/test.srt"
ffmpeg -hide_banner -loglevel error -y  -i ..\src1080.mp4 -ss 1.5 -vf "subtitles=C\:/<scratch>/test.srt" -frames:v 1 q10_cmd.png 2>q_err.txt
echo V10 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V11 cmd argv:
"%PY%" argv.py -ss 1.5 -vf "subtitles=/<scratch>/test.srt"
ffmpeg -hide_banner -loglevel error -y  -i ..\src1080.mp4 -ss 1.5 -vf "subtitles=/<scratch>/test.srt" -frames:v 1 q11_cmd.png 2>q_err.txt
echo V11 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V12 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text=\"Hi there\":fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text=\"Hi there\":fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q12_cmd.png 2>q_err.txt
echo V12 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V13 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:textfile=text.txt:expansion=none:fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:textfile=text.txt:expansion=none:fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q13_cmd.png 2>q_err.txt
echo V13 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V14 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='%%{pts\:hms}':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='%%{pts\:hms}':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q14_cmd.png 2>q_err.txt
echo V14 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V15 cmd argv:
"%PY%" argv.py -filter_complex "[0:v]scale=1920:-2,drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100[v]" -map "[v]"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -filter_complex "[0:v]scale=1920:-2,drawtext=fontfile=/Windows/Fonts/arial.ttf:text='Hi':fontsize=80:fontcolor=white:x=100:y=100[v]" -map "[v]" -frames:v 1 q15_cmd.png 2>q_err.txt
echo V15 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V16 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='50%%':expansion=none:fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='50%%':expansion=none:fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q16_cmd.png 2>q_err.txt
echo V16 cmd rc=%ERRORLEVEL%
type q_err.txt
echo --- V17 cmd argv:
"%PY%" argv.py -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='100%%':fontsize=80:fontcolor=white:x=100:y=100"
ffmpeg -hide_banner -loglevel error -y -ss 0.5 -i ..\src1080.mp4 -vf "drawtext=fontfile=/Windows/Fonts/arial.ttf:text='100%%':fontsize=80:fontcolor=white:x=100:y=100" -frames:v 1 q17_cmd.png 2>q_err.txt
echo V17 cmd rc=%ERRORLEVEL%
type q_err.txt
