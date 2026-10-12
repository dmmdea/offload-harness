package audioio

// F21 (2026-10-09): a transcription of a file with no audio stream deferred with ffmpeg's own text
// ("audio convert: audioio: ffmpeg failed: exit status 234 ([out#0/wav @ 0x...] Output file does not
// contain any stream ...)"). The conversion now returns ErrNoAudioStream for it, which the doors above turn
// into "the input has no audio stream" filed as a bad input.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// TestMain lets this test binary stand in for ffmpeg: a conversion pointed at os.Args[0] with
// AUDIOIO_FAKE_FFMPEG set prints what that mode of ffmpeg prints and exits non-zero. No ffmpeg binary
// is needed, and the words under test are the ones a real one was seen to use.
func TestMain(m *testing.M) {
	switch os.Getenv("AUDIOIO_FAKE_FFMPEG") {
	case "":
		os.Exit(m.Run())
	case "noaudio":
		fmt.Fprintln(os.Stderr, "[out#0/wav @ 000001dc61c17200] Output file does not contain any stream")
		fmt.Fprintln(os.Stderr, "Error opening output file x.wav.")
		fmt.Fprintln(os.Stderr, "Error opening output files: Invalid argument")
		os.Exit(234)
	case "noaudio-old":
		fmt.Fprintln(os.Stderr, "Output file #0 does not contain any stream")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "x.mp3: Invalid data found when processing input")
		os.Exit(183)
	}
}

func TestAnInputWithNoAudioStreamIsTypedNotAnFFmpegFailure(t *testing.T) {
	in := filepath.Join(t.TempDir(), "silent.mp4")
	if err := os.WriteFile(in, []byte("not a real container"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"noaudio", "noaudio-old"} {
		t.Setenv("AUDIOIO_FAKE_FFMPEG", mode)
		for name, convert := range map[string]func(string, string) (string, func(), error){
			"wav": ConvertToWav16k, "opus": ConvertToOpus16k,
		} {
			path, cleanup, err := convert(in, os.Args[0])
			if err == nil {
				cleanup()
				t.Fatalf("%s/%s: converted an input ffmpeg says holds no stream (%q)", mode, name, path)
			}
			if !errors.Is(err, ErrNoAudioStream) {
				t.Errorf("%s/%s: not ErrNoAudioStream: %v", mode, name, err)
			}
			if strings.Contains(err.Error(), "ffmpeg failed") || strings.Contains(err.Error(), "exit status") {
				t.Errorf("%s/%s: ffmpeg's words leaked into the message: %v", mode, name, err)
			}
			if !strings.Contains(err.Error(), "the input has no audio stream") {
				t.Errorf("%s/%s: the message must say what is wrong with the input: %v", mode, name, err)
			}
		}
	}
}

func TestAnotherFFmpegFailureIsStillAnFFmpegFailure(t *testing.T) {
	in := filepath.Join(t.TempDir(), "broken.mp3")
	if err := os.WriteFile(in, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOIO_FAKE_FFMPEG", "invalid")
	for name, convert := range map[string]func(string, string) (string, func(), error){"wav": ConvertToWav16k, "opus": ConvertToOpus16k} {
		_, _, err := convert(in, os.Args[0])
		if err == nil {
			t.Fatalf("%s: a failing ffmpeg converted", name)
		}
		if errors.Is(err, ErrNoAudioStream) {
			t.Errorf("%s: a corrupt file is not 'no audio stream': %v", name, err)
		}
		if !strings.Contains(err.Error(), "ffmpeg failed") || !strings.Contains(err.Error(), "Invalid data found") {
			t.Errorf("%s: any other failure keeps ffmpeg's text: %v", name, err)
		}
	}
}

// The pattern was taken from ffmpeg 9 on the box this shipped from; this runs it against whatever ffmpeg
// the machine has: a video with no audio track, converted for real.
func TestARealVideoWithNoAudioTrackIsTyped(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	silent := filepath.Join(t.TempDir(), "silent.mp4")
	if out, err := winexec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-pix_fmt", "yuv420p", silent).CombinedOutput(); err != nil {
		t.Skipf("could not synthesise a video with no audio track: %v (%s)", err, out)
	}
	if _, _, err := ConvertToWav16k(silent, ffmpeg); !errors.Is(err, ErrNoAudioStream) {
		t.Errorf("wav: a real ffmpeg on a video with no audio track must give ErrNoAudioStream, got: %v", err)
	}
	if _, _, err := ConvertToOpus16k(silent, ffmpeg); !errors.Is(err, ErrNoAudioStream) {
		// A build without libopus fails differently, which is a different (and already documented) failure.
		if err != nil && strings.Contains(err.Error(), "libopus") {
			t.Skipf("this ffmpeg has no libopus: %v", err)
		}
		t.Errorf("opus: a real ffmpeg on a video with no audio track must give ErrNoAudioStream, got: %v", err)
	}
}
