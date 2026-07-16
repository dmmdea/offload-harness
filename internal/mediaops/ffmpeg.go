// Package mediaops implements the offload_edit_image / offload_media engines:
// deterministic image edits (PIL worker + GIMP design-file conversion) and ffmpeg
// av operations. Everything here is CPU work — NO GPU lock, no llama-swap eviction;
// these ops run in parallel with renders. Spec:
// docs/superpowers/specs/2026-07-16-edit-media-tools-design.md.
package mediaops

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MediaRequest is one offload_media operation. Exactly one op per call (spec §1).
type MediaRequest struct {
	Op        string   // trim | concat | extract_frames | convert | mux_audio | probe
	In        string   // input path (all ops except concat)
	Inputs    []string // concat inputs (>=2)
	ListPath  string   // concat: path of the demuxer list file the caller wrote
	Out       string   // output path (probe: unused)
	Start     string   // trim: seconds or hh:mm:ss
	End       string   // trim: absolute end time (resolved to Duration in RunMedia)
	Duration  string   // trim: seconds
	FPS       float64  // extract_frames: sampling rate
	Count     int      // extract_frames: total frames (resolved to FPS via probe in RunMedia)
	Audio     string   // mux_audio: audio input path
	Shortest  bool     // mux_audio: stop at the shorter input (default true at the handler)
	Reencode  bool     // trim: re-encode for exact cuts instead of keyframe-snapped -c copy
	AudioOnly bool     // convert: drop video (-vn)
	VideoOnly bool     // convert: drop audio (-an)
}

