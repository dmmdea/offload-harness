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
	"strings"

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
		// ffmpeg 9 mjpeg strictness — see buildFFmpegWindowArgs.
		"-strict", "unofficial",
		"-frames:v", strconv.Itoa(maxFrames),
		outPattern,
	}
}

// buildFFmpegWindowArgs is buildFFmpegArgs restricted to one time window: it
// seeks to start (input-side `-ss`, keyframe-fast; the decoder then delivers
// accurate frames from that point) and decodes dur seconds. Frame timestamps in
// the output are relative to the window; callers add start back.
func buildFFmpegWindowArgs(videoPath, outPattern string, start, dur, fps float64, maxFrames, width int) []string {
	vf := fmt.Sprintf("fps=%s,scale=%d:-1", strconv.FormatFloat(fps, 'g', -1, 64), width)
	// A tail stub shorter than one sampling interval starves the fps filter —
	// it emits nothing, and (measured on ffmpeg 9, 2026-08-27) the lazily-opened
	// mjpeg encoder then fails at EOF-flush on limited-range YUV input, exit -1.
	// Sample such a window as ONE plain frame instead of deferring it.
	if dur*fps < 1 {
		vf = fmt.Sprintf("scale=%d:-1", width)
		maxFrames = 1
	}
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		"-t", strconv.FormatFloat(dur, 'f', 3, 64),
		"-i", videoPath,
		"-vf", vf,
		// ffmpeg 9 makes the mjpeg encoder REJECT limited-range YUV (the norm
		// for camera/edited footage) at default strictness; 8.x only warned.
		"-strict", "unofficial",
		"-frames:v", strconv.Itoa(maxFrames),
		outPattern,
	}
}

// SampleFramesWindow is SampleFrames over the [start, start+dur) window of the
// video only. video_watch sweeps a whole file window by window with it, so a
// long clip is seen END TO END instead of only its first maxFrames/fps seconds.
func SampleFramesWindow(videoPath, ffmpegPath string, start, dur, fps float64, maxFrames, width, maxBytesPerFrame int) ([]string, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if _, err := os.Stat(videoPath); err != nil {
		return nil, fmt.Errorf("videoio: video %q: %w", videoPath, err)
	}
	if dur <= 0 || fps <= 0 || maxFrames <= 0 {
		return nil, fmt.Errorf("videoio: bad window (dur=%g fps=%g maxFrames=%d)", dur, fps, maxFrames)
	}
	dir, err := os.MkdirTemp("", "lo-frames-*")
	if err != nil {
		return nil, fmt.Errorf("videoio: tempdir: %w", err)
	}
	defer os.RemoveAll(dir)

	pattern := filepath.Join(dir, "frame_%03d.jpg")
	cmd := exec.Command(ffmpegPath, buildFFmpegWindowArgs(videoPath, pattern, start, dur, fps, maxFrames, width)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("videoio: ffmpeg failed: %w (%s)", err, string(out))
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "frame_*.jpg"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("videoio: ffmpeg produced no frames for %q in window %.3f+%.3f", videoPath, start, dur)
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

// ProbePath derives the ffprobe executable that ships next to ffmpegPath
// ("ffmpeg" -> "ffmpeg's sibling ffprobe"); an unqualified "ffmpeg" resolves
// through PATH the same way.
func ProbePath(ffmpegPath string) string {
	if ffmpegPath == "" {
		return "ffprobe"
	}
	dir, base := filepath.Split(ffmpegPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.EqualFold(stem, "ffmpeg") {
		return filepath.Join(dir, "ffprobe"+ext)
	}
	return "ffprobe"
}

// Duration returns the container duration of videoPath in seconds via ffprobe.
func Duration(videoPath, ffmpegPath string) (float64, error) {
	if _, err := os.Stat(videoPath); err != nil {
		return 0, fmt.Errorf("videoio: video %q: %w", videoPath, err)
	}
	cmd := exec.Command(ProbePath(ffmpegPath), "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", videoPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("videoio: ffprobe failed: %w (%s)", err, string(out))
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("videoio: ffprobe returned no duration for %q (%q)", videoPath, strings.TrimSpace(string(out)))
	}
	return d, nil
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
