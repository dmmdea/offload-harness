package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dmmdea/local-offload/internal/config"
	"github.com/dmmdea/local-offload/internal/core"
	"github.com/dmmdea/local-offload/internal/llamaclient"
)

// NewRecordlessPipeline builds a fresh pipeline with nil cache + nil ledger, so
// nothing it runs (RunTier / any offload call) can write the savings ledger, cache,
// shadow store, or exemplars — and the real ledger is never even opened. This is the
// SINGLE place the nil-store invariant is constructed; NewRecordlessOffload and the
// agent-trajectory flywheel (agent-trajectory-label) both use it so it can't drift.
func NewRecordlessPipeline(cfg config.Config, timeout time.Duration) *Pipeline {
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, timeout)
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
		res, ok := ap.RunTier(ctx, core.Request{Task: core.TaskType(task), Input: input, Params: params}, model)
		if !ok || res.Deferred {
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
