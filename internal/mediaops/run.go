package mediaops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrEngineAbsent marks a defer-class failure: the machine simply lacks the
// engine (unset/missing binary). Handlers turn any error into a clean defer;
// this sentinel lets the reason say "engine absent" instead of "failed".
var ErrEngineAbsent = errors.New("engine absent on this machine")

// EditConfig carries this machine's edit-engine bindings (from config).
type EditConfig struct {
	Python      string        // resolved PIL python ("" = absent)
	GimpConsole string        // gimp-console path ("" = absent)
	Worker      string        // absolute path of render/edit_image.py
	Timeout     time.Duration // edit_timeout_sec
}

// EditRequest is one offload_edit_image call (ops already shaped; validated here).
type EditRequest struct {
	Image      string
	Ops        []EditOp
	Out        string
	Renditions []Rendition // optional export matrix derived from the master Out
}

// EditResult mirrors the tool's return payload (spec §1).
type EditResult struct {
	Out        string   `json:"image_path"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	OpsApplied int      `json:"ops_applied"`
	Layers     []Layer  `json:"layers,omitempty"`
	Renditions []string `json:"renditions,omitempty"`
	Engine     string   `json:"engine"`
}

// RunEditImage validates and executes a whole edit pipeline: optional GIMP
// flatten first (design files), then the PIL worker for the remaining ops.
func RunEditImage(ctx context.Context, cfg EditConfig, req EditRequest) (EditResult, error) {
	var res EditResult
	if req.Image == "" || req.Out == "" {
		return res, fmt.Errorf("image and out are required")
	}
	if err := ValidateOps(req.Ops); err != nil {
		return res, err
	}
	if err := ValidateRenditions(req.Renditions); err != nil {
		return res, err
	}
	if _, err := os.Stat(req.Image); err != nil {
		return res, fmt.Errorf("input image not found: %s", req.Image)
	}

	pilInput, ops := req.Image, req.Ops
	res.Engine = "pil"
	if UsesGimp(req.Ops) {
		gimpOp := req.Ops[0]
		if cfg.GimpConsole == "" {
			return res, fmt.Errorf("%s needs GIMP (gimp_console_path unset): %w", gimpOp.Op, ErrEngineAbsent)
		}
		if _, err := os.Stat(cfg.GimpConsole); err != nil {
			return res, fmt.Errorf("gimp_console_path %q not found: %w", cfg.GimpConsole, ErrEngineAbsent)
		}
		tmpDir, err := os.MkdirTemp("", "flatten-*")
		if err != nil {
			return res, err
		}
		defer os.RemoveAll(tmpDir)
		flatPng := filepath.Join(tmpDir, "flat.png")
		layersTxt := filepath.Join(tmpDir, "layers.txt")
		var script string
		if gimpOp.Op == "instantiate_design" {
			for name, p := range gimpOp.ReplaceImage {
				if _, err := os.Stat(p); err != nil {
					return res, fmt.Errorf("instantiate_design replace_image[%q]: file not found: %s", name, p)
				}
			}
			script, err = BuildInstantiateScript(req.Image, flatPng, gimpOp.SetText, gimpOp.ReplaceImage)
		} else {
			script, err = BuildGimpScript(req.Image, flatPng, layersTxt)
		}
		if err != nil {
			return res, err
		}
		// serialize headless GIMPs: concurrent consoles contend on the profile lock
		gimpMu.Lock()
		_, stderr, err := runCapture(ctx, cfg.Timeout, nil, nil, cfg.GimpConsole, GimpArgs(script)...)
		gimpMu.Unlock()
		if err != nil {
			return res, fmt.Errorf("gimp %s failed: %s", gimpOp.Op, tail(stderr, 300))
		}
		if fi, err := os.Stat(flatPng); err != nil || fi.Size() == 0 {
			// a script-fu error (e.g. layer name mismatch) can exit 0 but produce no
			// raster — surface GIMP's stderr, it names the failing call
			return res, fmt.Errorf("gimp produced no raster for %s (%s)", req.Image, tail(stderr, 300))
		}
		if b, err := os.ReadFile(layersTxt); err == nil {
			res.Layers = ParseLayerList(string(b))
		}
		pilInput, ops = flatPng, req.Ops[1:]
		res.Engine = "gimp+pil"
	}

	if cfg.Python == "" {
		return res, fmt.Errorf("image editing needs the PIL engine (edit_python unresolvable): %w", ErrEngineAbsent)
	}
	w, h, err := runEditWorker(ctx, cfg, pilInput, ops, req.Out)
	if err != nil {
		return res, err
	}
	res.Out, res.Width, res.Height = req.Out, w, h
	res.OpsApplied = len(req.Ops)

	// export matrix: each rendition is a fresh worker pass over the master out
	// (resize + convert), writing <out-stem><suffix>.<format> beside it.
	for _, r := range req.Renditions {
		rOut := renditionOut(req.Out, r)
		rOps := []EditOp{
			{Op: "resize", Width: r.Width, Height: r.Height},
			{Op: "convert", Format: r.Format},
		}
		if _, _, err := runEditWorker(ctx, cfg, req.Out, rOps, rOut); err != nil {
			return res, fmt.Errorf("rendition %q: %w", r.Suffix, err)
		}
		res.Renditions = append(res.Renditions, rOut)
	}
	return res, nil
}

// runEditWorker runs one render/edit_image.py invocation (image -> ops -> out)
// and returns the produced width/height.
func runEditWorker(ctx context.Context, cfg EditConfig, image string, ops []EditOp, outPath string) (int, int, error) {
	payload, _ := json.Marshal(map[string]any{"image": image, "ops": ops, "out": outPath})
	stdout, _, err := runCapture(ctx, cfg.Timeout, payload, nil, cfg.Python, cfg.Worker)
	if err != nil {
		// the worker prints {"error": ...} on stdout for arg/pipeline failures
		if msg := workerError(stdout); msg != "" {
			return 0, 0, fmt.Errorf("edit pipeline: %s", msg)
		}
		return 0, 0, err
	}
	var out struct {
		Out    string `json:"out"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
		N      int    `json:"ops_applied"`
	}
	if jerr := json.Unmarshal([]byte(lastJSONLine(stdout)), &out); jerr != nil {
		return 0, 0, fmt.Errorf("edit worker returned no result JSON (%s)", tail(stdout, 200))
	}
	return out.Width, out.Height, nil
}

