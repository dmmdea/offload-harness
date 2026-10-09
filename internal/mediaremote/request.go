package mediaremote

// Request mapping: a core.Request for one of the five media tasks becomes the fleet task type, the inner
// payload with the exact JSON field names the node's builders accept (internal/fleetnode/tasks.go: the
// builders mirror the MCP handlers, minus `out`), and the list of local input files that must travel.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The fleet task types (fleetnode: image-gen, video-gen, animate, audio-gen, run-graph, media-job).
const (
	taskImage    = "image-gen"
	taskVideo    = "video-gen"
	taskAnimate  = "animate"
	taskAudio    = "audio-gen"
	taskRunGraph = "run-graph"
	taskMediaJob = "media-job"
)

// maxGraphBytes bounds an inline run-graph payload: the node's /fleet/dispatch body cap is 1 MiB, and the
// envelope and the rest of the payload need room.
const maxGraphBytes = 900 << 10

// maxDispatchBody is the node's POST /fleet/dispatch body cap (fleetnode.maxDispatchBody): the whole
// envelope, payload included, must fit it.
const maxDispatchBody = 1 << 20

// contractError is a request this machine refuses to send: the caller's contract cannot be placed as given.
type contractError struct{ msg string }

func (e *contractError) Error() string { return e.msg }

// input is one local file that travels with the job: the payload field it fills and where it is.
type input struct {
	field string
	path  string
}

// planned is a request mapped for the wire.
type planned struct {
	fleetTask string
	payload   map[string]any
	inputs    []input
	// outDir is run_graph's out_dir. It never travels (the node writes into its own media dir): it is where
	// the fetched outputs land on this machine.
	outDir string
}

// fleetTaskOf maps the pipeline task to the fleet task type.
func fleetTaskOf(t core.TaskType) (string, bool) {
	switch t {
	case core.TaskGenerateImage:
		return taskImage, true
	case core.TaskGenerateVideo:
		return taskVideo, true
	case core.TaskAnimateCharacter:
		return taskAnimate, true
	case core.TaskGenerateAudio:
		return taskAudio, true
	case core.TaskRunGraph:
		return taskRunGraph, true
	}
	return "", false
}

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return strings.TrimSpace(s)
}

