// Package media holds the pure, injectable file-level helpers behind
// `comfyui stage`, `comfyui provenance`, and `comfyui outputs`.
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY THIS PACKAGE EXISTS. Two facts about ComfyUI drive everything here:
//
//  1. LoadImage takes a BARE FILENAME resolved inside ComfyUI's input dir, and
//     /history records only that filename. Once the input dir is cleaned, every
//     archived run referencing it becomes unreproducible. Content-addressing the
//     staged file (HashFile + StagedName) is what keeps a months-old
//     matched still+prompt+seed comparison valid.
//
//  2. ComfyUI embeds workflow metadata in PNGs ONLY — never in mp4/webm, which is
//     most of what gets produced here — and no ComfyUI endpoint reports width,
//     height, fps, duration, or audio presence of the produced FILE. Those are
//     exactly the columns a cross-model comparison needs, and ffprobe is the tool
//     that has them. It is optional, so every entry point here is written to be
//     skipped gracefully rather than to fail.
//
// Everything in this package is pure or takes its side effects through an
// injected seam (LookPathFunc, RunFunc), so the tests parse a captured ffprobe
// payload WITHOUT ffprobe installed.
package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// HashAlgorithm names the digest recorded in input_asset.content_sha256.
const HashAlgorithm = "sha256"

// shortSHALen is the prefix length used when a hash has to be readable
// (staged filenames, human output). 12 hex chars = 48 bits, which is far past
// the collision horizon for one workstation's input corpus while staying
// short enough to type.
const shortSHALen = 12

// FFprobeMissingNote is the exact operator-facing sentence used when ffprobe is
// absent. Probing is an enrichment, never a requirement: the command still
// lists its rows and exits zero.
const FFprobeMissingNote = "ffprobe not found on PATH — media probing skipped; width/height/fps/duration_s/has_audio stay unfilled. Install FFmpeg (it ships ffprobe) to record them: no ComfyUI endpoint reports these properties."

// ---------------------------------------------------------------------------
// Content hashing
// ---------------------------------------------------------------------------

// HashReader streams r through SHA-256 and returns the hex digest plus the
// number of bytes read. Streaming (rather than ReadFile) matters because staged
// inputs here include multi-hundred-megabyte video.
func HashReader(r io.Reader) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("hash: nil reader")
	}
	h := sha256.New()
	n, err := io.Copy(h, bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		return "", n, fmt.Errorf("hashing content: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashFile returns the SHA-256 hex digest and byte size of the file at path.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return HashReader(f)
}

// ShortSHA truncates a hex digest to a readable prefix. Safe on short input.
func ShortSHA(s string) string {
	if len(s) <= shortSHALen {
		return s
	}
	return s[:shortSHALen]
}

// ---------------------------------------------------------------------------
// Staged naming
// ---------------------------------------------------------------------------

// StagedName derives the filename a host file is staged under inside ComfyUI's
// input dir: the sanitised stem, a short content hash, and the original
// extension.
//
// The hash in the name is load-bearing, not decoration. Two different host
// files routinely share a base name ("input.png", "ref.jpg"); staging the
// second one under the same name would overwrite the first and silently change
// what every archived run referencing that name actually consumed. With the
// content hash in the name, a name collision can only mean identical bytes.
func StagedName(hostPath, sha256Hex string) string {
	base := filepath.Base(filepath.FromSlash(hostPath))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		// A dotfile such as .hidden: filepath.Ext claims the whole name as the
		// extension. Keep it as the stem instead of producing a nameless file.
		stem, ext = strings.TrimLeft(base, "."), ""
	}
	stem = sanitizeSegment(stem)
	if stem == "" {
		stem = "input"
	}
	ext = sanitizeSegment(ext)
	short := ShortSHA(strings.TrimSpace(sha256Hex))
	if short == "" {
		return stem + ext
	}
	return stem + "-" + short + ext
}

// sanitizeSegment reduces a path segment to characters that survive a round
// trip through ComfyUI's input dir, a JSON graph, and a shell.
func sanitizeSegment(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_-")
	if len(out) > 64 {
		out = strings.Trim(out[:64], "_-")
	}
	return out
}

