package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// NewRecordlessPipeline builds a fresh pipeline with nil cache + nil ledger, so
// nothing it runs (RunTier / any offload call) can write the savings ledger, cache,
// shadow store, or exemplars — and the real ledger is never even opened. This is the
// SINGLE place the nil-store invariant is constructed; NewRecordlessOffload and the
// agent-trajectory flywheel (agent-trajectory-label) both use it so it can't drift.
func NewRecordlessPipeline(cfg config.Config, timeout time.Duration) *Pipeline {
	// WithSeatEndpoints + WithRemoteLanes mirror openPipeline's construction:
	// the recordless pipeline must route an overridden seat — and fail a busy
	// hour over to the same cascade lane — exactly as the recorded one does; a
	// per-model endpoint is a property of the seat, not of which pipeline shape
	// happens to call it. Absent keys = no-ops, byte-identical client.
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, timeout).
		WithSeatEndpoints(cfg.SeatEndpoints)
	if len(cfg.CascadeRemoteLanes) > 0 {
		gpuLockPath, stateDir := cfg.GPULockPath, cfg.StateDir
		oc = oc.WithRemoteLanes(cfg.CascadeRemoteLanes,
			func() bool { return delegate.LocalBusy(gpuLockPath, stateDir) },
			llamaclient.RosterResident())
	}
	return New(cfg, oc, nil, nil)
}

// NewInLoopPipeline builds the agent loop's offload pipeline: nil LEDGER, but a
// SHARED result cache.
//
// # T2-D — why the old single invariant was two invariants wearing one coat
//
// "Recordless" conflated two unrelated guarantees under one nil-store rule:
//
//	(a) the agent's internal offload calls must not pollute the SAVINGS ledger,
//	    because those are the harness talking to itself, not work a caller
//	    delegated — counting them would inflate every savings number; and
//	(b) those same calls must not read or write the result cache.
//
// (a) is a real accounting invariant and is kept exactly. (b) was collateral: it
// makes the loop re-run the model on byte-identical input, so an agent that
// summarizes the same file twice in one run pays twice. Nothing about ledger
// hygiene requires that. The cache is content-addressed and its key now binds the
// prompt template and exemplar set, so an entry written here is valid for any
// caller and vice versa.
//
// ca may be nil (the caller has no cache, or it is held by another process), in
// which case this degrades to exactly NewRecordlessPipeline's behaviour.
//
// # Pass the handle, never re-open
//
// ca is an ALREADY-OPEN handle rather than a path on purpose. bbolt takes an
// exclusive file lock, so a second Open inside the same process would block for
// its full timeout and then fail — the in-loop pipeline would lose a lock race
// against the very process that owns it, once per construction.
func NewInLoopPipeline(cfg config.Config, timeout time.Duration, ca *cache.Cache) *Pipeline {
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, timeout)
	p := New(cfg, oc, ca, nil)
	// Opt IN to per-tier caching. RunTier is shared with the shadow-labelling
	// flywheel, which calls it on the MAIN pipeline (cache open) to evaluate
	// counterfactual tiers; serving those from cache — or writing them — would
	// corrupt the measurement the flywheel exists to produce. So the behaviour is
	// a property of THIS pipeline, not of RunTier.
	p.tierCache = true
	return p
}

// NewRecordlessOffload builds the in-process offload closure for the local
// Agent-loop with NO cache. Kept for callers that must not share cache state —
// notably prompt A/B runs, where a shared cache would let one arm serve the
// other's answers.
//
// model is the planner/cascade entry model id. On any non-result (defer / tier
// miss) it returns a defer JSON the loop can react to — never a fatal error.
func NewRecordlessOffload(cfg config.Config, model string, timeout time.Duration) func(ctx context.Context, task, input string, params map[string]any) (string, error) {
	return offloadClosure(NewRecordlessPipeline(cfg, timeout), model)
}

// NewInLoopOffload is NewRecordlessOffload plus the shared result cache (T2-D).
// This is what the ordinary drive modes (MCP front door, CLI agent) should use:
// the ledger stays pristine, and the loop stops paying twice for byte-identical
// work. Pass ca == nil to get the cache-free behaviour.
func NewInLoopOffload(cfg config.Config, model string, timeout time.Duration, ca *cache.Cache) func(ctx context.Context, task, input string, params map[string]any) (string, error) {
	return offloadClosure(NewInLoopPipeline(cfg, timeout, ca), model)
}

// offloadClosure is the shared body of both constructors, so the defer-shaping
// and media-dispatch semantics cannot drift between the cached and uncached
// variants.
func offloadClosure(ap *Pipeline, model string) func(ctx context.Context, task, input string, params map[string]any) (string, error) {
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