// MediaConfig carries the ffmpeg binding.
type MediaConfig struct {
	FFmpeg  string        // ffmpeg_path ("" = absent)
	Timeout time.Duration // edit_timeout_sec governs media ops too
}

// MediaResult is the op-specific payload (exactly one field group is set).
type MediaResult struct {
	MediaPath   string   `json:"media_path,omitempty"`
	DurationSec float64  `json:"duration_sec,omitempty"`
	Frames      []string `json:"frames,omitempty"`
	Count       int      `json:"count,omitempty"`
	Probe       *Probe   `json:"probe,omitempty"`
}

// RunMedia executes one offload_media op end-to-end.
func RunMedia(ctx context.Context, cfg MediaConfig, req MediaRequest) (MediaResult, error) {
	var res MediaResult
	if cfg.FFmpeg == "" {
		return res, fmt.Errorf("media ops need ffmpeg (ffmpeg_path unset): %w", ErrEngineAbsent)
	}
	if _, err := os.Stat(cfg.FFmpeg); err != nil {
		return res, fmt.Errorf("ffmpeg_path %q not found: %w", cfg.FFmpeg, ErrEngineAbsent)
	}

	switch req.Op {
	case "probe":
		args, err := BuildFFmpegArgs(req)
		if err != nil {
			return res, err
		}
		// `ffmpeg -i` with no output exits 1 BY DESIGN; the banner on stderr is the
		// product. ParseProbe failing = the input wasn't readable media (or banner drift).
		_, stderr, err := runCapture(ctx, cfg.Timeout, nil, func(code int) bool { return code == 1 }, cfg.FFmpeg, args...)
		if err != nil {
			return res, err
		}
		p, perr := ParseProbe(stderr)
		if perr != nil {
			return res, fmt.Errorf("probe %s: %v (%s)", req.In, perr, tail(stderr, 200))
		}
		res.Probe = &p
		res.DurationSec = p.DurationSec
		return res, nil

	case "trim":
		// end -> duration (the builder takes start+duration only)
		if req.End != "" && req.Duration == "" {
			start := 0.0
			if req.Start != "" {
				s, err := ToSeconds(req.Start)
				if err != nil {
					return res, err
				}
				start = s
			}
			end, err := ToSeconds(req.End)
			if err != nil {
				return res, err
			}
			if end <= start {
				return res, fmt.Errorf("trim: end (%s) must be after start (%s)", req.End, req.Start)
			}
			req.Duration = strconv.FormatFloat(end-start, 'g', -1, 64)
		}
		return runToOut(ctx, cfg, req)

	case "concat":
		content, err := BuildConcatList(req.Inputs)
		if err != nil {
			return res, err
		}
		for _, in := range req.Inputs {
			if _, err := os.Stat(in); err != nil {
				return res, fmt.Errorf("concat input not found: %s", in)
			}
		}
		f, err := os.CreateTemp("", "concat-*.txt")
		if err != nil {
			return res, err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return res, err
		}
		f.Close()
		req.ListPath = f.Name()
		return runToOut(ctx, cfg, req)

	case "extract_frames":
		if req.FPS <= 0 && req.Count > 0 {
			// count -> fps via probe (spec §1)
			p, err := RunMedia(ctx, cfg, MediaRequest{Op: "probe", In: req.In})
			if err != nil {
				return res, err
			}
			if p.DurationSec <= 0 {
				return res, fmt.Errorf("extract_frames count=%d needs a probed duration; %s has none", req.Count, req.In)
			}
			req.FPS = float64(req.Count) / p.DurationSec
		}
		// out is a directory or a pattern; a directory gets the default pattern
		if !strings.Contains(req.Out, "%") {
			if err := os.MkdirAll(req.Out, 0o755); err != nil {
				return res, err
			}
			req.Out = filepath.Join(req.Out, "frame_%05d.png")
		}
		if _, err := runToOutNoStat(ctx, cfg, req); err != nil {
			return res, err
		}
		frames, _ := filepath.Glob(strings.NewReplacer("%05d", "*", "%04d", "*", "%d", "*").Replace(req.Out))
		if len(frames) == 0 {
			return res, fmt.Errorf("extract_frames produced no frames at %s", req.Out)
		}
		res.Frames, res.Count = frames, len(frames)
		return res, nil

	case "convert", "mux_audio":
		return runToOut(ctx, cfg, req)
	}
	return res, fmt.Errorf("unknown media op %q", req.Op)
}

