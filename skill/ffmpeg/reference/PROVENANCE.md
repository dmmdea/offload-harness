# Provenance of this copy

This is the **sanitized public snapshot** of the operator's FFmpeg reference library, built
2026-09-01 by running ~200 ffmpeg/ffprobe commands on two real machines (a Windows workstation with
ffmpeg 8.1.2 Gyan full build + three NVIDIA Blackwell GPUs, and an Ubuntu node with ffmpeg 8.0.1
+ an RTX 3050). Machine names, user paths and footage locations were replaced with placeholders
(`<scratch>`, `%USERPROFILE%`, `<linux-node>` …); every number is unchanged. The live capability
dumps (`live-dump-*.json`) and raw measurement logs stay in the operator's private evidence repo
because they embed local paths; regenerate your own with `ffdump.py`.

`trac-2026-09-01/` holds plain-text extracts of the FFmpeg trac wiki pages (CC BY-SA, ffmpeg.org)
that the `[doc-trac]` tags cite.
