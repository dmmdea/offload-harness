export PATH="$LOCALAPPDATA/Microsoft/WinGet/Packages/Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe/ffmpeg-8.1.2-full_build/bin:$PATH"
PY="/c/Program Files/Python314/python"
now(){ "$PY" -c "import time; print(int(time.time()*1000))"; }
vmaf(){ ffmpeg -hide_banner -nostats -threads 16 -i "$2" -i "$1" -lavfi "[0:v][1:v]libvmaf=n_threads=16:n_subsample=2:feature=name=psnr" -f null - 2>&1 | grep -oE "VMAF score: [0-9.]+" ; }
echo "## reference: DJI HEVC 10-bit 3840x3840 59.94 -> 1920x1920 30fps 8-bit near-lossless (crf 8), 8 s"
ffmpeg -hide_banner -loglevel error -y -i real_src.mp4 -t 8 -vf "fps=30,scale=1920:-2:flags=lanczos,format=yuv420p" -c:v libx264 -preset medium -crf 8 -an real_ref.mp4
ffprobe -v error -count_frames -select_streams v -show_entries stream=nb_read_frames,width,height,pix_fmt -show_entries format=bit_rate -of csv=p=0 real_ref.mp4
echo "## sweep (real footage, 240 frames 1920x1920 30fps): encoder cq/crf kbps fps VMAF"
run(){ name=$1; shift; s=$(now); ffmpeg -hide_banner -loglevel error -y -i real_ref.mp4 "$@" -an r_$name.mp4; e=$(now); fps=$((240000/(e-s))); kb=$(ffprobe -v error -show_entries format=bit_rate -of csv=p=0 r_$name.mp4); echo "$name kbps=$((kb/1000)) enc_fps=$fps $(vmaf real_ref.mp4 r_$name.mp4)"; }
for cq in 19 23 27 31; do run h264nv_cq$cq -c:v h264_nvenc -preset p6 -tune hq -rc vbr -cq $cq -b:v 0; done
for cq in 22 26 30 34; do run hevcnv_cq$cq -c:v hevc_nvenc -preset p6 -tune hq -rc vbr -cq $cq -b:v 0; done
for cq in 26 30 34 38; do run av1nv_cq$cq -c:v av1_nvenc -preset p6 -tune hq -rc vbr -cq $cq -b:v 0; done
run hevcnv_p7_cq26 -c:v hevc_nvenc -preset p7 -tune hq -rc vbr -cq 26 -b:v 0 -multipass fullres -rc-lookahead 32 -spatial-aq 1 -temporal-aq 1 -b_ref_mode middle
for crf in 18 23 28; do run x264_crf$crf -c:v libx264 -preset medium -crf $crf; done
for crf in 22 28; do run x265_crf$crf -c:v libx265 -preset medium -crf $crf; done
for crf in 30 38; do run svt_crf$crf -c:v libsvtav1 -preset 6 -crf $crf; done
echo "## native 4K-ish check: 3840x3840 10-bit HEVC source -> hevc_nvenc p4 cq26 p010, throughput + VMAF (4 s)"
ffmpeg -hide_banner -loglevel error -y -i real_src.mp4 -t 4 -c:v libx265 -preset ultrafast -crf 6 -an real_ref4k.mp4 2>/dev/null
s=$(now); ffmpeg -hide_banner -loglevel error -y -hwaccel cuda -hwaccel_output_format cuda -i real_ref4k.mp4 -c:v hevc_nvenc -preset p4 -tune hq -rc vbr -cq 26 -b:v 0 -an r_4k_hevc.mp4; e=$(now); fr=$(ffprobe -v error -count_frames -select_streams v -show_entries stream=nb_read_frames -of csv=p=0 r_4k_hevc.mp4); echo "4k hevc_nvenc p4 cq26 frames=$fr fps=$((fr*1000/(e-s))) kbps=$(( $(ffprobe -v error -show_entries format=bit_rate -of csv=p=0 r_4k_hevc.mp4)/1000 )) $(vmaf real_ref4k.mp4 r_4k_hevc.mp4)"
echo DONE
