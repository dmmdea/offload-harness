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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// fleetTaskOrder is the advertisement order (stable for health payloads + error
// messages). Membership is decided per-config by taskConfigured.
var fleetTaskOrder = []string{"image-gen", "video-gen", "stt", "audio-gen", "run-graph", "agent"}

// taskConfigured reports whether THIS box actually serves taskType — the same
// route gates the pipeline uses (empty script/model = the task defers there, so
// advertising it to the fleet would be a lie).
func taskConfigured(cfg config.Config, taskType string) bool {
	switch taskType {
	case "image-gen":
		return cfg.ImageRouteConfigured()   // ComfyUI script OR the sdcpp engine (J2)
	case "video-gen":
		return cfg.VideoGenScript != ""
	case "stt":
		return cfg.STTModel != ""
	case "audio-gen":
		return cfg.VoiceGenScript != "" || cfg.MusicGenScript != ""
	case "run-graph":
		return cfg.RunGraphScript != ""
	case "agent":
		return agentTaskConfigured(cfg)
	}
	// Anything else is only "configured" when it is a VALID cfg.Pipelines key
	// (Task 6): 100% config-driven, so a new pipeline needs no new case here.
	return pipelineNameConfigured(cfg, taskType)
}

// agentTaskConfigured gates the fleet "agent" task's advertisement and
// dispatch (multi-node delegation, Task 4). Three conditions, all required:
//
//  1. cfg.FleetAgentEnabled — an explicit operator opt-in, unlike every other
//     task here (see the config field's doc: the BINDING exists on every tier,
//     the worker ROLE is a per-box decision), so default-off keeps the
//     advertisement byte-identical for nodes that never opted in.
//  2. A resolvable agent seat (config.AgentPlannerModel's fallback chain).
//     Roster LIVENESS is deliberately NOT checked here — this function feeds
//     ack-time 400s and health advertisement, both of which must stay cheap
//     and network-free; whether the seat is actually served is health's
//     roster-probe job (Task 5) and runAgentTask's own probe at run time.
//  3. Safe reachability: a loopback listener, or a bearer token for anything
//     beyond it — the same posture server.go enforces at ack time. Checked
//     here too so an unsafe lane is never even ADVERTISED: a dispatcher must
//     not learn "this node does agent work" from health and then eat a 403.
//     cfg.FleetListen is the config-side view of the bind; an empty value
//     means the operator never overrode config.Default()'s loopback bind, so
//     it counts as loopback (netguard.LoopbackAddr itself fails closed on
//     ""). The ack-time gate keys on the RESOLVED listener and stays
//     authoritative if a --listen flag ever diverges from the config.
func agentTaskConfigured(cfg config.Config) bool {
	if !cfg.FleetAgentEnabled {
		return false
	}
	if cfg.AgentPlannerModel("") == "" {
		return false
	}
	loopback := cfg.FleetListen == "" || netguard.LoopbackAddr(cfg.FleetListen)
	return loopback || cfg.FleetAuthToken != ""
}

