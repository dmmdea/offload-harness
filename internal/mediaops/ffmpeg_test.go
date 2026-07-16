package mediaops

import (
	"strings"
	"testing"
)

func join(a []string) string { return strings.Join(a, " ") }

// --- BuildFFmpegArgs -----------------------------------------------------------

func TestBuildFFmpegArgs_TrimCopyIsDefault(t *testing.T) {
	args, err := BuildFFmpegArgs(MediaRequest{Op: "trim", In: "in.mp4", Out: "out.mp4", Start: "3", Duration: "2.5"})
	if err != nil {
		t.Fatal(err)
	}
	s := join(args)
	// fast seek: -ss BEFORE -i; stream copy by default (keyframe-snapped, no re-encode)
	if !strings.Contains(s, "-ss 3") || !strings.Contains(s, "-t 2.5") {
		t.Fatalf("trim window missing: %s", s)
	}
	if strings.Index(s, "-ss") > strings.Index(s, "-i in.mp4") {
		t.Fatalf("-ss must precede -i for fast seek: %s", s)
	}
	if !strings.Contains(s, "-c copy") {
		t.Fatalf("default trim must stream-copy: %s", s)
	}
	if !strings.Contains(s, "-y") {
		t.Fatalf("must overwrite out (-y): %s", s)
	}
}

