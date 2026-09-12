// The fleet "vision" lane (0.116.0): one single-image vision task — vqa, ocr
// or assess_image — runs on THIS node's vision seat for a caller whose own
// card is busy (or who asked for a remote outright). The image travels INSIDE
// the request as a data URI, so the node never sees a path that is not its
// own, and the pipeline's own loader (imageio.LoadImageB64) enforces this
// node's vision_max_image_bytes exactly as it does for a local call.
//
// Why a route of its own (POST /fleet/vision) rather than a task_type on
// /fleet/dispatch: dispatch caps its body at 1 MiB — right for every media
// envelope and every agent contract, and a fifth of one image at the
// configured cap. The vision route sizes its body from vision_max_image_bytes
// (base64 inflation + envelope slack) and then joins the SAME admission path
// (Server.admit) as every other job: known-id re-ack, drain, lease, band,
// queue cap, and the job store's concurrency cap (the lane touches the shared
// llama-swap endpoint, so it stays CAPPED — see concurrencyCapped).
//
// Auth is the agent lane's rule, verbatim (tokenGated): fleet_auth_token
// required beyond loopback, loopback + no token stays open. A node that
// cannot admit the lane never advertises it (VisionLaneAdmissible feeds both
// the advertisement and the ack-time gate, the AgentLaneAdmissible pattern).

package fleetnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// VisionTask is the lane's fleet task_type — what health lists in
// supported_task_types and what the job feed shows as `task`.
const VisionTask = "vision"

// VisionPayload is the wire body of POST /fleet/vision. job_id is the
// caller-minted id exactly as on /fleet/dispatch (re-acks and polls key on
// it); task is the pipeline task name; image is a data:image/...;base64 URI.
type VisionPayload struct {
	JobID    string `json:"job_id"`
	Task     string `json:"task"`
	Image    string `json:"image"`
	Question string `json:"question,omitempty"`
	Brief    string `json:"brief,omitempty"`
}

// visionBodySlack is the envelope room on top of the base64-inflated image
// cap: the question/brief text and the JSON framing.
const visionBodySlack = 64 << 10

// VisionBodyCap is the request-body ceiling of POST /fleet/vision for cfg:
// the node's vision_max_image_bytes inflated by base64 (4/3) plus slack. A
// config that never set the cap gets the default's 6 MB, the same number the
// pipeline loader applies to the decoded bytes.
func VisionBodyCap(cfg config.Config) int64 {
	max := cfg.VisionMaxImageBytes
	if max <= 0 {
		max = config.Default().VisionMaxImageBytes
	}
	return int64(max)*4/3 + visionBodySlack
}

// VisionLaneAdmissible is THE predicate behind the vision lane, on both sides
// of the wire (health advertisement AND ack-time admission, the same
// discipline as AgentLaneAdmissible): a bound vision_model, and safe
// reachability — a loopback listener or a fleet_auth_token for anything
// beyond it.
func VisionLaneAdmissible(cfg config.Config, loopbackListener bool) bool {
	return cfg.VisionModel != "" && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// tokenGated reports whether a task_type rides the bearer rule: the agent lane
// (v1 scope) and, since 0.116.0, the vision lane. Every media task stays
// tokenless so deployed media clients keep working byte-identically.
func tokenGated(taskType string) bool {
	return taskType == string(core.TaskAgentRun) || taskType == VisionTask
}

// visionTaskOf maps the payload's task name to the pipeline task, refusing
// anything that is not one of the three single-image vision tasks.
func visionTaskOf(name string) (core.TaskType, bool) {
	switch core.TaskType(strings.TrimSpace(name)) {
	case core.TaskVQA:
		return core.TaskVQA, true
	case core.TaskOCR:
		return core.TaskOCR, true
	case core.TaskAssessImage:
		return core.TaskAssessImage, true
	}
	return "", false
}

// buildVision translates a vision payload into the core.Request the pipeline
// runs, mirroring the MCP handlers' param mapping exactly (handleVQA /
// handleOCR / handleAssessImage): vqa always carries `question`, assess_image
// carries `brief` only when non-empty, ocr carries nothing. Every refusal is a
// 400 at ack time — the caller can get each of them wrong — while the
// pipeline's own defers (unreadable image, gpu busy, empty answer) come back
// inside the job result as the full core.Result.
func buildVision(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var p VisionPayload
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return core.Request{}, noop, fmt.Errorf("vision: payload: %w", err)
	}
	task, ok := visionTaskOf(p.Task)
	if !ok {
		return core.Request{}, noop, fmt.Errorf("vision: task %q is not one of vqa, ocr, assess_image", p.Task)
	}
	if !strings.HasPrefix(strings.ToLower(p.Image), "data:image/") {
		return core.Request{}, noop, fmt.Errorf("vision: image must be a data:image/...;base64 URI (the bytes travel with the job; a path on the caller's disk is unreadable here)")
	}
	// Decoded-size check at ack time, so an oversize image is a 400 the caller
	// reads immediately rather than a defer inside a job it has to poll for.
	// base64 length × 3/4 bounds the decoded size from above; the pipeline's
	// loader re-checks the exact decoded bytes against the same cap.
	comma := strings.IndexByte(p.Image, ',')
	if comma < 0 {
		return core.Request{}, noop, fmt.Errorf("vision: image data URI has no payload")
	}
	max := cfg.VisionMaxImageBytes
	if max <= 0 {
		max = config.Default().VisionMaxImageBytes
	}
	if est := (len(p.Image) - comma - 1) * 3 / 4; est > max+3 {
		return core.Request{}, noop, fmt.Errorf("vision: image is about %d bytes, cap %d (vision_max_image_bytes on this node)", est, max)
	}
	req := core.Request{Task: task, Image: p.Image}
	switch task {
	case core.TaskVQA:
		if strings.TrimSpace(p.Question) == "" {
			return core.Request{}, noop, fmt.Errorf("vision: vqa requires question")
		}
		req.Params = map[string]any{"question": p.Question}
	case core.TaskAssessImage:
		req.Params = map[string]any{}
		if p.Brief != "" {
			req.Params["brief"] = p.Brief
		}
	}
	return req, noop, nil
}

// visionJobData is what a vision job stores as its result: the FULL
// core.Result, defers included, so the caller reads `deferred`, `reason`,
// `defer_class` and `meta` (model, latency, err_class) exactly as a local
// call returns them. The media lanes map a defer to job state "error" with
// only the reason string; the vision caller needs the whole shape to keep
// its local and remote results byte-comparable, so a defer here is a "done"
// job whose data says deferred — the agent lane's convention.
func visionJobData(res core.Result) (json.RawMessage, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("vision: encoding result: %w", err)
	}
	return b, nil
}
