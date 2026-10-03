package audioio

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary doubles as a fake ffmpeg: started under a name whose stem is "ffmpeg" it records
// its argument list (LO_FAKE_ARGS_FILE) and writes LO_FAKE_OUT_BYTES bytes to its last argument
// (LO_FAKE_FAIL=1 makes it exit 1 instead), so the converter is testable with no real ffmpeg.
func init() {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	if stem != "ffmpeg" {
		return
	}
	if f := os.Getenv("LO_FAKE_ARGS_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}
	if os.Getenv("LO_FAKE_FAIL") == "1" {
		os.Stderr.WriteString("fake ffmpeg: refusing")
		os.Exit(1)
	}
	n := 0
	if v := os.Getenv("LO_FAKE_OUT_BYTES"); v != "" {
		for _, c := range v {
			n = n*10 + int(c-'0')
		}
	}
	_ = os.WriteFile(os.Args[len(os.Args)-1], make([]byte, n), 0o644)
	os.Exit(0)
}

func fakeFFmpegBin(t *testing.T) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	dst := filepath.Join(t.TempDir(), "ffmpeg"+ext)
	if err := os.Link(os.Args[0], dst); err != nil {
		b, rerr := os.ReadFile(os.Args[0])
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(dst, b, 0o755); werr != nil {
			t.Fatal(werr)
		}
	}
	return dst
}

func srcFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.m4a")
	if err := os.WriteFile(p, []byte("not really audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildOpusArgs(t *testing.T) {
	got := strings.Join(buildOpusArgs("/tmp/in.mp4", "/tmp/out.ogg"), " ")
	for _, want := range []string{"-i /tmp/in.mp4", "-vn", "-ar 16000", "-ac 1", "-c:a libopus", "-b:a 32k", "-f ogg", "/tmp/out.ogg"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "/tmp/out.ogg") {
		t.Errorf("args %q: the output must be the last argument", got)
	}
}

// The converter hands back a non-empty .ogg and a cleanup that removes its temp dir, and runs ffmpeg
// with the speech settings (16 kHz mono Opus at 32 kbps).
func TestConvertToOpus16kProducesAnOggAndCleansUp(t *testing.T) {
	ff := fakeFFmpegBin(t)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("LO_FAKE_ARGS_FILE", argsFile)
	t.Setenv("LO_FAKE_OUT_BYTES", "1234")
	out, cleanup, err := ConvertToOpus16k(srcFile(t), ff)
	if err != nil {
		t.Fatalf("ConvertToOpus16k: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() != 1234 || !strings.HasSuffix(out, ".ogg") {
		t.Fatalf("output %q: stat %v err %v, want the fake's 1234-byte .ogg", out, fi, err)
	}
	b, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(b), "libopus") || !strings.Contains(string(b), "32k") {
		t.Fatalf("ffmpeg was run with %q, want libopus at 32k", b)
	}
	cleanup()
	cleanup() // safe twice
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("cleanup left %q behind (stat err %v)", out, err)
	}
}

func TestConvertToOpus16kRefusesAMissingInput(t *testing.T) {
	if _, _, err := ConvertToOpus16k("does-not-exist.m4a", "ffmpeg"); err == nil {
		t.Fatal("expected an error for a missing audio file")
	}
}

// A failing ffmpeg, or one that writes nothing, is an error and leaves no temp dir behind.
func TestConvertToOpus16kFailuresLeaveNothingBehind(t *testing.T) {
	ff := fakeFFmpegBin(t)
	for name, env := range map[string]map[string]string{
		"ffmpeg fails":      {"LO_FAKE_FAIL": "1"},
		"ffmpeg writes zero": {"LO_FAKE_OUT_BYTES": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			out, cleanup, err := ConvertToOpus16k(srcFile(t), ff)
			if err == nil {
				cleanup()
				t.Fatalf("expected an error, got %q", out)
			}
			left, _ := filepath.Glob(filepath.Join(tmp, "lo-audio-*"))
			if len(left) != 0 {
				t.Fatalf("a failed conversion left %v behind", left)
			}
		})
	}
}

// Integration: a real ffmpeg with libopus turns a 2 s stereo tone into a small 16 kHz mono Opus file.
func TestConvertToOpus16kIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping integration test")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "tone.wav")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2:sample_rate=44100", "-ac", "2", src).CombinedOutput(); err != nil {
		t.Skipf("could not synth test audio: %v (%s)", err, out)
	}
	ogg, cleanup, err := ConvertToOpus16k(src, "ffmpeg")
	if err != nil {
		t.Skipf("this ffmpeg cannot encode libopus: %v", err)
	}
	defer cleanup()
	fi, err := os.Stat(ogg)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("expected a non-empty ogg at %q (err=%v)", ogg, err)
	}
	wavSize := int64(2 * 44100 * 2 * 2)
	if fi.Size() > wavSize/20 {
		t.Errorf("ogg is %d bytes for 2 s of audio, want well under %d (32 kbps is about 8 KB)", fi.Size(), wavSize/20)
	}
	if probe, perr := exec.LookPath("ffprobe"); perr == nil {
		out, err := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels", "-of", "csv=p=0", ogg).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe: %v (%s)", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != "opus,48000,1" {
			t.Errorf("ffprobe says %q, want opus mono (libopus stores 48000 Hz and decodes at the request's rate)", got)
		}
	}
}