// BuildFFmpegArgs assembles the ffmpeg argv for one media op. Pure — no filesystem,
// no exec — so every op's argv is unit-testable. The caller prepends the ffmpeg path.
func BuildFFmpegArgs(r MediaRequest) ([]string, error) {
	switch r.Op {
	case "trim":
		if r.In == "" || r.Out == "" {
			return nil, fmt.Errorf("trim: in and out are required")
		}
		if r.Start == "" && r.Duration == "" {
			return nil, fmt.Errorf("trim: a window is required (start and/or duration)")
		}
		// -ss BEFORE -i = fast input seek (keyframe-snapped with -c copy).
		args := []string{"-y"}
		if r.Start != "" {
			args = append(args, "-ss", r.Start)
		}
		if r.Duration != "" {
			args = append(args, "-t", r.Duration)
		}
		args = append(args, "-i", r.In)
		if !r.Reencode {
			args = append(args, "-c", "copy")
		}
		return append(args, r.Out), nil

	case "concat":
		if r.ListPath == "" || r.Out == "" {
			return nil, fmt.Errorf("concat: list path and out are required")
		}
		return []string{"-y", "-f", "concat", "-safe", "0", "-i", r.ListPath, "-c", "copy", r.Out}, nil

	case "extract_frames":
		if r.In == "" || r.Out == "" {
			return nil, fmt.Errorf("extract_frames: in and out are required")
		}
		if r.FPS <= 0 {
			return nil, fmt.Errorf("extract_frames: fps (or count, resolved via probe) is required")
		}
		fps := strconv.FormatFloat(r.FPS, 'g', -1, 64)
		return []string{"-y", "-i", r.In, "-vf", "fps=" + fps, r.Out}, nil

	case "convert":
		if r.In == "" || r.Out == "" {
			return nil, fmt.Errorf("convert: in and out are required")
		}
		args := []string{"-y", "-i", r.In}
		if r.AudioOnly {
			args = append(args, "-vn")
		}
		if r.VideoOnly {
			args = append(args, "-an")
		}
		return append(args, r.Out), nil

	case "mux_audio":
		if r.In == "" || r.Audio == "" || r.Out == "" {
			return nil, fmt.Errorf("mux_audio: in (video), audio, and out are required")
		}
		args := []string{"-y", "-i", r.In, "-i", r.Audio, "-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "aac"}
		if r.Shortest {
			args = append(args, "-shortest")
		}
		return append(args, r.Out), nil

	case "probe":
		if r.In == "" {
			return nil, fmt.Errorf("probe: in is required")
		}
		// `ffmpeg -i <in>` with no output exits non-zero by design; the banner on
		// stderr is the product (imageio_ffmpeg ships no ffprobe — spec §1).
		return []string{"-i", r.In}, nil
	}
	return nil, fmt.Errorf("unknown media op %q (trim|concat|extract_frames|convert|mux_audio|probe)", r.Op)
}

// BuildConcatList renders the concat demuxer list file content. Single quotes in
// paths use the demuxer's '\” escape. Pure.
func BuildConcatList(inputs []string) (string, error) {
	if len(inputs) < 2 {
		return "", fmt.Errorf("concat: need at least 2 inputs, got %d", len(inputs))
	}
	var b strings.Builder
	for _, in := range inputs {
		esc := strings.ReplaceAll(in, "'", `'\''`)
		fmt.Fprintf(&b, "file '%s'\n", esc)
	}
	return b.String(), nil
}

// Probe is the parsed `ffmpeg -i` banner (spec §1: probe op).
type Probe struct {
	DurationSec float64       `json:"duration_sec"`
	Format      string        `json:"format"`
	Streams     []ProbeStream `json:"streams"`
}

type ProbeStream struct {
	Type       string  `json:"type"` // video | audio
	Codec      string  `json:"codec"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	SampleRate int     `json:"sample_rate,omitempty"`
}

var (
	reInput    = regexp.MustCompile(`(?m)^Input #0, ([^,]+(?:,[^,]+)*?), from`)
	reDuration = regexp.MustCompile(`Duration: (\d+):(\d+):(\d+(?:\.\d+)?)`)
	reVideo    = regexp.MustCompile(`Stream #\d+:\d+(?:\[[^\]]*\])?(?:\([^)]*\))?: Video: ([A-Za-z0-9_]+)[^\n]*?(\d{2,5})x(\d{2,5})`)
	reFPS      = regexp.MustCompile(`(\d+(?:\.\d+)?) fps`)
	reAudio    = regexp.MustCompile(`Stream #\d+:\d+(?:\[[^\]]*\])?(?:\([^)]*\))?: Audio: ([A-Za-z0-9_]+)[^\n]*?(\d+) Hz`)
)

// ParseProbe extracts duration/format/streams from `ffmpeg -i` stderr. The banner
// grammar is fixture-tested against the REAL imageio ffmpeg 7.1 output so a future
// ffmpeg bump that changes the banner fails in tests, not silently in production.
func ParseProbe(stderr string) (Probe, error) {
	var p Probe
	m := reInput.FindStringSubmatch(stderr)
	if m == nil {
		return p, fmt.Errorf("unrecognized ffmpeg banner (no 'Input #0' line) — ffmpeg output format may have changed")
	}
	p.Format = m[1]
	if d := reDuration.FindStringSubmatch(stderr); d != nil {
		h, _ := strconv.ParseFloat(d[1], 64)
		mi, _ := strconv.ParseFloat(d[2], 64)
		s, _ := strconv.ParseFloat(d[3], 64)
		p.DurationSec = h*3600 + mi*60 + s
	}
	for _, line := range strings.Split(stderr, "\n") {
		if v := reVideo.FindStringSubmatch(line); v != nil {
			st := ProbeStream{Type: "video", Codec: v[1]}
			st.Width, _ = strconv.Atoi(v[2])
			st.Height, _ = strconv.Atoi(v[3])
			if f := reFPS.FindStringSubmatch(line); f != nil {
				st.FPS, _ = strconv.ParseFloat(f[1], 64)
			}
			p.Streams = append(p.Streams, st)
			continue
		}
		if a := reAudio.FindStringSubmatch(line); a != nil {
			st := ProbeStream{Type: "audio", Codec: a[1]}
			st.SampleRate, _ = strconv.Atoi(a[2])
			p.Streams = append(p.Streams, st)
		}
	}
	if len(p.Streams) == 0 {
		return p, fmt.Errorf("ffmpeg banner parsed but no streams recognized")
	}
	return p, nil
}