func TestBuildFFmpegArgs_TrimReencodeDropsCopy(t *testing.T) {
	args, err := BuildFFmpegArgs(MediaRequest{Op: "trim", In: "in.mp4", Out: "out.mp4", Start: "3", Duration: "2", Reencode: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(join(args), "-c copy") {
		t.Fatalf("reencode trim must not stream-copy: %s", join(args))
	}
}

func TestBuildFFmpegArgs_ConcatUsesListFile(t *testing.T) {
	args, err := BuildFFmpegArgs(MediaRequest{Op: "concat", ListPath: "list.txt", Out: "out.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	s := join(args)
	for _, need := range []string{"-f concat", "-safe 0", "-i list.txt", "-c copy", "out.mp4"} {
		if !strings.Contains(s, need) {
			t.Fatalf("missing %q in: %s", need, s)
		}
	}
}

func TestBuildConcatList_EscapesSingleQuotes(t *testing.T) {
	content, err := BuildConcatList([]string{"a.mp4", "it's here.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "file 'a.mp4'") {
		t.Fatalf("plain entry malformed: %s", content)
	}
	// concat demuxer escaping: ' -> '\''
	if !strings.Contains(content, `file 'it'\''s here.mp4'`) {
		t.Fatalf("quote escaping wrong: %s", content)
	}
	if _, err := BuildConcatList([]string{"only-one.mp4"}); err == nil {
		t.Fatal("concat with <2 inputs must error")
	}
}

func TestBuildFFmpegArgs_ExtractFramesFpsPattern(t *testing.T) {
	args, err := BuildFFmpegArgs(MediaRequest{Op: "extract_frames", In: "in.mp4", Out: `D:\frames\frame_%05d.png`, FPS: 2})
	if err != nil {
		t.Fatal(err)
	}
	s := join(args)
	if !strings.Contains(s, "fps=2") || !strings.Contains(s, `frame_%05d.png`) {
		t.Fatalf("frame extraction argv wrong: %s", s)
	}
}

func TestBuildFFmpegArgs_ConvertFlags(t *testing.T) {
	a, _ := BuildFFmpegArgs(MediaRequest{Op: "convert", In: "in.mp4", Out: "out.m4a", AudioOnly: true})
	if !strings.Contains(join(a), "-vn") {
		t.Fatalf("audio_only must drop video (-vn): %s", join(a))
	}
	v, _ := BuildFFmpegArgs(MediaRequest{Op: "convert", In: "in.mp4", Out: "out.mp4", VideoOnly: true})
	if !strings.Contains(join(v), "-an") {
		t.Fatalf("video_only must drop audio (-an): %s", join(v))
	}
}

func TestBuildFFmpegArgs_MuxAudioMapsBothInputs(t *testing.T) {
	args, err := BuildFFmpegArgs(MediaRequest{Op: "mux_audio", In: "v.mp4", Audio: "a.m4a", Out: "out.mp4", Shortest: true})
	if err != nil {
		t.Fatal(err)
	}
	s := join(args)
	for _, need := range []string{"-i v.mp4", "-i a.m4a", "-map 0:v:0", "-map 1:a:0", "-c:v copy", "-shortest"} {
		if !strings.Contains(s, need) {
			t.Fatalf("missing %q in: %s", need, s)
		}
	}
}

func TestBuildFFmpegArgs_Validation(t *testing.T) {
	cases := []MediaRequest{
		{Op: "nope", In: "x", Out: "y"},                      // unknown op
		{Op: "trim", Out: "y", Start: "0", Duration: "1"},    // missing in
		{Op: "trim", In: "x", Out: "y"},                      // missing window
		{Op: "extract_frames", In: "x", Out: "y"},            // neither fps nor count resolved
		{Op: "mux_audio", In: "v.mp4", Out: "out.mp4"},       // missing audio
	}
	for _, c := range cases {
		if _, err := BuildFFmpegArgs(c); err == nil {
			t.Fatalf("op %q with bad args must error", c.Op)
		}
	}
}

// --- ParseProbe (fixtures captured from the REAL imageio ffmpeg 7.1 on this stack) ---

const probeVideo = `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'wan-native-q8-verify.mp4':
    prompt          : {"1": {"class_type": "LoadImage"}}
  Duration: 00:00:02.06, start: 0.000000, bitrate: 1425 kb/s
  Stream #0:0[0x1](und): Video: h264 (High) (avc1 / 0x31637661), yuv420p(tv, bt709, progressive), 1280x720, 1407 kb/s, 16 fps, 16 tbr, 16384 tbn (default)
`

const probeImage = `Input #0, png_pipe, from 'banding-fixed-o1graph-bf16.png':
  Duration: N/A, bitrate: N/A
  Stream #0:0: Video: png, rgb24(pc, gbr/unknown/unknown), 2048x2048, 25 fps, 25 tbr, 25 tbn
`

const probeAudio = `Input #0, wav, from 'tone.wav':
  Duration: 00:00:02.00, bitrate: 1411 kb/s
  Stream #0:0: Audio: pcm_s16le ([1][0][0][0] / 0x0001), 44100 Hz, stereo, s16, 1411 kb/s
`

func TestParseProbe_Video(t *testing.T) {
	p, err := ParseProbe(probeVideo)
	if err != nil {
		t.Fatal(err)
	}
	if p.DurationSec < 2.05 || p.DurationSec > 2.07 {
		t.Fatalf("duration = %v, want ~2.06", p.DurationSec)
	}
	if p.Format != "mov,mp4,m4a,3gp,3g2,mj2" {
		t.Fatalf("format = %q", p.Format)
	}
	if len(p.Streams) != 1 || p.Streams[0].Type != "video" || p.Streams[0].Codec != "h264" {
		t.Fatalf("streams = %+v", p.Streams)
	}
	if p.Streams[0].Width != 1280 || p.Streams[0].Height != 720 || p.Streams[0].FPS != 16 {
		t.Fatalf("video geometry = %+v", p.Streams[0])
	}
}

func TestParseProbe_ImageHasNoDuration(t *testing.T) {
	p, err := ParseProbe(probeImage)
	if err != nil {
		t.Fatal(err)
	}
	if p.DurationSec != 0 {
		t.Fatalf("N/A duration must parse as 0, got %v", p.DurationSec)
	}
	if p.Streams[0].Width != 2048 || p.Streams[0].Height != 2048 {
		t.Fatalf("image geometry = %+v", p.Streams[0])
	}
}

func TestParseProbe_Audio(t *testing.T) {
	p, err := ParseProbe(probeAudio)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Streams) != 1 || p.Streams[0].Type != "audio" || p.Streams[0].Codec != "pcm_s16le" {
		t.Fatalf("streams = %+v", p.Streams)
	}
	if p.Streams[0].SampleRate != 44100 {
		t.Fatalf("sample rate = %d", p.Streams[0].SampleRate)
	}
}

func TestParseProbe_GarbageErrors(t *testing.T) {
	if _, err := ParseProbe("not an ffmpeg banner at all"); err == nil {
		t.Fatal("garbage input must error (banner-format drift must fail loudly)")
	}
}