// pipelineNameConfigured reports whether taskType is one of cfg.Pipelines'
// VALID entries (config.PipelineNames() already excludes invalid ones).
func pipelineNameConfigured(cfg config.Config, taskType string) bool {
	for _, n := range cfg.PipelineNames() {
		if n == taskType {
			return true
		}
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
	// Config-driven pipeline routes (Task 6) advertise alongside the hardcoded
	// families — already sorted by PipelineNames().
	out = append(out, cfg.PipelineNames()...)
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
//
// ctx is threaded through to the pipeline-job family's ack-time ref fetch
// (buildPipelineJob -> FetchRefs): the caller is server.go's handleDispatch,
// which passes r.Context(), so a dispatch whose client vanishes mid-ack stops
// fetching refs instead of racing to completion against context.Background().
func BuildRequest(ctx context.Context, cfg config.Config, taskType string, payload json.RawMessage) (core.Request, func(), error) {
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
	case "agent":
		return buildAgentRun(cfg, payload)
	}
	// Any other taskType that reaches here (taskConfigured already gated
	// membership) must be a configured cfg.Pipelines key (Task 6) — 100%
	// config-driven, so a new pipeline needs no new case above.
	if pipelineNameConfigured(cfg, taskType) {
		return buildPipelineJob(ctx, cfg, taskType, payload)
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

// buildAgentRun translates a fleet "agent" dispatch (multi-node delegation,
// Task 4) into the core.Request pipeline.runAgentTask executes. Strict path:
// core.DecodeAgentContract owns decode + Validate + the MaxSteps/TimeoutSec
// ceilings (roast delta 5 — clamped THERE, never re-clamped here), so every
// error out of it is an ack-time 400 with the decoder's reason intact.
//
// Two Task-4 rules land here on top of the decoder:
//
//   - Roast delta 3: remote execution REQUIRES OutputSchema. Without one the
//     delegator has nothing mechanical to verify before merging weak-node
//     output, so the contract is refused at ACK — after the network round-trip
//     and loop budget are spent is too late to discover unverifiability.
//   - Roast delta 2: depth is DERIVED on the receiving node — anything arriving
//     over the fleet wire executes at max(1, wireDepth); the wire field is
//     advisory. The derived value REPLACES contract.Depth before the pipeline
//     sees it, so no downstream reader can accidentally trust a wire claim of
//     "origin". In v1 this denies nothing node-side: agent.Build registers no
//     delegate tool for ANY caller, so the hop limit holds structurally
//     (tool absent) — the derived depth is carried so the day a delegate tool
//     exists, its depth>0 registration gate keys off this value, not the wire.
//
// Materialization follows buildPipelineJob's discipline: a job-scoped dir
// under BaseDir()/pipeline-jobs/ (so SweepOrphanedPipelineJobs reclaims a
// crash's leftovers at startup), context docs written under <dir>/context/,
// and a cleanup closure that removes the WHOLE dir — the docs live exactly as
// long as the job. Unlike buildPipelineJob the id is NODE-MINTED (the contract
// carries none), so os.MkdirTemp is the exclusive create: uniqueness by
// construction instead of a caller-collision 400.
func buildAgentRun(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	contract, err := core.DecodeAgentContract(bytes.NewReader(payload))
	if err != nil {
		return core.Request{}, noop, err
	}
	if len(contract.OutputSchema) == 0 {
		return core.Request{}, noop, fmt.Errorf(
			"agent contract: output_schema required for remote execution (the delegator must have a mechanical check before merging)")
	}
	if contract.Depth < 1 {
		contract.Depth = 1
	}

	jobsRoot := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	if mkErr := os.MkdirAll(jobsRoot, 0o755); mkErr != nil {
		return core.Request{}, noop, fmt.Errorf("agent contract: creating pipeline-jobs dir: %w", mkErr)
	}
	jobDir, mkErr := os.MkdirTemp(jobsRoot, "agent-*")
	if mkErr != nil {
		return core.Request{}, noop, fmt.Errorf("agent contract: creating job dir: %w", mkErr)
	}
	contextDir := filepath.Join(jobDir, "context")
	if mkErr := os.MkdirAll(contextDir, 0o755); mkErr != nil {
		os.RemoveAll(jobDir)
		return core.Request{}, noop, fmt.Errorf("agent contract: creating context dir: %w", mkErr)
	}
	for _, d := range contract.Context {
		// Validate held every Name to flat-filename shape (no separators,
		// colons, or dot-dirs), so this Join cannot escape contextDir.
		if werr := os.WriteFile(filepath.Join(contextDir, d.Name), []byte(d.Text), 0o644); werr != nil {
			os.RemoveAll(jobDir)
			return core.Request{}, noop, fmt.Errorf("agent contract: writing context doc %q: %w", d.Name, werr)
		}
	}

	cleanup := func() { os.RemoveAll(jobDir) }
	return core.Request{
		Task:  core.TaskAgentRun,
		Input: contract.Goal,
		Params: map[string]any{
			"contract":    contract,
			"context_dir": contextDir,
			"job_id":      filepath.Base(jobDir),
		},
	}, cleanup, nil
}

// pipelineJobIDPattern validates job_spec.id: it becomes the materialization
// directory name, a filename prefix on every published artifact, and (per the
// CMP CLI contract) a render seed — the same slug-safety rule ingress.go's ref
// keys use (refKeyPattern), kept as its own var since the two validate
// different payload fields for different reasons.
var pipelineJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// buildPipelineJob translates a fleet dispatch for a config-driven pipeline
// route (cfg.Pipelines[taskType], e.g. "scene-swap" — Task 4's PipelineSpec)
// into a core.Request. EVERY validation error surfaces here at ack time (the
// server maps them to 400s); runPipelineJob (internal/pipeline) never
// re-validates the payload.
//
// On a valid payload this ALSO does the network-bound work: it fetches
// image_refs via Task 5's FetchRefs (allowlisted, capped, magic-sniffed) and
// writes the job.json a CLI process will read, with product/logo (and
// background.path, when fetched) injected as absolute local paths — so a bad
// ref URL is ALSO an ack-time 400, never a mid-run surprise. Rules (shared-
// contracts.md, verbatim): job_spec is a JSON object with a slug-valid id;
// tier is a non-empty string; image_refs.product and .logo are required;
// image_refs.background is required IFF job_spec.background.mode=="stock";
// job_spec must not already contain product/logo/background.path — the node,
// never the payload, is authoritative there; job_spec.id must not already
// have an in-flight job on this node (a duplicate dispatch sharing an id with
// a still-running job is refused at ack, never silently reused — see the
// exclusive os.Mkdir below).
//
// Materialization dir: filepath.Join(cfg.BaseDir(), "pipeline-jobs", jobID) —
// assets/ (fetched refs), job.json, out/ (the CLI's --out root; the CLI
// itself creates out/<id>/ and writes artifacts there). jobDir itself is
// created with os.Mkdir (exclusive, NOT MkdirAll): two dispatches racing on
// the same job_spec.id must not both proceed into the same directory, or the
// first job to finish would os.RemoveAll it out from under the second,
// still-running one.
//
// Cleanup-scope decision (brief left this to the implementer): the returned
// cleanup ALWAYS removes the WHOLE job dir, never just assets/. FetchRefs'
// own cleanup only reaches its destDir (assets/), which is not enough — the
// job also has job.json and (later) out/ under the same jobDir, and those
// must vanish together too. So: on any failure AFTER something was written to
// disk, this function calls os.RemoveAll(jobDir) itself before returning (the
// server's error path still calls the returned noop cleanup defensively — see
// server.go's `if err != nil { cleanup(); ... }` — so that stays a safe no-op
// covering an already-clean state). On SUCCESS the returned cleanup closure
// removes the whole jobDir, but is not invoked here — the caller (server.go's
// dispatch handler) defers it until AFTER the run finishes, so assets/job.json
// survive for the whole render, exactly as long as the job needs them, not
// just the ack.
func buildPipelineJob(ctx context.Context, cfg config.Config, taskType string, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	if !pipelineNameConfigured(cfg, taskType) {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: unconfigured pipeline %q", taskType)
	}
	spec := cfg.Pipelines[taskType]

	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	var in struct {
		JobSpec   json.RawMessage   `json:"job_spec"`
		ImageRefs map[string]string `json:"image_refs"`
		Tier      string            `json:"tier"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: %w", err)
	}
	if isAbsentJSON(in.JobSpec) {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: job_spec required (a JSON object)")
	}
	var jobSpec map[string]json.RawMessage
	if err := json.Unmarshal(in.JobSpec, &jobSpec); err != nil {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: job_spec must be a JSON object: %v", err)
	}
	jobID, err := pipelineJobSpecID(jobSpec)
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: %w", err)
	}
	if strings.TrimSpace(in.Tier) == "" {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: tier required (a non-empty string)")
	}
	for _, forbidden := range [2]string{"product", "logo"} {
		if _, has := jobSpec[forbidden]; has {
			return core.Request{}, noop, fmt.Errorf(
				"pipeline-job payload: job_spec must not contain %q (the node injects it from image_refs)", forbidden)
		}
	}
	bgMode, bgHasPath, berr := pipelineBackgroundMode(jobSpec)
	if berr != nil {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: %w", berr)
	}
	if bgHasPath {
		return core.Request{}, noop, fmt.Errorf(
			"pipeline-job payload: job_spec.background must not contain \"path\" (the node injects it from image_refs)")
	}
	if strings.TrimSpace(in.ImageRefs["product"]) == "" {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: image_refs.product required")
	}
	if strings.TrimSpace(in.ImageRefs["logo"]) == "" {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: image_refs.logo required")
	}
	if bgMode == "stock" && strings.TrimSpace(in.ImageRefs["background"]) == "" {
		return core.Request{}, noop, fmt.Errorf(
			"pipeline-job payload: image_refs.background required when job_spec.background.mode is \"stock\"")
	}

	// --- Every validation rule passed. From here on we touch the filesystem
	// and the network, so any failure must clean up whatever it created. ---
	pipelineJobsDir := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	if mkErr := os.MkdirAll(pipelineJobsDir, 0o755); mkErr != nil {
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: creating pipeline-jobs dir: %w", mkErr)
	}
	jobDir := filepath.Join(pipelineJobsDir, jobID)
	// Exclusive create (os.Mkdir, NOT MkdirAll): two dispatches sharing the same
	// job_spec.id would otherwise both proceed into the SAME jobDir, and the
	// first one to finish would os.RemoveAll it out from under the second,
	// still-running job (the cleanup closure below). A collision is therefore
	// an ack-time validation error, not a race resolved later.
	if mkErr := os.Mkdir(jobDir, 0o755); mkErr != nil {
		if os.IsExist(mkErr) {
			return core.Request{}, noop, fmt.Errorf(
				"pipeline-job payload: job_spec.id %q already in flight on this node", jobID)
		}
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: creating job dir: %w", mkErr)
	}
	assetsDir := filepath.Join(jobDir, "assets")
	outRoot := filepath.Join(jobDir, "out")

	refs := map[string]string{
		"product": in.ImageRefs["product"],
		"logo":    in.ImageRefs["logo"],
	}
	if bgMode == "stock" {
		refs["background"] = in.ImageRefs["background"]
	}
	fetched, _, ferr := FetchRefs(ctx, refs, assetsDir, spec.IngressAllow, spec.RefCapMB())
	if ferr != nil {
		os.RemoveAll(jobDir) // FetchRefs' own cleanup only reaches assetsDir; jobDir may still exist (mkdir'd, empty).
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: %w", ferr)
	}

	if mkErr := os.MkdirAll(outRoot, 0o755); mkErr != nil {
		os.RemoveAll(jobDir)
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: creating out root: %w", mkErr)
	}

	jobJSON, jerr := pipelineInjectRefs(jobSpec, fetched, bgMode)
	if jerr != nil {
		os.RemoveAll(jobDir)
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: %w", jerr)
	}
	jobPath := filepath.Join(jobDir, "job.json")
	if werr := os.WriteFile(jobPath, jobJSON, 0o644); werr != nil {
		os.RemoveAll(jobDir)
		return core.Request{}, noop, fmt.Errorf("pipeline-job payload: writing job.json: %w", werr)
	}

	cleanup := func() { os.RemoveAll(jobDir) }
	req := core.Request{
		Task: core.TaskPipelineJob,
		Params: map[string]any{
			"pipeline_task": taskType, // which cfg.Pipelines entry runPipelineJob uses
			"job_id":        jobID,
			"job_path":      jobPath,
			"out_root":      outRoot,
			"tier":          in.Tier,
		},
	}
	return req, cleanup, nil
}

// pipelineJobSpecID extracts and validates job_spec.id.
func pipelineJobSpecID(jobSpec map[string]json.RawMessage) (string, error) {
	raw, ok := jobSpec["id"]
	if !ok || isAbsentJSON(raw) {
		return "", fmt.Errorf("job_spec.id required")
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("job_spec.id must be a string: %v", err)
	}
	if !pipelineJobIDPattern.MatchString(id) {
		return "", fmt.Errorf("job_spec.id %q must match %s", id, pipelineJobIDPattern.String())
	}
	return id, nil
}

// pipelineBackgroundMode reads job_spec.background (absent = ("", false,
// nil)) and reports its declared mode plus whether it ALREADY carries a
// "path" key (which must be rejected — the node, not the payload, injects it).
func pipelineBackgroundMode(jobSpec map[string]json.RawMessage) (mode string, hasPath bool, err error) {
	raw, ok := jobSpec["background"]
	if !ok || isAbsentJSON(raw) {
		return "", false, nil
	}
	var bg map[string]json.RawMessage
	if uerr := json.Unmarshal(raw, &bg); uerr != nil {
		return "", false, fmt.Errorf("job_spec.background must be a JSON object: %v", uerr)
	}
	if _, has := bg["path"]; has {
		return "", true, nil
	}
	if modeRaw, has := bg["mode"]; has {
		if merr := json.Unmarshal(modeRaw, &mode); merr != nil {
			return "", false, fmt.Errorf("job_spec.background.mode must be a string: %v", merr)
		}
	}
	return mode, false, nil
}

// pipelineInjectRefs returns job.json's bytes: jobSpec with product/logo (and
// background.path, when bgMode=="stock") set to the FETCHED absolute local
// paths. buildPipelineJob has already rejected a payload that tried to set
// these itself, so this never overwrites an operator-supplied value.
func pipelineInjectRefs(jobSpec map[string]json.RawMessage, fetched map[string]string, bgMode string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(jobSpec)+2)
	for k, v := range jobSpec {
		out[k] = v
	}
	productRaw, err := json.Marshal(fetched["product"])
	if err != nil {
		return nil, err
	}
	out["product"] = productRaw
	logoRaw, err := json.Marshal(fetched["logo"])
	if err != nil {
		return nil, err
	}
	out["logo"] = logoRaw
	if bgMode == "stock" {
		var bg map[string]json.RawMessage
		if raw, has := jobSpec["background"]; has {
			_ = json.Unmarshal(raw, &bg) // already validated as an object by pipelineBackgroundMode
		}
		if bg == nil {
			bg = map[string]json.RawMessage{}
		}
		pathRaw, perr := json.Marshal(fetched["background"])
		if perr != nil {
			return nil, perr
		}
		bg["path"] = pathRaw
		bgBytes, berr := json.Marshal(bg)
		if berr != nil {
			return nil, berr
		}
		out["background"] = bgBytes
	}
	return json.Marshal(out)
}

// SweepOrphanedPipelineJobs removes every entry directly under
// cfg.BaseDir()/pipeline-jobs/ (each one a materialized job dir from
// buildPipelineJob) and reports how many were removed. Missing pipeline-jobs/
// (nothing configured yet, or a fresh install) is not an error — (0, nil).
//
// Called ONCE at fleet-serve startup, BEFORE listening (main.go's
// runFleetServe). Jobs are in-memory (internal/fleetnode/jobs.go's Jobs
// store) — an ungraceful stop (crash, kill -9, power loss) loses every
// in-flight job's state, but its materialized directory on disk survives.
// Left in place, that orphaned directory would permanently refuse every
// FUTURE dispatch that reuses its job_spec.id: buildPipelineJob's exclusive
// os.Mkdir collision guard has no way to distinguish "a real still-running
// job" from "a directory nobody is ever going to finish or clean up" — so at
// process start, before any job has been accepted, EVERY directory present is
// orphaned by definition and safe to remove.
//
// A per-entry removal failure (e.g. a locked file) is collected but does not
// abort the sweep of the REST of the entries — one bad directory blocking one
// job_spec.id forever is a much smaller failure than a startup crash over it.
func SweepOrphanedPipelineJobs(cfg config.Config) (swept int, err error) {
	dir := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep pipeline-jobs: reading %s: %w", dir, rerr)
	}
	var firstErr error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if rmErr := os.RemoveAll(p); rmErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sweep pipeline-jobs: removing %s: %w", p, rmErr)
			}
			continue
		}
		swept++
	}
	return swept, firstErr
}
