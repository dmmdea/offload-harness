// offload_agent_delegate and offload_agent_rig: batch delegation, deadlines and options.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/rig"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// callDeadlineAt is the instant a delegation door's call must be over: entered
// plus the configured whole-call deadline (ADR 0065), or the zero time when the
// deadline is switched off. The MCP client aborts a call at its own limit and
// drops the response with it, so the door answers first — with what has finished.
// entered is taken at handler entry because the client's clock starts when it
// sends the request, not when the delegator begins placing work.
func (s *Server) callDeadlineAt(entered time.Time) time.Time {
	if d := s.p.Cfg().CallDeadline(); d > 0 {
		return entered.Add(d)
	}
	return time.Time{}
}

// handleAgentDelegate is the MCP front door onto delegate.Run (Task 6). It
// prepares contracts (delegator-mints version/depth, inlines context_paths
// under read_root, validates — all BEFORE any placement or network), then
// hands them to the shared engine. House style throughout: every failure path
// is a deferred-shape result, never an MCP error (see handleAgentRun).
//
// The call has a deadline below the client's abort (ADR 0065, config
// agent_call_deadline_sec): at it the finished subtasks' results are returned and
// each unfinished one is a budget defer "call deadline reached; N unfinished".
func (s *Server) handleAgentDelegate(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	entered := time.Now()
	var in struct {
		Subtasks []struct {
			Goal         string                  `json:"goal"`
			Context      []core.ContextDoc       `json:"context"`
			ContextPaths []string                `json:"context_paths"`
			OutputSchema json.RawMessage         `json:"output_schema"`
			Acceptance   []string                `json:"acceptance"`
			Profile      string                  `json:"profile"`
			MaxSteps     int                     `json:"max_steps"`
			TimeoutSec   int                     `json:"timeout_sec"`
			SetupActions []core.AgentSetupAction `json:"setup_actions"`
			Thinking     string                  `json:"thinking"`
			WriteRoot    string                  `json:"write_root"`
			ContextClass string                  `json:"context_class"`
			Layer        string                  `json:"layer"`
			AllowBrowse  bool                    `json:"allow_browse"`
			BrowseHosts  []string                `json:"browse_hosts"`
		} `json:"subtasks"`
		Route     string   `json:"route"`
		PinReason string   `json:"pin_reason"`
		ReadRoot  string   `json:"read_root"`
		Remotes   []string `json:"remotes"`
		Priority  int      `json:"priority"`
		LintOnly  bool     `json:"lint_only"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	if len(in.Subtasks) == 0 {
		return jsonResult(map[string]any{"deferred": true, "reason": "at least one subtask required"})
	}
	// pin_reason is checked before anything is read or prepared (ADR 0078): a value outside the closed set, one the
	// route does not take, and one given with a route that is not a pin all die here, naming the valid set. The
	// engine checks again (delegate.RunWith); this is the cheap refusal, before context_paths are read. An empty
	// route is auto, this door's default.
	if err := delegate.CheckPinReason(in.Route, in.PinReason); err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	// Browse (ADR 0060) drives THIS box's operator browser: a contract asking for it
	// is admitted only on route "local" and only where this node opted in. auto /
	// spread / remote / queue could place it on another node's browser.
	for i, st := range in.Subtasks {
		if !st.AllowBrowse {
			continue
		}
		if refusal := browseDoorRefusal(s.p.Cfg(), in.Route, false, st.BrowseHosts); refusal != "" { // agent_delegate: "" = auto
			return jsonResult(map[string]any{"deferred": true, "defer_class": core.DeferClassConfig,
				"reason": fmt.Sprintf("subtask %d: %s", i, refusal)})
		}
	}
	// Remotes are vetted HERE too (delegate.Run re-checks): a caller mistake
	// should die naming the URL before any contract prep work is spent. The list is a MODEL's
	// naming of nodes, so beyond its shape it may only NARROW the configured fleet: a node that is
	// not in delegate_remotes is refused, never dialled (delegate.CheckRosterRemotes, the operator's
	// standing rule about the standalone box), and a box that configures no roster accepts no list.
	for _, base := range in.Remotes {
		if err := netguard.TailnetURL(base); err != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": "remote " + base + ": " + err.Error()})
		}
	}
	if err := delegate.CheckRosterRemotes(s.p.Cfg(), in.Remotes); err != nil {
		return jsonResult(map[string]any{"deferred": true, "defer_class": core.DeferClassConfig, "reason": err.Error()})
	}
	// read_root defaulting mirrors handleAgentRun: the server working dir.
	readRoot := in.ReadRoot
	if readRoot == "" {
		wd, werr := os.Getwd()
		if werr != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": "cannot determine working dir for read_root: " + werr.Error()})
		}
		readRoot = wd
	}
	absRoot, err := filepath.Abs(readRoot)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": "bad read_root: " + err.Error()})
	}
	// The subtask cap is read off what the caller SENT, before anything is prepared: nine subtasks
	// with one bad one are refused for their number exactly as nine good ones are, and a rejection
	// never buys a call a place under the cap (register D-111).
	if err := delegate.CheckSubtaskCount(len(in.Subtasks)); err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	specs := make([]delegate.SubtaskSpec, 0, len(in.Subtasks))
	for _, st := range in.Subtasks {
		specs = append(specs, delegate.SubtaskSpec{
			AgentContract: core.AgentContract{
				Goal:         st.Goal,
				Context:      st.Context,
				OutputSchema: st.OutputSchema,
				Acceptance:   st.Acceptance,
				Profile:      st.Profile,
				MaxSteps:     st.MaxSteps,
				TimeoutSec:   st.TimeoutSec,
				SetupActions: st.SetupActions,
				Thinking:     st.Thinking,
				WriteRoot:    st.WriteRoot,
				ContextClass: st.ContextClass,
				Layer:        st.Layer,
				AllowBrowse:  st.AllowBrowse,
				BrowseHosts:  st.BrowseHosts,
				Door:         "agent_delegate",
			},
			ContextPaths: st.ContextPaths,
		})
	}
	// PrepareBatchWithCap, not PrepareContract: each subtask is prepared on its own (register D-111),
	// so one bad subtask no longer rejects the valid ones, and a composite box raises the per-contract
	// context ceiling to its largest window x 3 bytes (config.AgentContextCapBytes). At chars/3 a
	// 256 KiB contract estimates ~87k tokens, so without the cap the box's own 262k seat is
	// unreachable through its own front door - the long seats would be dead code. A plain box passes
	// the same 256 KiB it always did.
	batch := delegate.PrepareBatchWithCap(specs, absRoot, s.p.Cfg().AgentContextCapBytes())
	if batch.AllRejected() {
		// Nothing valid: nothing to run, and the call keeps its flat defer. It names every bad subtask.
		return jsonResult(map[string]any{"deferred": true, "reason": batch.FlatReason("subtask", nil)})
	}
	contracts := batch.Contracts
	// Linted on the PREPARED contract (context_paths already inlined - grounding is judged against
	// everything the sub-agent will see), and kept by INPUT position: a rejected subtask has none, and
	// the valid ones' lints must stay on their own results once the rejections are spliced back in.
	lints := make([][]string, len(in.Subtasks))
	for k, c := range contracts {
		lints[batch.Indices[k]] = delegate.LintAcceptance(c)
	}
	if in.LintOnly {
		return s.lintOnlyResult(ctx, len(in.Subtasks), batch, lints, in.Route, in.Remotes, in.PinReason)
	}
	localRun := s.localAgent
	if localRun == nil {
		localRun = s.p.RunAgentContract
	}
	// The server-lifetime quarantine rides every delegation, not only research:
	// a node proven to answer about the wrong document must not keep receiving
	// agent_delegate work (silent-failure review, 2026-09-02).
	deadline := s.callDeadlineAt(entered)
	// Progress notifications, only for a request that supplied a progress token.
	// The reporter speaks in the caller's numbers (register D-111): the subtasks sent, each at its input position,
	// the refused ones done from the start. The engine only knows the valid subset.
	onProgress, stopProgress := s.startBatchProgress(ctx, req, batch, deadline)
	defer stopProgress()
	results, sum, rerr := delegate.RunWith(ctx, s.p.Cfg(), localRun, contracts, in.Route, in.Remotes,
		s.agentDelegateOptions(in.Priority, in.PinReason, deadline, onProgress))
	if rerr != nil {
		// Nothing ran, so there is no result to splice a rejection into: the whole-call defer names the engine's
		// complaint and also the subtasks intake refused, so fixing the first does not meet the second on the retry.
		return jsonResult(map[string]any{"deferred": true, "reason": batch.AlsoRefused(rerr.Error(), "subtask", nil)})
	}
	// The subtasks intake refused go back among the results at their own positions: one result per
	// subtask sent, in the order sent. Each is a contract-class defer (the caller's mistake, not a
	// broken stack), so summary.infrastructure and lost_to_stack do not move, and the call is no more
	// an error than any other result with a deferred subtask in it.
	results, sum = batch.Splice(results, sum, "subtask", nil)
	// WireResponse is a struct, so "summary" marshals FIRST (roast delta 14) —
	// jsonResult over a map would alphabetize results ahead of it.
	staleNote, guardState := s.staleBinaryFields(buildinfo.Version)
	res, jerr := jsonResult(delegateWire{ResponseWire: delegate.WireResponse(results, sum, lints), StaleBinary: staleNote, StaleBinaryGuard: guardState})
	if jerr != nil || res == nil {
		return res, jerr
	}
	// The loud-exit contract used to live ONLY on the CLI (main.go's
	// delegateExitErr): every one of these came back to the MCP caller — this
	// lane's primary consumer — as a plain successful tool call, so a fleet with
	// a dead llama-swap read like a clean run to the delegating model. The flag
	// carries the same meaning — a human has to look — for the cases where the
	// call itself failed (delegateIsError); a PARTIAL result is not one of them.
	//
	// House style stays intact in the important half: the BODY is unchanged, so
	// the summary and every per-subtask reason/defer_class are still there to
	// read. Ordinary defers (abstention, budget — a call deadline is one) and
	// failed verification remain successes — those are RESULT shapes, exactly as
	// on the CLI.
	if delegateIsError(sum) {
		res.IsError = true
	}
	return res, nil
}

// lintOnlyPlacementBudget bounds the read-only placement look of a lint_only call: the local seat's in-flight GET and
// the fleet's health GETs run inside it, so a down node costs this long at most and the call still answers.
const lintOnlyPlacementBudget = 4 * time.Second

// lintOnlyResult answers an agent_delegate call that set lint_only (register D-109), after intake and the lint and
// before anything is dispatched: per subtask its acceptance_lint findings and the placement the joint deal would give
// it right now (delegate.PlanPlacement, read-only). It takes no lease, mints no job, opens no queue and warms no seat,
// and it runs no model. A subtask intake refused is answered at its own position with the refusal, as in a real call.
func (s *Server) lintOnlyResult(ctx context.Context, sent int, batch delegate.PreparedBatch, lints [][]string, route string, remotes []string, pinReason string) (*mcp.CallToolResult, error) {
	pctx, cancel := context.WithTimeout(ctx, lintOnlyPlacementBudget)
	defer cancel()
	plans := delegate.PlanPlacement(pctx, s.p.Cfg(), batch.Contracts, route, remotes, &delegate.RunOptions{
		Quarantine: s.quarantine, Tenant: s.tenant, RosterOnly: true, PinReason: pinReason, PinNeedsReason: true,
	})
	rows := make([]map[string]any, sent)
	for k, i := range batch.Indices {
		lint := lints[i]
		if lint == nil {
			lint = []string{}
		}
		rows[i] = map[string]any{"index": i, "acceptance_lint": lint, "placement": plans[k]}
	}
	for _, rej := range batch.Rejections {
		rows[rej.Index] = map[string]any{"index": rej.Index, "deferred": true, "defer_class": core.DeferClassContract,
			"reason": fmt.Sprintf("subtask %d: %v", rej.Index, rej.Err)}
	}
	return jsonResult(map[string]any{
		"lint_only":  true,
		"dispatched": false,
		"note": "lint_only: nothing ran - no job, no lease, no queue, no seat warm-up, no model. placement is the pick the " +
			"deal would make from the readings taken now; a lease taken or a node that refuses before a real call can still change it",
		"subtasks": rows,
	})
}

// agentDelegateOptions is the engine's options for one agent_delegate call. RosterOnly is part of
// the door's contract: the call's remotes list was named by a MODEL, so the engine, like the door's
// own early check, lets it only narrow the configured fleet (delegate.CheckRosterRemotes, the
// standing rule about the standalone box). PinNeedsReason is the other half of the door's contract (ADR 0078):
// this door offers pin_reason, so a local or remote route without one is a hint placement may override. Built
// here, not inline, so the guards have one place to be read and one to be pinned (roster_remotes_test.go).
func (s *Server) agentDelegateOptions(priority int, pinReason string, deadline time.Time, onProgress func(delegate.ProgressEvent)) *delegate.RunOptions {
	return &delegate.RunOptions{
		Quarantine: s.quarantine, Priority: priority, Tenant: s.tenant, Deadline: deadline,
		OnProgress: onProgress, Rescue: s.rescueFunc(), RosterOnly: true,
		PinReason: pinReason, PinNeedsReason: true, PinTally: s.pins,
	}
}

// delegateIsError decides the tool-call error flag for one delegation run. It is
// NOT the CLI's exit rule, and the difference is the point.
//
// IsError:true means, in MCP, THE CALL FAILED — most models react by discarding
// or redoing the work. Summary.Infrastructure alone cannot carry that: it counts
// a SUCCESSFUL local placement taken while the fleet was down (delegate's
// remotesUnreachable), so a run whose every subtask completed, validated, and
// passed acceptance came back flagged as a failure. The fleet-down verdict is
// still fully published — it is in the body's summary and per-subtask reasons,
// and `local-offload delegate` still exits non-zero on it, because an exit code
// can sit BESIDE printed results in a way a boolean cannot.
//
// The rule that expresses that WITHOUT a silent path is stated on lost WORK, not
// on the presence of Infrastructure. Infrastructure covers both remotesUnreachable
// (a result that succeeded) and a broken-stack DEFER (a subtask whose contracted
// output never arrived), and only the first justifies staying quiet.
// LostToStack counts exactly the subtasks that DELIVERED NO USABLE RESULT because
// the stack failed them, so the rule needs no proxy; `Deferred > 0 &&
// Infrastructure > 0` is NOT one — a contract-classed defer beside a fleet-down
// local success satisfies it with nothing lost, re-creating the
// flag-on-finished-work defect. The count is stated on the CONTRACTED output, not
// on empty bytes: a finished agent loop whose structured re-pack seat was
// unreachable publishes its prose with `structured` absent, and is lost — a
// contract carrying an output_schema is owed a mechanically checked deliverable.
//
// What that lost work does to the FLAG changed twice. R5-2 flagged the call
// whenever any subtask failed or was lost, so one of two subtasks eaten by a dead
// llama-server could not read as a clean call. C-75 (register, 2026-09) narrowed
// it again, because the same argument cuts against the flag: IsError means THE
// CALL FAILED, and a call that delivered digests for seven of eight subtasks did
// not fail. The MCP client answers an error-flagged body by keeping only its head
// and tail, so every partial research reply the workers saw lost the middle — the
// digests that succeeded. So the flag is kept for the two cases it is true of:
//
//   - nothing succeeded, and something failed or was lost to the stack (the call
//     delivered nothing, and the fix is on a box or in the contract);
//   - work was skipped outright (a batched run's later chunks never ran) and
//     nothing succeeded.
//
// A PARTIAL result is a successful call whose body says what is missing: the
// summary counts (failed, lost_to_stack, infrastructure, skipped) and each
// subtask's own `failed` / `defer_class` / `reason`. The CLI keeps its wider
// exit-code rule — an exit code sits BESIDE the printed results — and a call
// deadline's budget defers are result shapes, never a reason for the flag.
func delegateIsError(sum delegate.Summary) bool {
	if sum.Succeeded > 0 {
		return false
	}
	// Skipped: subtasks a batched run never attempted because an earlier chunk
	// errored — lost work the prose `error` field alone must not hide when there
	// is no delivered result beside it.
	return sum.Failed > 0 || sum.LostToStack > 0 || sum.Skipped > 0
}

// addEffects folds a run's effect ledger into an agent_run response — counts
// when any tools ran, plus the full records for every NON-committed call. Used
// by BOTH the success and deferred paths so they cannot drift: the one record a
// caller must never miss is "unknown" — a tool abandoned mid-flight whose
// effects may exist, which changes whether the run is safe to blindly retry.
// handleAgentRig runs the rigger's classifier over this box's corpus (rig
// package). Every failure is a clean defer: an unknown seat names the seats
// seen, a bad window names the flag.
func (s *Server) handleAgentRig(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Seat     string `json:"seat"`
		Since    string `json:"since"`
		Node     string `json:"node"`
		Markdown bool   `json:"markdown"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	if strings.TrimSpace(in.Seat) == "" {
		return jsonResult(map[string]any{"deferred": true, "reason": "seat is required (the alias as the corpus names it)"})
	}
	until := time.Now()
	since, err := rig.ParseSince(in.Since, until)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	dir := filepath.Join(s.p.Cfg().BaseDir(), "delegation-log")
	rows, skipped, err := rig.ReadShards(dir, since, until)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	rep, err := rig.Build(rows, in.Seat, in.Node, since, until)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error(), "seats_seen": rep.SeatsSeen})
	}
	out := map[string]any{"report": rep, "corpus_dir": dir}
	if skipped > 0 {
		out["skipped_lines"] = skipped
	}
	if in.Markdown {
		out["markdown"] = rig.Markdown(rep)
	}
	return jsonResult(out)
}

func addEffects(out map[string]any, effects []agent.EffectRecord) {
	counts := agent.EffectCounts(effects)
	if counts == nil {
		return
	}
	out["effects"] = counts
	// The step trace (ADR 0036): what the corpus keeps per call, here for
	// the caller — the same projection the fleet wire result carries.
	out["trace"] = pipeline.TraceFromEffects(effects)
	var flagged []agent.EffectRecord
	for _, r := range effects {
		if r.Status != agent.EffectCommitted {
			flagged = append(flagged, r)
		}
	}
	if len(flagged) > 0 {
		out["effects_flagged"] = flagged
	}
}

// agentTimeout resolves an agent run's wall-clock budget: an explicit per-call
// timeout wins; else the tier-seeded config default. A tier that binds a big
// planner seat seeds agent_timeout_sec with it: a cold big-model load + low
// tok/s inside the old 180s default is a timeout machine (roast finding,
// 2026-08-02). 180s stays the floor for configs that seed nothing.
func agentTimeout(reqSec int, cfg config.Config) time.Duration {
	if reqSec > 0 {
		return time.Duration(reqSec) * time.Second
	}
	if cfg.AgentTimeoutSec > 0 {
		return time.Duration(cfg.AgentTimeoutSec) * time.Second
	}
	return 180 * time.Second
}

// plannerUnserved checks the endpoint's /v1/models roster for the resolved
// planner seat. checked=false means the roster was unreachable/unparseable —
// callers proceed and let the loop's first chat call surface the transport
// error; only a POSITIVE "roster answered and the model is absent" fails loud.
// llama-swap's /v1/models lists CANONICAL ids in data[].id; a harness-bound ALIAS
// (the very names tier seeds put in agent_model) appears only in each entry's
// meta.llamaswap.aliases — matching id alone would fail-loud on a correctly
// served seat. swapclient.Roster.Serves matches both.
func plannerUnserved(ctx context.Context, base, model string) (missing, checked bool) {
	if strings.TrimSpace(base) == "" || model == "" {
		return false, false
	}
	roster, err := swapclient.FetchRoster(ctx, base, 10*time.Second)
	if err != nil || roster.Len() == 0 {
		return false, false
	}
	return !roster.Serves(model), true
}
