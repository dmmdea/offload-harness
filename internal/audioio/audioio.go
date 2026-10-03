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

// buildOpusArgs builds the ffmpeg argument list for the upload form of a transcription: decode in,
// drop video, downmix to mono, resample to 16 kHz (the rate whisper reads at), encode Opus at 32 kbps
// tuned for speech into an Ogg container at out. The output is the last argument.
func buildOpusArgs(in, out string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "libopus",
		"-b:a", "32k",
		"-application", "voip",
		"-f", "ogg",
		out,
	}
}

// ConvertToOpus16k transcodes audioPath to a 16 kHz mono Opus/Ogg file at about 32 kbps in a fresh
// temp dir and returns its path plus a cleanup func that removes the dir (safe to call twice). It is
// the form a transcription travels in to a fleet node (the stt upload door): about 14 MB per hour of
// speech where the 16 kHz WAV the local path feeds whisper is 115 MB. ffmpegPath is the ffmpeg
// executable ("" => "ffmpeg"). A missing input or an ffmpeg failure (a build without libopus
// included) returns an error and leaves nothing behind; the caller may then send the original file.
func ConvertToOpus16k(audioPath, ffmpegPath string) (string, func(), error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if _, err := os.Stat(audioPath); err != nil {
		return "", nil, fmt.Errorf("audioio: audio %q: %w", audioPath, err)
	}
	dir, err := os.MkdirTemp("", "lo-audio-up-*")
	if err != nil {
		return "", nil, fmt.Errorf("audioio: tempdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	out := filepath.Join(dir, "audio16k.ogg")
	cmd := exec.Command(ffmpegPath, buildOpusArgs(audioPath, out)...)
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
