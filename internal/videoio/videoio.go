// Package videoio samples frames from a local video file (via ffmpeg) into
// data:image/...;base64 URIs for the multimodal vision path. It NEVER fetches a
// remote URL — only local files. Frames, not weights, are the 8GB VRAM pressure,
// so callers cap maxFrames.
package videoio

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/dmmdea/offload-harness/internal/imageio"
)

// buildFFmpegArgs builds the ffmpeg argument list: decode videoPath, sample at
// fps, scale to width (aspect kept), cap to maxFrames, write JPEGs to outPattern.
func buildFFmpegArgs(videoPath, outPattern string, fps float64, maxFrames, width int) []string {
	vf := fmt.Sprintf("fps=%s,scale=%d:-1", strconv.FormatFloat(fps, 'g', -1, 64), width)
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", videoPath,
		"-vf", vf,
		"-frames:v", strconv.Itoa(maxFrames),
		outPattern,
	}
}

// SampleFrames extracts up to maxFrames frames from videoPath at fps, each scaled
// to width px wide, and returns them as data:image/jpeg;base64 URIs (in order).
// ffmpegPath is the ffmpeg executable ("" => "ffmpeg"). A frame exceeding
// maxBytesPerFrame is rejected (guards the activation budget). The temp frame
// dir is always cleaned up.
func SampleFrames(videoPath, ffmpegPath string, fps float64, maxFrames, width, maxBytesPerFrame int) ([]string, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if _, err := os.Stat(videoPath); err != nil {
		return nil, fmt.Errorf("videoio: video %q: %w", videoPath, err)
	}
	dir, err := os.MkdirTemp("", "lo-frames-*")
	if err != nil {
		return nil, fmt.Errorf("videoio: tempdir: %w", err)
	}
	defer os.RemoveAll(dir)

	pattern := filepath.Join(dir, "frame_%03d.jpg")
	cmd := exec.Command(ffmpegPath, buildFFmpegArgs(videoPath, pattern, fps, maxFrames, width)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("videoio: ffmpeg failed: %w (%s)", err, string(out))
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "frame_*.jpg"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("videoio: ffmpeg produced no frames for %q", videoPath)
	}
	sort.Strings(matches)
	if len(matches) > maxFrames {
		matches = matches[:maxFrames]
	}

	uris := make([]string, 0, len(matches))
	for _, m := range matches {
		uri, err := imageio.LoadImageB64(m, maxBytesPerFrame)
		if err != nil {
			return nil, fmt.Errorf("videoio: load frame %q: %w", m, err)
		}
		uris = append(uris, uri)
	}
	return uris, nil
}
