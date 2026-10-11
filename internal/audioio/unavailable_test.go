package audioio

// F22 follow-up (2026-10-10): every failure of the conversion filed as "audio convert: ..." and so as the
// caller's bad input, including the one where ffmpeg could not be run at all (the box has none, the configured
// path is wrong). That is the stack's fault, and a ledger that sends an operator to look at a file when the box
// has no ffmpeg is wrong. A run that never started (anything but an *exec.ExitError) is typed
// ErrFFmpegUnavailable; ffmpeg running and refusing the input is not.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFFmpegThatCannotBeRunIsTypedUnavailableInBothConverters(t *testing.T) {
	in := filepath.Join(t.TempDir(), "a.mp3")
	if err := os.WriteFile(in, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-ffmpeg")
	for name, convert := range map[string]func(string, string) (string, func(), error){"wav": ConvertToWav16k, "opus": ConvertToOpus16k} {
		_, _, err := convert(in, missing)
		if err == nil {
			t.Fatalf("%s: converted with an ffmpeg that does not exist", name)
		}
		if !errors.Is(err, ErrFFmpegUnavailable) {
			t.Errorf("%s: an ffmpeg that could not be run must be ErrFFmpegUnavailable: %v", name, err)
		}
		if errors.Is(err, ErrNoAudioStream) {
			t.Errorf("%s: not 'no audio stream': %v", name, err)
		}
	}
}

func TestFFmpegThatRanAndRefusedTheInputIsNotUnavailable(t *testing.T) {
	in := filepath.Join(t.TempDir(), "broken.mp3")
	if err := os.WriteFile(in, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOIO_FAKE_FFMPEG", "invalid") // exits 183 after printing what ffmpeg prints
	for name, convert := range map[string]func(string, string) (string, func(), error){"wav": ConvertToWav16k, "opus": ConvertToOpus16k} {
		_, _, err := convert(in, os.Args[0])
		if err == nil {
			t.Fatalf("%s: a failing ffmpeg converted", name)
		}
		if errors.Is(err, ErrFFmpegUnavailable) {
			t.Errorf("%s: ffmpeg ran and refused the file: that is the caller's input, not an unavailable ffmpeg: %v", name, err)
		}
	}
}
