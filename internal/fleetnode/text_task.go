// The fleet "text" lane (0.154.0): one classify or extract call runs on THIS node's
// own pipeline for a caller that asked for a remote outright (or whose own card is
// leased). The node runs the call through its OWN cascade, so its seat capability
// (an unconstrained seat takes the prompt-carried shape and strict validation, not a
// grammar), its timeouts and its input cap all live here, where they are true, and
// the caller reads back the node's full core.Result, defers included.
//
// The lane ships DARK. A node advertises it only when its tier's media seat declares
// text tasks (config text_tasks, written by mediaseat.Bindings), and no shipped tier
// declares any until measured data passes (>= 30 cases per lane through this pipeline,
// >= 90 % correct, zero off-schema outputs accepted). Only classify and extract are
// ever admitted: the runtime behind an unconstrained seat returns no logprobs, so the
// decision-margin gate that backs summarize and triage cannot run, and blind quality
// on the reference seat was 0/4 for each. summarize and triage are refused at ack time
// whatever text_tasks says.
//
// It is the vision lane's shape (vision_task.go) with a small body: a route of its own
// only so the caller's payload is typed, the same token rule (tokenGated), the same
// admission path (Server.admit), results through the job store.

package fleetnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// TextTask is the lane's fleet task_type: what health lists in supported_task_types
// and what the job feed shows as `task`.
const TextTask = "text"

// TextBodyCap is the request-body ceiling of POST /fleet/text: dispatch's own 1 MiB.
// The payload is one text plus a label list or a schema, so it never needs more, and
// the node's max_input_chars trims the text before it reaches the model.
const TextBodyCap = 1 << 20

// TextPayload is the wire body of POST /fleet/text. job_id is the caller-minted id
// exactly as on /fleet/dispatch; task is classify or extract; params carries what the
// task takes (classify: labels, at least two; extract: schema), nothing else.
type TextPayload struct {
	JobID  string         `json:"job_id"`
	Task   string         `json:"task"`
	Input  string         `json:"input"`
	Params map[string]any `json:"params,omitempty"`
}

// TextLaneAdmissible is THE predicate behind the text lane, on both sides of the wire
// (health advertisement AND ack-time admission, the vision lane's discipline): the
// node's tier declares at least one text task, and the listener is safely reachable
// (loopback, or a fleet_auth_token for anything beyond it).
func TextLaneAdmissible(cfg config.Config, loopbackListener bool) bool {
	return len(cfg.TextTasks) > 0 && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// textTaskOf maps the payload's task name to the pipeline task. summarize and triage
// are named in their refusal because the caller can reasonably try them: they are
// never served on this lane (see the file comment).
func textTaskOf(name string) (core.TaskType, error) {
	switch t := core.TaskType(strings.TrimSpace(name)); t {
	case core.TaskClassify, core.TaskExtract:
		return t, nil
	case core.TaskSummarize, core.TaskTriage:
		return "", fmt.Errorf("text: task %q is never served on the text lane (no logprobs behind an unconstrained seat, so no uncertainty gate): only classify and extract", t)
	}
	return "", fmt.Errorf("text: task %q is not one of classify, extract", name)
}

// textTaskServed reports whether this node's seat is declared to serve task. The
// match is exact on purpose: the same strings travel in health, so the delegator's
// placement and this refusal can never disagree about a spelling.
func textTaskServed(cfg config.Config, task core.TaskType) bool {
	for _, t := range cfg.TextTasks {
		if t == string(task) {
			return true
		}
	}
	return false
}

// buildText translates a text payload into the core.Request the node's pipeline runs,
// mirroring the MCP handlers' param mapping exactly (handleClassify / handleExtract).
// Every refusal is a 400 at ack time (a task outside this node's text_tasks included):
// the caller can get each of them wrong. The pipeline's own defers (the model's reply
// failed validation, low confidence, input too small) come back inside the job result
// as the full core.Result.
func buildText(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var p TextPayload
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return core.Request{}, noop, fmt.Errorf("text: payload: %w", err)
	}
	task, err := textTaskOf(p.Task)
	if err != nil {
		return core.Request{}, noop, err
	}
	if !textTaskServed(cfg, task) {
		return core.Request{}, noop, fmt.Errorf("text: task %q is not served by this node's text lane (text_tasks: %s)",
			task, strings.Join(cfg.TextTasks, ", "))
	}
	if strings.TrimSpace(p.Input) == "" {
		return core.Request{}, noop, fmt.Errorf("text: input is required")
	}
	req := core.Request{Task: task, Input: p.Input}
	switch task {
	case core.TaskClassify:
		labels, err := textLabels(p.Params)
		if err != nil {
			return core.Request{}, noop, err
		}
		req.Params = map[string]any{"labels": labels}
	case core.TaskExtract:
		schema, err := textSchema(p.Params)
		if err != nil {
			return core.Request{}, noop, err
		}
		req.Params = map[string]any{"schema": schema}
	}
	return req, noop, nil
}

func textLabels(params map[string]any) ([]string, error) {
	for k := range params {
		if k != "labels" {
			return nil, fmt.Errorf("text: classify takes only params.labels (got %q)", k)
		}
	}
	raw, ok := params["labels"].([]any)
	if !ok {
		return nil, fmt.Errorf("text: classify requires params.labels, an array of at least 2 strings")
	}
	labels := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("text: params.labels must be non-empty strings")
		}
		labels = append(labels, s)
	}
	if len(labels) < 2 {
		return nil, fmt.Errorf("text: classify requires at least 2 labels")
	}
	return labels, nil
}

func textSchema(params map[string]any) (map[string]any, error) {
	for k := range params {
		if k != "schema" {
			return nil, fmt.Errorf("text: extract takes only params.schema (got %q)", k)
		}
	}
	schema, ok := params["schema"].(map[string]any)
	if !ok || len(schema) == 0 {
		return nil, fmt.Errorf("text: extract requires params.schema, a JSON schema object with properties")
	}
	return schema, nil
}

// textJobData is what a text job stores as its result: the FULL core.Result, defers
// included, exactly as the vision lane does (visionJobData), so a caller's local and
// remote results stay byte-comparable and a defer is a "done" job whose data says so.
func textJobData(res core.Result) (json.RawMessage, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("text: encoding result: %w", err)
	}
	return b, nil
}