// num reads a numeric parameter however a door spelled it: an int or float, or a number in a string (the
// MCP handlers stringify reserve_vram).
func num(p map[string]any, k string) (float64, bool) {
	switch v := p[k].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func flag(p map[string]any, k string) bool {
	b, _ := p[k].(bool)
	return b
}

// setStr copies a non-empty string parameter into the payload under the node's field name.
func setStr(p map[string]any, k string, to map[string]any) {
	if s := str(p, k); s != "" {
		to[k] = s
	}
}

// setPos copies a positive integer parameter (the builders drop zero and negative values).
func setPos(p map[string]any, k string, to map[string]any) {
	if f, ok := num(p, k); ok && f > 0 {
		to[k] = int(f)
	}
}

// setReserve carries reserve_vram as the number the node's video, animate and audio builders decode.
func setReserve(p map[string]any, to map[string]any) {
	if f, ok := num(p, "reserve_vram"); ok && f > 0 {
		to["reserve_vram"] = f
	}
}

// plan maps req to its wire form. A parameter the node's task cannot carry is refused here, by name,
// instead of being dropped silently: a run that differs from the one the caller asked for is not "remote".
func plan(req core.Request) (planned, error) {
	ft, ok := fleetTaskOf(req.Task)
	if !ok {
		return planned{}, &contractError{fmt.Sprintf("task %q is not a media task a fleet node can run for another machine", req.Task)}
	}
	p := req.Params
	pl := planned{fleetTask: ft, payload: map[string]any{}}
	out := pl.payload
	switch req.Task {
	case core.TaskGenerateImage:
		if req.Input == "" {
			return planned{}, &contractError{"generate_image: prompt required"}
		}
		if b, ok := p["refine"].(bool); ok && !b {
			return planned{}, &contractError{"refine=false is not carried by the fleet image-gen task (the node applies its own refiner setting); render it with route local, or drop refine"}
		}
		out["prompt"] = req.Input
		for _, k := range []string{"negative", "family"} {
			setStr(p, k, out)
		}
		if flag(p, "transparent") {
			out["transparent"] = true
		}
		for _, k := range []string{"width", "height", "steps", "seed"} {
			setPos(p, k, out)
		}
	case core.TaskGenerateVideo:
		if req.Input == "" {
			return planned{}, &contractError{"generate_video: prompt required"}
		}
		if str(p, "transformer") != "" {
			return planned{}, &contractError{"transformer is not carried by the fleet video-gen task; use route local for a per-request transformer override"}
		}
		out["prompt"] = req.Input
		for _, k := range []string{"model", "negative"} {
			setStr(p, k, out)
		}
		for _, k := range []string{"fast", "hero", "upscale"} {
			if flag(p, k) {
				out[k] = true
			}
		}
		for _, k := range []string{"frames", "width", "height", "steps", "seed"} {
			setPos(p, k, out)
		}
		setReserve(p, out)
		still := str(p, "still")
		if still == "" {
			still = strings.TrimSpace(req.Image)
		}
		if still != "" {
			pl.inputs = append(pl.inputs, input{"still", still})
		}
	case core.TaskAnimateCharacter:
		if req.Input == "" {
			return planned{}, &contractError{"animate_character: prompt required"}
		}
		ref, driver := str(p, "ref"), str(p, "driver")
		if ref == "" {
			ref = strings.TrimSpace(req.Image)
		}
		if driver == "" {
			driver = strings.TrimSpace(req.Video)
		}
		if ref == "" || driver == "" {
			return planned{}, &contractError{"animate_character: ref and driver required"}
		}
		out["prompt"] = req.Input
		for _, k := range []string{"motion_prompt", "negative", "pose_strength", "ref_strength"} {
			setStr(p, k, out)
		}
		for _, k := range []string{"width", "height", "frames", "steps", "seed"} {
			setPos(p, k, out)
		}
		setReserve(p, out)
		pl.inputs = append(pl.inputs, input{"ref", ref}, input{"driver", driver})
	case core.TaskGenerateAudio:
		if req.Input == "" {
			return planned{}, &contractError{"generate_audio: text required"}
		}
		if str(p, "tts_voice") != "" {
			return planned{}, &contractError{"tts_voice is not carried by the fleet audio-gen task (a node's speech server picks its own voice); use route local to name a server-side voice"}
		}
		out["text"] = req.Input
		for _, k := range []string{"kind", "voice", "lang"} {
			setStr(p, k, out)
		}
		for _, k := range []string{"seconds", "seed"} {
			setPos(p, k, out)
		}
		setReserve(p, out)
		if clone := str(p, "clone"); clone != "" {
			pl.inputs = append(pl.inputs, input{"clone", clone})
		}
	case core.TaskRunGraph:
		if hasDevices(p) {
			return planned{}, &contractError{"devices is not carried by the fleet run-graph task (a card id names a card on this machine, and a node places the graph on its own cards); use route local to name a card"}
		}
		graph, err := readJSONObject(str(p, "graph_path"), "graph")
		if err != nil {
			return planned{}, err
		}
		if graph == nil {
			return planned{}, &contractError{"run_graph: graph_path or graph_json required"}
		}
		out["graph"] = graph
		manifest, err := readJSONObject(str(p, "manifest_path"), "manifest")
		if err != nil {
			return planned{}, err
		}
		if manifest != nil {
			out["manifest"] = manifest
		}
		setStr(p, "reserve_vram", out)
		setStr(p, "model_family", out)
		// The graph and the manifest share ONE dispatch body with the envelope and the other fields: each
		// may be under its cap and the two together over what the node reads.
		if total := len(graph) + len(manifest); total > maxGraphBytes {
			return planned{}, &contractError{fmt.Sprintf("the graph (%d bytes) and the manifest (%d bytes) together are over the %d bytes a fleet dispatch carries inline", len(graph), len(manifest), maxGraphBytes)}
		}
		pl.outDir = str(p, "out_dir")
	}
	for _, in := range pl.inputs {
		fi, err := os.Stat(in.path)
		if err != nil {
			return planned{}, &contractError{fmt.Sprintf("%s: %v", in.field, err)}
		}
		if !fi.Mode().IsRegular() {
			return planned{}, &contractError{fmt.Sprintf("%s: %s is not a regular file", in.field, in.path)}
		}
	}
	return pl, nil
}

// hasDevices reports whether the caller declared cards (run_graph's operator-only devices): a non-empty list
// or a non-blank string.
func hasDevices(p map[string]any) bool {
	switch v := p["devices"].(type) {
	case []string:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case string:
		return strings.TrimSpace(v) != ""
	}
	return false
}

// readJSONObject reads a graph or manifest file the caller named as raw JSON the node takes inline. An empty
// path is "not given" (nil, nil).
func readJSONObject(path, what string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	// The size is asked first, so a large file named by mistake (a video passed as a graph) is refused
	// without being read into memory; the length check after the read still holds for a file that grew.
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxGraphBytes {
		return nil, &contractError{fmt.Sprintf("%s is %d bytes, over the %d bytes a fleet dispatch carries inline", what, fi.Size(), maxGraphBytes)}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &contractError{fmt.Sprintf("%s: %v", what, err)}
	}
	if len(b) > maxGraphBytes {
		return nil, &contractError{fmt.Sprintf("%s is %d bytes, over the %d bytes a fleet dispatch carries inline", what, len(b), maxGraphBytes)}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, &contractError{fmt.Sprintf("%s must be a JSON object: %v", what, err)}
	}
	if len(obj) == 0 {
		return nil, &contractError{what + " must be a non-empty JSON object"}
	}
	return json.RawMessage(b), nil
}

var extRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

// bundleName is the bare name a local file travels under: the payload field plus the file's own extension
// (when it has a plain one). A caller's file name never reaches the node, so no name a caller picks can be
// a traversal, a reserved device name or a collision between two inputs.
func bundleName(field, path string) string {
	if ext := filepath.Ext(path); extRe.MatchString(ext) {
		return field + strings.ToLower(ext)
	}
	return field
}

var errNoOutput = errors.New("the node's result names no output file")
