// Package audioio converts a local audio (or video) file into the 16 kHz mono
// 16-bit PCM WAV that whisper.cpp expects, using ffmpeg. It drops any video
// stream (-vn) so a video source only ships its audio downstream, and NEVER
// fetches a remote URL — only local files. Mirrors internal/videoio.
package audioio

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// buildFFmpegArgs builds the ffmpeg argument list: decode in, drop video, downmix
// to mono, resample to 16 kHz, encode signed-16 PCM, write a WAV to out.
func buildFFmpegArgs(in, out string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		"-f", "wav",
		out,
	}
}

// ConvertToWav16k transcodes audioPath to a 16 kHz mono PCM WAV in a fresh temp
// dir and returns the wav path plus a cleanup func that removes the temp dir.
// ffmpegPath is the ffmpeg executable ("" => "ffmpeg"). The caller MUST defer
// cleanup() (it is also safe to call twice). A missing input or an ffmpeg
// failure returns an error and leaves nothing behind.
func ConvertToWav16k(audioPath, ffmpegPath string) (string, func(), error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if _, err := os.Stat(audioPath); err != nil {
		return "", nil, fmt.Errorf("audioio: audio %q: %w", audioPath, err)
	}
	dir, err := os.MkdirTemp("", "lo-audio-*")
	if err != nil {
		return "", nil, fmt.Errorf("audioio: tempdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	out := filepath.Join(dir, "audio16k.wav")
	cmd := exec.Command(ffmpegPath, buildFFmpegArgs(audioPath, out)...)
	if o, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("audioio: ffmpeg failed: %w (%s)", err, string(o))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		cleanup()
		return "", nil, fmt.Errorf("audioio: ffmpeg produced no audio for %q", audioPath)
	}
	return out, cleanup, nil
}