// runToOut builds args, runs ffmpeg, and verifies a non-empty Out.
func runToOut(ctx context.Context, cfg MediaConfig, req MediaRequest) (MediaResult, error) {
	res, err := runToOutNoStat(ctx, cfg, req)
	if err != nil {
		return res, err
	}
	if fi, err := os.Stat(req.Out); err != nil || fi.Size() == 0 {
		return res, fmt.Errorf("%s produced no output at %s", req.Op, req.Out)
	}
	res.MediaPath = req.Out
	return res, nil
}

func runToOutNoStat(ctx context.Context, cfg MediaConfig, req MediaRequest) (MediaResult, error) {
	var res MediaResult
	if req.In != "" {
		if _, err := os.Stat(req.In); err != nil {
			return res, fmt.Errorf("input not found: %s", req.In)
		}
	}
	args, err := BuildFFmpegArgs(req)
	if err != nil {
		return res, err
	}
	if _, stderr, err := runCapture(ctx, cfg.Timeout, nil, nil, cfg.FFmpeg, args...); err != nil {
		return res, fmt.Errorf("%s: %v (%s)", req.Op, err, tail(stderr, 200))
	}
	return res, nil
}

// ToSeconds parses "ss", "ss.ms", "mm:ss", or "hh:mm:ss(.ms)" into seconds.
func ToSeconds(v string) (float64, error) {
	if v == "" {
		return 0, fmt.Errorf("empty time")
	}
	parts := strings.Split(v, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("bad time %q", v)
	}
	total := 0.0
	for _, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("bad time %q", v)
		}
		total = total*60 + f
	}
	return total, nil
}

func workerError(stdout string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(lastJSONLine(stdout)), &e) == nil {
		return e.Error
	}
	return ""
}

func lastJSONLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}
