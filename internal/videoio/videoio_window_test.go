package videoio

import (
	"strings"
	"testing"
)

func TestBuildFFmpegWindowArgsSeeksBeforeInput(t *testing.T) {
	args := buildFFmpegWindowArgs("in.mp4", "out_%03d.jpg", 12.5, 8, 1, 8, 768)
	joined := strings.Join(args, " ")
	// input-side seek: -ss/-t must precede -i so the decode starts at the window
	ss := strings.Index(joined, "-ss 12.500")
	tt := strings.Index(joined, "-t 8.000")
	in := strings.Index(joined, "-i in.mp4")
	if ss < 0 || tt < 0 || in < 0 || ss > in || tt > in {
		t.Fatalf("window args wrong: %s", joined)
	}
	if !strings.Contains(joined, "fps=1,scale=768:-1") || !strings.Contains(joined, "-frames:v 8") {
		t.Fatalf("filter/cap wrong: %s", joined)
	}
}

func TestProbePathFollowsFFmpeg(t *testing.T) {
	cases := map[string]string{
		"":                                  "ffprobe",
		"ffmpeg":                            "ffprobe",
		"C:/tools/ffmpeg-9/bin/ffmpeg.exe":  "C:/tools/ffmpeg-9/bin/ffprobe.exe",
		"/usr/bin/ffmpeg":                   "/usr/bin/ffprobe",
		"C:/x/imageio_ffmpeg/ffmpeg-win.exe": "ffprobe", // not literally ffmpeg -> PATH lookup
	}
	for in, want := range cases {
		if got := ProbePath(in); strings.ReplaceAll(got, "\\", "/") != want {
			t.Errorf("ProbePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSampleFramesWindowRejectsBadWindow(t *testing.T) {
	if _, err := SampleFramesWindow("no-such.mp4", "", 0, 5, 1, 4, 512, 1<<20); err == nil {
		t.Fatal("missing file must error")
	}
}
