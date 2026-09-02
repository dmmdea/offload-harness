import subprocess, json, sys, re, platform, datetime, shutil, os
ff = sys.argv[1] if len(sys.argv) > 1 else "ffmpeg"
def run(*a):
    p = subprocess.run([ff, "-hide_banner", *a], capture_output=True, text=True, errors="replace")
    return p.stdout + p.stderr
ver = run("-version")
def table(flag, minus_start):
    out = run(flag); rows = {}
    for line in out.splitlines():
        m = re.match(r"^\s*([A-Z.TSC]{2,}|\S+)\s+(\S+)\s+(.*)$", line)
        if not m: continue
        flags, name, desc = m.groups()
        if name in ("=", "-", ":"): continue
        if not re.search(r"[A-Za-z0-9_]", name): continue
        if flags.startswith(("Encoders","Decoders","Filters","File","Hardware","-")): continue
        rows[name] = {"flags": flags, "desc": desc.strip()}
    return rows
enc = table("-encoders", "V"); dec = table("-decoders", "V"); flt = table("-filters", "")
fmts = table("-formats", "")
hw = [l.strip() for l in run("-hwaccels").splitlines()[1:] if l.strip()]
want_enc = ["h264_nvenc","hevc_nvenc","av1_nvenc","libx264","libx265","libsvtav1","libaom-av1","librav1e","libvpx-vp9","h264_amf","hevc_amf","av1_amf","h264_qsv","hevc_qsv","av1_qsv","h264_mf","hevc_mf","prores_ks","prores","dnxhd","png","apng","gif","libwebp","libwebp_anim","mjpeg","aac","libopus","libmp3lame","flac","pcm_s16le","pcm_s24le","ass","subrip","mov_text","webvtt","ffv1","libvvenc"]
want_dec = ["h264","h264_cuvid","hevc","hevc_cuvid","av1","av1_cuvid","libdav1d","vp9","vp9_cuvid","prores","dnxhd","mjpeg","aac","opus","mp3","flac","ass","srt","subrip","webvtt","mov_text"]
want_flt = ["scale","scale_cuda","scale_npp","scale_vulkan","hwupload","hwupload_cuda","hwdownload","overlay","overlay_cuda","overlay_vulkan","libplacebo","subtitles","ass","drawtext","drawbox","loudnorm","silencedetect","silenceremove","ebur128","astats","volumedetect","sidechaincompress","acompressor","dynaudnorm","atempo","rubberband","asetrate","aresample","setpts","minterpolate","fps","pad","crop","format","palettegen","paletteuse","select","showinfo","signalstats","freezedetect","blackdetect","mpdecimate","concat","amix","amerge","pan","channelmap","channelsplit","volume","afade","fade","xfade","acrossfade","thumbnail","tpad","trim","atrim","split","asplit","null","anull","nullsink","anullsink","testsrc2","sine","color","anoisesrc","zscale","tonemap","yadif","bwdif","deshake","vidstabdetect","vidstabtransform","unsharp","hqdn3d","nlmeans","transpose","hflip","vflip","setsar","setdar","framerate","tmix","chromakey","colorkey","alphamerge","alphaextract","apad","aecho","adelay","highpass","lowpass","afftdn","arnndn","anlmdn","compand","alimiter","speechnorm","whisper","libvmaf","ssim","psnr","scale2ref","scdet","detelecine","idet","showspectrum","showwaves","showvolume","aphasemeter","ametadata","metadata","sendcmd","zmq","frei0r","lut3d","haldclut","curves","eq","hue","colorbalance","colorchannelmixer","gblur","boxblur","edgedetect","pixelize","dilation","erosion"]
want_fmt = ["mp4","mov","matroska","webm","gif","image2","image2pipe","concat","lavfi","mp3","wav","flac","ogg","ass","srt","webvtt","mpegts","hls","dash","apng","webp","png_pipe","rawvideo","null","ffmetadata","segment","avi","mxf"]
def sub(rows, want):
    return {k: (rows[k] if k in rows else None) for k in want}
def parse_ver(v):
    m = re.search(r"ffmpeg version (\S+)", v); return m.group(1) if m else None
def enc_opts(name):
    if name not in enc: return None
    return run("-h", "encoder=" + name)
nv = {}
for n in ("h264_nvenc","hevc_nvenc","av1_nvenc","libx264","libx265","libsvtav1","libaom-av1"):
    o = enc_opts(n)
    if o:
        presets = re.findall(r"^\s+(\S+)\s+\d+\s+E\.\.V", o, re.M)
        nv[n] = {"presets_and_enums": presets[:200], "help_text_bytes": len(o), "supported_pixfmts": (re.search(r"Supported pixel formats: (.*)", o) or [None, None])[1]}
d = {
 "host": platform.node(), "os": platform.platform(), "captured_utc": datetime.datetime.utcnow().isoformat()+"Z",
 "ffmpeg_path": shutil.which(ff) or ff, "ffmpeg_version": parse_ver(ver),
 "version_banner": ver.splitlines()[:2], "configuration": (re.search(r"configuration: (.*)", ver) or [None,None])[1],
 "libs": [l.strip() for l in ver.splitlines() if l.startswith("lib")],
 "hwaccels": hw,
 "counts": {"encoders": len(enc), "decoders": len(dec), "filters": len(flt), "formats": len(fmts)},
 "encoders_subset": sub(enc, want_enc), "decoders_subset": sub(dec, want_dec),
 "filters_subset": sub(flt, want_flt), "formats_subset": sub(fmts, want_fmt),
 "encoder_options": nv,
 "all_encoder_names": sorted(enc), "all_filter_names": sorted(flt),
}
json.dump(d, open(sys.argv[2], "w"), indent=1)
print("wrote", sys.argv[2], d["ffmpeg_version"], d["counts"], "hw:", hw)
