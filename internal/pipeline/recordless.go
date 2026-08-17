package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// NewRecordlessPipeline builds a fresh pipeline with nil cache + nil ledger, so
// nothing it runs (RunTier / any offload call) can write the savings ledger, cache,
// shadow store, or exemplars — and the real ledger is never even opened. This is the
// SINGLE place the nil-store invariant is constructed; NewRecordlessOffload and the
// agent-trajectory flywheel (agent-trajectory-label) both use it so it can't drift.
func NewRecordlessPipeline(cfg config.Config, timeout time.Duration) *Pipeline {
	// WithSeatEndpoints mirrors openPipeline's construction: the recordless
	// pipeline must route an overridden seat to the same remote base the
	// recorded one does — a per-model endpoint is a property of the seat, not
	// of which pipeline shape happens to call it. Absent key = no-op.
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, timeout).
		WithSeatEndpoints(cfg.SeatEndpoints)
	return New(cfg, oc, nil, nil)
}

// NewRecordlessOffload builds the in-process offload closure for the local
// Agent-loop: a FRESH pipeline with nil cache + nil ledger, calling RunTier
// (record=false), so the agent's offload_* calls cannot write the savings ledger,
// cache, shadow store, or exemplars. This is the SINGLE place that record=false /
// nil-store invariant is constructed, so every drive mode (CLI, MCP front door,
// standalone) shares it and the ledger-pristine guarantee cannot drift.
//
// model is the planner/cascade entry model id. On any non-result (defer / tier
// miss) it returns a defer JSON the loop can react to — never a fatal error.
func NewRecordlessOffload(cfg config.Config, model string, timeout time.Duration) func(ctx context.Context, task, input string, params map[string]any) (string, error) {
	ap := NewRecordlessPipeline(cfg, timeout)
	return func(ctx context.Context, task, input string, params map[string]any) (string, error) {
		req := offloadRequest(task, input, params)

		var res core.Result
		if textOnlyTask(req.Task) {
			r, ok := ap.RunTier(ctx, req, model)
			if !ok {
				r.Deferred = true
			}
			res = r
		} else {
			res = ap.Run(ctx, req)
		}

		if res.Deferred || !res.OK {
			reason := res.Reason
			if reason == "" {
				reason = "offload could not run — check inputs (classify needs >=2 labels; extract needs a schema)"
			}
			b, _ := json.Marshal(map[string]any{"deferred": true, "reason": reason})
			return string(b), nil // a defer is a valid tool result the agent can react to
		}
		return string(res.Data), nil
	}
}

// textOnlyTask reports whether a task may take the single-tier RunTier path.
//
// THIS DEFAULTS TO FALSE ON PURPOSE. RunTier does tasks.Build + a TEXT generate and
// never enters Pipeline.Run's media dispatch — and tasks.Build SUCCEEDS for vqa, ocr,
// assess_image and video_describe. So routing one of those through RunTier does not
// error: it returns a confident answer about an image the model never saw. Handing a
// hallucination to an autonomous planner is worse than any error, so anything not
// explicitly listed here goes through Run, which fails closed (no vision model
// configured / image load failed).
//
// The four listed tasks stay on RunTier deliberately: it runs ONE named tier, so an
// agent's offload call cannot trigger a cascade that swaps the planner's own model off
// a shared GPU mid-run.
func textOnlyTask(t core.TaskType) bool {
	switch t {
	case core.TaskSummarize, core.TaskClassify, core.TaskTriage, core.TaskExtract:
		return true
	}
	return false
}

// offloadRequest builds the core.Request, lifting file-bearing params onto the
// FIELDS the pipeline actually reads.
//
// core.Request.Image/.Video/.Audio were never assigned by this closure, which is why
// the agent loop could not see, listen or draw even though the same binary does all
// three. They are lifted out of params rather than added to the closure signature so
// internal/agent keeps its zero-import-of-pipeline invariant; the keys are declared
// explicitly in each tool's JSON Schema, so this is a published contract, not a magic
// convention.
func offloadRequest(task, input string, params map[string]any) core.Request {
	req := core.Request{Task: core.TaskType(task), Input: input, Params: params}
	req.Image = paramPath(params, "image")
	req.Video = paramPath(params, "video")
	req.Audio = paramPath(params, "audio")
	return req
}

func paramPath(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	s, _ := params[key].(string)
	return strings.TrimSpace(s)
}
