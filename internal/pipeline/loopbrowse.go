package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// NewLoopBrowse is the agent loop's browse lane (ADR 0060): the `browse` tool's
// BrowseFunc on every agent door. nil when this box has not configured the lane, so
// a door that passes it straight into BuildConfig grants nothing (Build refuses a
// browse grant without a lane and says so in its notes).
//
// It runs on a RECORDLESS pipeline for the same reason the in-loop offload tools do:
// an agent's internal calls are the harness talking to itself, not work a caller
// delegated, so they never enter the savings ledger. The browse slot is process-wide,
// so a tool call and an MCP offload_browse still take turns at the one browser. The
// tool gets the lane's whole result — including a defer's reason and partial — as
// JSON, because a denied or blocked run is information the model must act on.
func NewLoopBrowse(cfg config.Config, door string) agent.BrowseFunc {
	if !cfg.BrowseConfigured() {
		return nil
	}
	p := NewRecordlessPipeline(cfg, 2*time.Minute) // the text-generation client's per-call budget
	return func(ctx context.Context, in agent.BrowseInput) (string, error) {
		params := map[string]any{"url": in.URL, "goal": in.Goal, "unattended": in.Unattended}
		if len(in.AllowHosts) > 0 {
			params["allow_hosts"] = in.AllowHosts
		}
		if in.MaxActions > 0 {
			params["max_actions"] = in.MaxActions
		}
		res := p.Run(ctx, core.Request{Task: core.TaskBrowse, Door: door, Params: params})
		b, err := json.Marshal(res)
		return string(b), err
	}
}

// LoopBrowseTimeout is the `browse` tool's per-call cap: the lane's own timeout plus
// a minute for the sidecar's start and shutdown, so the lane — not the loop's
// generic tool timeout — is what ends a slow run with a typed defer.
func LoopBrowseTimeout(cfg config.Config) time.Duration {
	sec := cfg.BrowseTimeoutSec
	if sec <= 0 {
		sec = 300
	}
	return time.Duration(sec)*time.Second + time.Minute
}