// ValidateComfyFilename enforces the rule that costs a whole run when broken:
// LoadImage takes a name resolved INSIDE ComfyUI's input dir — never an
// absolute host path. A Windows drive-qualified path, a UNC path, a leading
// slash, or a ".." escape all get rejected here rather than at render time.
func ValidateComfyFilename(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("empty filename")
	}
	slashed := filepath.ToSlash(trimmed)
	if strings.HasPrefix(slashed, "//") {
		return fmt.Errorf("%q is a UNC path; LoadImage takes a name relative to ComfyUI's input dir, never a host path", name)
	}
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(trimmed) {
		return fmt.Errorf("%q is an absolute path; LoadImage takes a name relative to ComfyUI's input dir, never a host path", name)
	}
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return fmt.Errorf("%q is drive-qualified; LoadImage takes a name relative to ComfyUI's input dir, never a host path", name)
	}
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return fmt.Errorf("%q escapes ComfyUI's input dir", name)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ffprobe: availability, invocation, parsing
// ---------------------------------------------------------------------------

// LookPathFunc resolves an executable name to a path. Injected so availability
// can be tested without ffprobe installed.
type LookPathFunc func(file string) (string, error)

// RunFunc executes a command and returns its stdout. Injected so parsing can be
// tested against a captured payload.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Availability reports whether media probing can run at all.
type Availability struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Note      string `json:"note,omitempty"`
}

// SystemLookPath is the default LookPathFunc.
func SystemLookPath(file string) (string, error) { return exec.LookPath(file) }

// SystemRunner is the default RunFunc. ffprobe writes diagnostics to stderr, so
// a failure carries the last stderr line rather than a bare exit status.
func SystemRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := lastLine(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, detail)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// FFprobeAvailability probes for ffprobe through the injected lookup. A missing
// binary is a documented skip, never an error return: callers list their rows
// and surface Note.
func FFprobeAvailability(look LookPathFunc) Availability {
	if look == nil {
		look = SystemLookPath
	}
	path, err := look("ffprobe")
	if err != nil || strings.TrimSpace(path) == "" {
		return Availability{Available: false, Note: FFprobeMissingNote}
	}
	return Availability{Available: true, Path: path}
}

// FFprobeArgs returns the argv (after the binary) for a single-file probe.
// `-v error` keeps stdout pure JSON, which is what ParseInfo consumes.
func FFprobeArgs(path string) []string {
	return []string{
		"-hide_banner",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	}
}

