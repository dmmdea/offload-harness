package videoio

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBuildFFmpegArgs(t *testing.T) {
	args := buildFFmpegArgs("/tmp/in.mp4", "/tmp/out/frame_%03d.jpg", 1.0, 12, 768)
	got := strings.Join(args, " ")
	for _, want := range []string{"-i /tmp/in.mp4", "fps=1", "scale=768:-1", "-frames:v 12", "/tmp/out/frame_%03d.jpg"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

func TestSampleFramesMissingFile(t *testing.T) {
	_, err := SampleFrames("does-not-exist.mp4", "ffmpeg", 1.0, 4, 512, 6000000)
	if err == nil {
		t.Fatal("expected error for a missing video file")
	}
}

// Integration: only runs when ffmpeg is actually available.
func TestSampleFramesIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping integration test")
	}
	tmp := t.TempDir()
	src := tmp + "/src.mp4"
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=5", src).CombinedOutput(); err != nil {
		t.Skipf("could not synth test video: %v (%s)", err, out)
	}
	uris, err := SampleFrames(src, "ffmpeg", 1.0, 4, 320, 6000000)
	if err != nil {
		t.Fatalf("SampleFrames: %v", err)
	}
	if len(uris) == 0 || len(uris) > 4 {
		t.Fatalf("got %d frames, want 1..4", len(uris))
	}
	for i, u := range uris {
		if !strings.HasPrefix(u, "data:image/") {
			t.Errorf("frame %d not a data:image URI: %.40q", i, u)
		}
	}
}
