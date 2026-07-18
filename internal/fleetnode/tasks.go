// Task mapping + payload translation for fleet dispatch: the fleet contract's
// task_type vocabulary (image-gen / video-gen / stt / audio-gen / run-graph) is
// translated to core.Request exactly the way the MCP tools build them
// (internal/mcpserver handleGenerate*/handleTranscribe/handleRunGraph are the
// mirrored siblings) so a fleet job renders byte-for-byte like a local call.
// Advertised tasks are DERIVED from the machine's actual bindings, never
// hardcoded. Every error returned here is a caller mistake — the server maps
// them to 400s at ACK time, so a malformed fleet job dies with a clear reason,
// never mid-render.

package fleetnode

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fleetTaskOrder is the advertisement order (stable for health payloads + error
// messages). Membership is decided per-config by taskConfigured.
var fleetTaskOrder = []string{"image-gen", "video-gen", "stt", "audio-gen", "run-graph"}

// taskConfigured reports whether THIS box actually serves taskType — the same
// route gates the pipeline uses (empty script/model = the task defers there, so
// advertising it to the fleet would be a lie).
func taskConfigured(cfg config.Config, taskType string) bool {
	switch taskType {
	case "image-gen":
		return cfg.ImageGenScript != ""
	case "video-gen":
		return cfg.VideoGenScript != ""
	case "stt":
		return cfg.STTModel != ""
	case "audio-gen":
		return cfg.VoiceGenScript != "" || cfg.MusicGenScript != ""
	case "run-graph":
		return cfg.RunGraphScript != ""
	}
	return false
}

// SupportedTasks returns the fleet task_types this machine's config actually
// serves, in stable order. nil when nothing is bound.
func SupportedTasks(cfg config.Config) []string {
	var out []string
	for _, t := range fleetTaskOrder {
		if taskConfigured(cfg, t) {
			out = append(out, t)
		}
	}
	return out
}

// familyFor returns the footprint/advertisement model family for a task on this
// box (the spec's table): image = the machine's imagegen_family binding (else
// "sdxl"), video = "wan2.2" (the bound recipe family), stt = "whisper",
// audio = "acestep", run-graph = "comfy-graph" (payload-declared families are a
// per-job concern, not an advertisement).
func familyFor(cfg config.Config, taskType string) string {
	switch taskType {
	case "image-gen":
		if cfg.ImageGenFamily != "" {
			return cfg.ImageGenFamily
		}
		return "sdxl"
	case "video-gen":
		return "wan2.2"
	case "stt":
		return "whisper"
	case "audio-gen":
		return "acestep"
	case "run-graph":
		return "comfy-graph"
	}
	return ""
}

// Families returns the loadable model families for the advertised tasks,
// deduplicated, in task order. nil when nothing is bound.
func Families(cfg config.Config) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range SupportedTasks(cfg) {
		f := familyFor(cfg, t)
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// BuildRequest translates a fleet dispatch (task_type + opaque payload) into the
// core.Request the pipeline runs, mirroring the MCP handlers' param mapping
// exactly. cleanup is ALWAYS non-nil (defer it unconditionally); for run-graph it
// removes the materialized temp files. Errors are caller mistakes (the server's
// 400s): unknown/unconfigured task_type (listing this box's supported set),
// malformed payload JSON, or a missing/invalid required field.
func BuildRequest(cfg config.Config, taskType string, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	if !taskConfigured(cfg, taskType) {
		return core.Request{}, noop, fmt.Errorf("unsupported task_type %q (supported: %s)",
			taskType, strings.Join(SupportedTasks(cfg), ", "))
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	switch taskType {
	case "image-gen":
		return buildImageGen(payload)
	case "video-gen":
		return buildVideoGen(payload)
	case "stt":
		return buildSTT(payload)
	case "audio-gen":
		return buildAudioGen(payload)
	case "run-graph":
		return buildRunGraph(payload)
	}
	// Unreachable: taskConfigured gates membership. Kept for defense.
	return core.Request{}, noop, fmt.Errorf("unsupported task_type %q (supported: %s)",
		taskType, strings.Join(SupportedTasks(cfg), ", "))
}

// buildImageGen mirrors mcpserver.handleGenerateImage.
func buildImageGen(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Prompt   string `json:"prompt"`
		Negative string `json:"negative"`
		Out      string `json:"out"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Steps    int    `json:"steps"`
		Seed     int    `json:"seed"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("image-gen payload: %w", err)
	}
	if in.Prompt == "" {
		return core.Request{}, noop, fmt.Errorf("image-gen payload: prompt required")
	}
	params := map[string]any{}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Width > 0 {
		params["width"] = in.Width
	}
	if in.Height > 0 {
		params["height"] = in.Height
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	return core.Request{Task: core.TaskGenerateImage, Input: in.Prompt, Params: params}, noop, nil
}

// buildVideoGen mirrors mcpserver.handleGenerateVideo (incl. the LO-19
// fast/hero/upscale flow and the stringified reserve_vram wire shape).
func buildVideoGen(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Prompt      string  `json:"prompt"`
		Still       string  `json:"still"`
		Model       string  `json:"model"`
		Negative    string  `json:"negative"`
		Out         string  `json:"out"`
		Frames      int     `json:"frames"`
		Width       int     `json:"width"`
		Height      int     `json:"height"`
		Steps       int     `json:"steps"`
		Seed        int     `json:"seed"`
		ReserveVRAM float64 `json:"reserve_vram"`
		Fast        bool    `json:"fast"`
		Hero        bool    `json:"hero"`
		Upscale     bool    `json:"upscale"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("video-gen payload: %w", err)
	}
	if in.Prompt == "" {
		return core.Request{}, noop, fmt.Errorf("video-gen payload: prompt required")
	}
	params := map[string]any{}
	if in.Fast {
		params["fast"] = true
	}
	if in.Hero {
		params["hero"] = true
	}
	if in.Upscale {
		params["upscale"] = true
	}
	if in.Still != "" {
		params["still"] = in.Still
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Frames > 0 {
		params["frames"] = in.Frames
	}
	if in.Width > 0 {
		params["width"] = in.Width
	}
	if in.Height > 0 {
		params["height"] = in.Height
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.ReserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(in.ReserveVRAM, 'f', -1, 64)
	}
	return core.Request{Task: core.TaskGenerateVideo, Input: in.Prompt, Params: params}, noop, nil
}

// buildSTT mirrors mcpserver.handleTranscribe (minus the MCP-only select
// projection, which is a response-shaping concern, not a request one).
func buildSTT(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Audio    string `json:"audio"`
		Language string `json:"language"`
		HQ       bool   `json:"hq"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("stt payload: %w", err)
	}
	if in.Audio == "" {
		return core.Request{}, noop, fmt.Errorf("stt payload: audio required (a path readable on this node)")
	}
	params := map[string]any{}
	if in.Language != "" {
		params["language"] = in.Language
	}
	if in.HQ {
		params["hq"] = true
	}
	return core.Request{Task: core.TaskTranscribe, Audio: in.Audio, Params: params}, noop, nil
}

// buildAudioGen mirrors mcpserver.handleGenerateAudio (kind defaulting is the
// pipeline's business; zero/empty optionals are omitted).
func buildAudioGen(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Text        string  `json:"text"`
		Kind        string  `json:"kind"`
		Voice       string  `json:"voice"`
		Clone       string  `json:"clone"`
		Lang        string  `json:"lang"`
		Seconds     int     `json:"seconds"`
		Out         string  `json:"out"`
		Seed        int     `json:"seed"`
		ReserveVRAM float64 `json:"reserve_vram"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("audio-gen payload: %w", err)
	}
	if in.Text == "" {
		return core.Request{}, noop, fmt.Errorf("audio-gen payload: text required")
	}
	params := map[string]any{}
	if in.Kind != "" {
		params["kind"] = in.Kind
	}
	if in.Voice != "" {
		params["voice"] = in.Voice
	}
	if in.Clone != "" {
		params["clone"] = in.Clone
	}
	if in.Lang != "" {
		params["lang"] = in.Lang
	}
	if in.Seconds > 0 {
		params["seconds"] = in.Seconds
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.ReserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(in.ReserveVRAM, 'f', -1, 64)
	}
	return core.Request{Task: core.TaskGenerateAudio, Input: in.Text, Params: params}, noop, nil
}

// buildRunGraph carries graph/manifest as RAW NESTED JSON over the wire (no
// base64, no stringification) and strict-validates at ACK time per the spec:
// graph present, decodable, non-empty JSON object; manifest optional but, when
// present, a JSON object too. Valid payloads are materialized to temp files (the
// runner reads files — mcpserver.materialize's pattern) whose removal is the
// returned cleanup's job.
func buildRunGraph(payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Graph       json.RawMessage `json:"graph"`
		Manifest    json.RawMessage `json:"manifest"`
		OutDir      string          `json:"out_dir"`
		ReserveVram string          `json:"reserve_vram"`
		// ModelFamily is the payload-declared footprint family for THIS graph
		// (the spec's run-graph row): threaded to the pipeline so the passive
		// footprint recorder keys the render correctly; absent = "comfy-graph".
		ModelFamily string `json:"model_family"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("run-graph payload: %w", err)
	}
	if isAbsentJSON(in.Graph) {
		return core.Request{}, noop, fmt.Errorf("run-graph payload: graph required (a ComfyUI API-format graph as a raw JSON object)")
	}
	var graphObj map[string]json.RawMessage
	if err := json.Unmarshal(in.Graph, &graphObj); err != nil {
		return core.Request{}, noop, fmt.Errorf("run-graph payload: graph must be a JSON object: %v", err)
	}
	if len(graphObj) == 0 {
		return core.Request{}, noop, fmt.Errorf("run-graph payload: graph must be a non-empty JSON object")
	}
	if !isAbsentJSON(in.Manifest) {
		var manifestObj map[string]json.RawMessage
		if err := json.Unmarshal(in.Manifest, &manifestObj); err != nil {
			return core.Request{}, noop, fmt.Errorf("run-graph payload: manifest must be a JSON object: %v", err)
		}
	}

	graphPath, err := materializeRaw(in.Graph, "fleet-run-graph-*.json")
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("run-graph payload: graph: %w", err)
	}
	manifestPath := ""
	if !isAbsentJSON(in.Manifest) {
		manifestPath, err = materializeRaw(in.Manifest, "fleet-run-graph-manifest-*.json")
		if err != nil {
			os.Remove(graphPath)
			return core.Request{}, noop, fmt.Errorf("run-graph payload: manifest: %w", err)
		}
	}
	cleanup := func() {
		os.Remove(graphPath)
		if manifestPath != "" {
			os.Remove(manifestPath)
		}
	}
	params := map[string]any{
		"graph_path":    graphPath,
		"manifest_path": manifestPath,
		"out_dir":       in.OutDir,
		"reserve_vram":  in.ReserveVram,
	}
	if in.ModelFamily != "" {
		params["model_family"] = in.ModelFamily
	}
	return core.Request{Task: core.TaskRunGraph, Params: params}, cleanup, nil
}

// isAbsentJSON treats a missing field and an explicit null the same way (both
// mean "not provided" for optional/required checks).
func isAbsentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// materializeRaw writes raw JSON to a temp file and returns its path — the
// fleet-side twin of mcpserver.materialize (the render runners read files, not
// inline JSON).
func materializeRaw(raw json.RawMessage, pattern string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