// Info is the probed description of one produced FILE.
type Info struct {
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	DurationS  float64 `json:"duration_s,omitempty"`
	HasAudio   bool    `json:"has_audio"`
	StillImage bool    `json:"still_image"`
	VideoCodec string  `json:"video_codec,omitempty"`
	AudioCodec string  `json:"audio_codec,omitempty"`
	FormatName string  `json:"format_name,omitempty"`
	Frames     int64   `json:"frames,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
}

type ffprobeStream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
	Duration     string `json:"duration"`
	NBFrames     string `json:"nb_frames"`
	Channels     int    `json:"channels"`
}

type ffprobeFormat struct {
	Filename   string `json:"filename"`
	NBStreams  int    `json:"nb_streams"`
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
	Size       string `json:"size"`
}

type ffprobePayload struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// stillImageDemuxers are the ffprobe format names used for single-image input.
// They matter because ffprobe reports a FABRICATED frame rate for a still (a
// PNG comes back as 25/1), and recording 25 fps for a PNG would poison exactly
// the cross-model comparison this column exists to serve.
var stillImageDemuxers = map[string]bool{
	"image2":      true,
	"image2pipe":  true,
	"png_pipe":    true,
	"mjpeg_pipe":  true,
	"jpeg_pipe":   true,
	"webp_pipe":   true,
	"bmp_pipe":    true,
	"tiff_pipe":   true,
	"gif_pipe":    true,
	"jpegls_pipe": true,
}

// ParseInfo parses `ffprobe -print_format json -show_format -show_streams`
// output into an Info.
//
// Two deliberate rules, each covering a way the raw payload lies:
//   - a still image gets FPS 0 and DurationS 0, whatever r_frame_rate claims;
//   - a rational of "0/0" (which ffprobe emits for avg_frame_rate on many
//     image and VFR streams) is not a rate, so it falls through to r_frame_rate
//     rather than becoming a zero or a divide-by-zero.
func ParseInfo(raw []byte) (Info, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Info{}, errors.New("ffprobe returned an empty payload")
	}
	var payload ffprobePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Info{}, fmt.Errorf("parsing ffprobe json: %w", err)
	}

	info := Info{FormatName: strings.TrimSpace(payload.Format.FormatName)}
	if n, err := strconv.ParseInt(strings.TrimSpace(payload.Format.Size), 10, 64); err == nil && n > 0 {
		info.Bytes = n
	}

	var video *ffprobeStream
	for i := range payload.Streams {
		s := &payload.Streams[i]
		switch strings.ToLower(strings.TrimSpace(s.CodecType)) {
		case "video":
			if video == nil {
				video = s
			}
		case "audio":
			if !info.HasAudio {
				info.HasAudio = true
				info.AudioCodec = strings.TrimSpace(s.CodecName)
			}
		}
	}

	info.StillImage = isStillFormat(info.FormatName)
	if video != nil {
		info.Width, info.Height = video.Width, video.Height
		info.VideoCodec = strings.TrimSpace(video.CodecName)
		if n, err := strconv.ParseInt(strings.TrimSpace(video.NBFrames), 10, 64); err == nil && n > 0 {
			info.Frames = n
		}
		// A one-frame video stream is a still no matter which demuxer read it.
		if info.Frames == 1 && !info.HasAudio {
			info.StillImage = true
		}
	}

	if info.StillImage {
		// FPS and duration are meaningless for a still; leave them zero rather
		// than recording ffprobe's fabricated 25/1.
		return info, nil
	}

	if video != nil {
		if fps, ok := ParseRational(video.AvgFrameRate); ok {
			info.FPS = fps
		} else if fps, ok := ParseRational(video.RFrameRate); ok {
			info.FPS = fps
		}
	}
	if d, ok := parseSeconds(payload.Format.Duration); ok {
		info.DurationS = d
	} else if video != nil {
		if d, ok := parseSeconds(video.Duration); ok {
			info.DurationS = d
		}
	}
	return info, nil
}

func isStillFormat(formatName string) bool {
	for _, part := range strings.Split(formatName, ",") {
		if stillImageDemuxers[strings.ToLower(strings.TrimSpace(part))] {
			return true
		}
	}
	return false
}

// ParseRational converts an ffprobe rate field ("30000/1001", "24/1", "24",
// "0/0", "N/A") to a float. The bool reports whether the value is a usable
// rate; "0/0" and "N/A" are not.
func ParseRational(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	num, den, hasSlash := strings.Cut(s, "/")
	if !hasSlash {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v <= 0 {
			return 0, false
		}
		return v, true
	}
	n, errN := strconv.ParseFloat(strings.TrimSpace(num), 64)
	d, errD := strconv.ParseFloat(strings.TrimSpace(den), 64)
	if errN != nil || errD != nil || d == 0 || n <= 0 {
		return 0, false
	}
	return n / d, true
}

func parseSeconds(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// Probe runs ffprobe against filePath through the injected runner and parses
// the result. ffprobePath may be empty, in which case "ffprobe" is resolved on
// PATH by the runner.
func Probe(ctx context.Context, run RunFunc, ffprobePath, filePath string) (Info, error) {
	if run == nil {
		run = SystemRunner
	}
	bin := strings.TrimSpace(ffprobePath)
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := run(ctx, bin, FFprobeArgs(filePath)...)
	if err != nil {
		return Info{}, fmt.Errorf("probing %s: %w", filepath.Base(filePath), err)
	}
	return ParseInfo(out)
}
