package audioio

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBuildFFmpegArgs(t *testing.T) {
	args := buildFFmpegArgs("/tmp/in.mp4", "/tmp/out.wav")
	got := strings.Join(args, " ")
	for _, want := range []string{"-i /tmp/in.mp4", "-vn", "-ar 16000", "-ac 1", "pcm_s16le", "/tmp/out.wav"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

func TestConvertMissingFile(t *testing.T) {
	_, _, err := ConvertToWav16k("does-not-exist.m4a", "ffmpeg")
	if err == nil {
		t.Fatal("expected error for a missing audio file")
	}
}

// Integration: only runs when ffmpeg is actually available.
func TestConvertIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping integration test")
	}
	tmp := t.TempDir()
	src := tmp + "/tone.wav"
	// 1s 440Hz sine at 44.1kHz stereo — exercises downmix + resample.
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1:sample_rate=44100", "-ac", "2", src).CombinedOutput(); err != nil {
		t.Skipf("could not synth test audio: %v (%s)", err, out)
	}
	wav, cleanup, err := ConvertToWav16k(src, "ffmpeg")
	if err != nil {
		t.Fatalf("ConvertToWav16k: %v", err)
	}
	defer cleanup()
	fi, err := os.Stat(wav)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("expected a non-empty wav at %q (err=%v)", wav, err)
	}
	if !strings.HasSuffix(wav, ".wav") {
		t.Errorf("expected a .wav path, got %q", wav)
	}
	cleanup()
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Errorf("cleanup should remove the temp wav; stat err=%v", err)
	}
}
