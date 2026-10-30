// Agent tools: browse, NIM and offload_agent_run with its admission, sizing and result-shaping helpers.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/tokclient"
)

// browseDoorRefusal is the ONE admission rule both agent doors apply to a browse
// grant (ADR 0060): route "local" (agent_run's "" is local; agent_delegate's "" is
// auto, so there the caller must say local — emptyIsLocal), this node's
// agent_allow_browse opt-in, a configured lane, and a non-empty host list. "" means
// admitted; anything else is the reason to defer with.
func browseDoorRefusal(cfg config.Config, route string, emptyIsLocal bool, hosts []string) string {
	r := strings.TrimSpace(route)
	if r == "" && !emptyIsLocal {
		// agent_delegate reads an omitted route as "auto" (delegate.RunWith), which
		// may place the contract on another node's browser: name local explicitly.
		return `allow_browse requires route "local" (an omitted route means auto here, which may place the run on another node's browser)`
	}
	switch {
	case r != "" && r != "local":
		return fmt.Sprintf("allow_browse requires route \"local\" (got %q): the browse tool drives this machine's own browser", r)
	case !cfg.AgentAllowBrowse:
		return "this node does not open the browse door (agent_allow_browse is false)"
	case !cfg.BrowseConfigured():
		return "the browse lane is not configured on this machine (browse_python, browse_script and a loopback browse_decision_url; ADR 0060)"
	case len(hosts) == 0:
		return "allow_browse needs a non-empty browse_hosts list on an agent door"
	}
	return ""
}

// doorAuditFor is the broker audit trail this agent door hands Build (register
// SF-02): with audit_all_doors off (the default) a run without browse builds
// exactly as before; warn attaches an advisory trail, enforce an enforcing one, and
// a browse run keeps the enforcing trail its grant requires in every mode.
func doorAuditFor(cfg config.Config, allowBrowse bool) agent.DoorAuditPlan {
	return agent.DoorAudit(cfg.AuditAllDoorsMode(), allowBrowse, cfg.BaseDir())
}

// handleBrowse — offload_browse (ADR 0060). The pipeline validates everything before
// the sidecar starts; this door only refuses a route the lane cannot honour (there is
// one operator browser, on this machine) and never forwards `unattended`: the attended
// MCP door is the one place allow_labels may lift the deny-list.
func (s *Server) handleBrowse(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		URL         string   `json:"url"`
		Goal        string   `json:"goal"`
		MaxActions  int      `json:"max_actions"`
		AllowHosts  []string `json:"allow_hosts"`
		AllowLabels []string `json:"allow_labels"`
		Capture     []string `json:"capture"`
		Route       string   `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	if in.Route != "" && in.Route != "local" {
		return jsonResult(map[string]any{"deferred": true,
			"reason": fmt.Sprintf("browse: BAD_INPUT: route %q — the lane drives this machine's own browser, so route must be local", in.Route)})
	}
	params := map[string]any{"url": in.URL, "goal": in.Goal}
	if in.MaxActions != 0 {
		params["max_actions"] = in.MaxActions
	}
	if len(in.AllowHosts) > 0 {
		params["allow_hosts"] = in.AllowHosts
	}
	if len(in.AllowLabels) > 0 {
		params["allow_labels"] = in.AllowLabels
	}
	if len(in.Capture) > 0 {
		params["capture"] = in.Capture
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskBrowse, Door: "offload_browse", Params: params}))
}

func (s *Server) handleNIM(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt      string  `json:"prompt"`
		Model       string  `json:"model"`
		System      string  `json:"system"`
		Base        string  `json:"base"`
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		ListModels  bool    `json:"list_models"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// defer-not-crash: defend against an empty prompt even though the schema marks it required.
	if !in.ListModels && in.Prompt == "" {
		return jsonResult(map[string]any{"deferred": true, "reason": "empty prompt"})
	}
	cfg := s.p.Cfg()
	base := in.Base
	if base == "" {
		base = cfg.NIMEndpoint
	}
	// The base allowlist (security standard L5, register S-30): a caller-named
	// base outside NVIDIA's hosted API, nim_endpoint and nim_bases is refused
	// under nim_base_policy "enforce"; under "audit" (the default) the call runs,
	// the result says it would have been refused, and a would-refuse row is
	// appended for the promotion decision (ADR 0067).
	basePolicy := ""
	if in.Base != "" {
		if ok, why := nimclient.BaseAllowed(in.Base, cfg.NIMEndpoint, cfg.NIMBases); !ok {
			mode := "audit"
			if cfg.NIMBaseEnforced() {
				mode = "enforce"
			}
			// The row goes to the machine-wide state root the GPU lease uses:
			// state_dir, else LOCAL_OFFLOAD_STATE_DIR, else the platform default.
			// Reading cfg.StateDir alone dropped every row on hosts that leave
			// state_dir unset (SF-05).
			var auditErr error
			if root, rerr := gpulease.ResolveStateRoot(cfg.StateDir); rerr != nil {
				auditErr = rerr
			} else {
				auditErr = appendNIMBaseAudit(root, in.Base, why, mode)
			}
			if mode == "enforce" {
				return jsonResult(map[string]any{"deferred": true, "defer_class": string(core.DeferClassConfig),
					"reason": "offload_nim base refused (nim_base_policy enforce): " + why + "; add it to nim_bases if it is a NIM you run"})
			}
			basePolicy = "audit: this base would be refused under nim_base_policy enforce (" + why + ")"
			if auditErr != nil {
				basePolicy += "; the would-refuse row was not written: " + auditErr.Error()
			}
		}
	}
	key := nimclient.KeyForBase(base) // env key only for NVIDIA hosts; never transmitted to a non-NVIDIA base
	// defer-not-crash: a missing key on the hosted endpoint is a clean defer, not an error.
	if key == "" && nimclient.IsHostedNVIDIA(base) {
		return jsonResult(map[string]any{"deferred": true, "reason": "NVIDIA_API_KEY (or NGC_API_KEY) not set in the MCP server env — required for the hosted NIM endpoint; a self-hosted NIM via base is keyless"})
	}
	timeout := time.Duration(cfg.NIMTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	client := nimclient.New(base, key, timeout)
	if in.ListModels {
		ids, err := client.ListModels(ctx)
		if err != nil {
			return jsonResult(withBasePolicy(map[string]any{"deferred": true, "reason": err.Error()}, basePolicy))
		}
		return jsonResult(withBasePolicy(map[string]any{"models": ids, "count": len(ids), "endpoint": base}, basePolicy))
	}
	model := in.Model
	if model == "" {
		model = cfg.NIMModel
	}
	maxTok := in.MaxTokens
	if maxTok == 0 {
		maxTok = cfg.NIMMaxTokens
	}
	res, err := client.Chat(ctx, model, in.System, in.Prompt, maxTok, in.Temperature)
	if err != nil {
		return jsonResult(withBasePolicy(map[string]any{"deferred": true, "reason": err.Error()}, basePolicy))
	}
	return jsonResult(withBasePolicy(map[string]any{
		"model":             res.Model,
		"content":           res.Content,
		"reasoning_content": res.ReasoningContent,
		"tokens_in":         res.TokensIn,
		"tokens_out":        res.TokensOut,
		"truncated":         res.Truncated,
	}, basePolicy))
}

// withBasePolicy adds the audit-mode note (S-30) to an offload_nim result when
// the base was outside the allowlist.
func withBasePolicy(out map[string]any, note string) map[string]any {
	if note != "" {
		out["base_policy"] = note
	}
	return out
}

// appendNIMBaseAudit appends one would-refuse (or refuse) row to
// <stateDir>/nim-base-audit.jsonl: the counted data a promotion from audit to
// enforce needs (ADR 0067). Only the base's scheme, host and port are kept —
// never the prompt, never a path or query that could carry data.
func appendNIMBaseAudit(stateDir, base, why, mode string) error {
	if strings.TrimSpace(stateDir) == "" {
		return fmt.Errorf("no state_dir")
	}
	host := base
	if u, err := url.Parse(strings.TrimSpace(base)); err == nil && u.Host != "" {
		host = u.Scheme + "://" + u.Host
	}
	row, err := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "mode": mode, "base": host, "why": why})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "nim-base-audit.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(row, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// withAdmission stamps an agent_run result with what was spent BEFORE the wall
// — the cordon wait, the llama-swap swap pre-flight, the cold-load warm-up, the
// coherence probe and the served-window probe, summed exactly as the delegation
// door sums its `admitted` — under that wire's names (core.AgentWireResult
// admission_wait_sec / admission_note): a run that waited or loaded its seat
// first must say so, or its wall time and its window read as if the seat had
// been warm and free.
func withAdmission(out map[string]any, admitted time.Duration, note string) {
	if admitted > 0 {
		out["admission_wait_sec"] = admitted.Seconds()
	}
	if note != "" {
		out["admission_note"] = note
	}
}

// withSizing stamps an agent_run result with the D-03 wall sizing under the
// delegation wire's own field names (core.AgentWireResult wall_estimate_sec /
// min_turn_sec / wall_note): what the run was estimated to need on this seat,
// the least a one-turn retry is worth, and the arithmetic — or why there is no
// number. A caller who then hits the wall reads the cause here instead of
// guessing (register D-102). No rate = no numbers, only the note.
func withSizing(out map[string]any, est seatrate.Estimate) {
	if est.TotalSec > 0 {
		out["wall_estimate_sec"] = est.TotalSec
	}
	if est.MinTurnSec > 0 {
		out["min_turn_sec"] = est.MinTurnSec
	}
	if est.Note != "" {
		out["wall_note"] = est.Note
	}
}

// withFinalFit stamps an agent_run result with the final-budget fit (register D-134, the
// D-95 arithmetic) under the delegation wire's own field names (core.AgentWireResult
// final_budget_fit / budget_note): the budget the forced final actually opened at once the
// wall that was LEFT was taken into account, and the arithmetic behind it. What the loop ran
// at supersedes the fit predicted when the wall started, as on the node; a run that never
// reached its forced final (it answered early, or the wall ended it first) carries the
// prediction. Both are published only when the fit narrowed the configured budget (a note
// exists): a run that was not narrowed, a wall with room and a seat with no rate of its own
// carry neither, and the wall refusal, which never starts a run, is not stamped at all.
func withFinalFit(out map[string]any, start seatrate.FinalFitResult, res agent.Result) {
	budget, note := start.Budget, start.Note
	if res.FinalBudgetFit > 0 {
		budget, note = res.FinalBudgetFit, res.BudgetNote
	}
	if note == "" || budget <= 0 {
		return
	}
	out["final_budget_fit"] = budget
	out["budget_note"] = note
}

// withTruncation tells the caller of a local agent_run that its final answer is a partial: the loop ended it on the
// completion budget (finish_reason "length", or a repetition loop trimmed off its tail), marked it Result.OutputTruncated and
// still ended "done", because a cut answer is the caller's to use. The door published it as a clean success, and the final-answer
// fit (register D-134) makes that end likelier on purpose, since it opens the final narrower than the configured budget.
// The flag and the loop's own stop note use the delegation wire's names; the sentence names the budget the final was cut at
// when the loop's fit narrowed it, and says where its arithmetic is. A complete answer carries none of the three.
func withTruncation(out map[string]any, res agent.Result) {
	if !res.OutputTruncated {
		return
	}
	out["output_truncated"] = true
	if res.StopNote != "" {
		out["stop_note"] = res.StopNote
	}
	note := "the final answer was cut at its completion budget, so output is partial"
	if res.FinalBudgetFit > 0 {
		// The loop's own narrowing, not the fit predicted when the wall started (withFinalFit may publish either): only the former
		// is the budget this final was cut at.
		note += fmt.Sprintf(": the forced final opened at %d tokens to fit the wall that was left (budget_note has the arithmetic)", res.FinalBudgetFit)
	}
	out["truncation_note"] = note
}

// wallRefusalReason is the agent_run door's refusal of a wall that cannot hold
// even the smallest answer on the seat (register D-102): the wall it was given
// and where that number came from, the least wall one answer needs, what the
// whole run is estimated to need, and the argument to change. The arithmetic
// itself rides in wall_note beside it.
func wallRefusalReason(cfg config.Config, explicit bool, wallSec int, model string, maxSteps int, sz pipeline.RunSizing) string {
	source := "timeout_sec"
	if !explicit {
		source = "the box default: agent_timeout_sec is unset, so the built-in 180 s"
		if cfg.AgentTimeoutSec > 0 {
			source = "the box default agent_timeout_sec"
		}
	}
	est := sz.Estimate
	// The wall to ask for is the whole run's estimate, never less than the floor
	// itself (a very fast seat on a one-step run can estimate under it).
	ask := est.TotalSec
	if sz.MinViableSec > ask {
		ask = sz.MinViableSec
	}
	return fmt.Sprintf("wall %d s (%s) cannot hold even the smallest answer on %s: one tool step and a %d-token final need %d s at %.1f tok/s, and a %d-step run is estimated at %d s (min_turn %d s) — the run was not started; pass timeout_sec of at least %d s, arithmetic in wall_note",
		wallSec, source, model, seatrate.MinimalFinalTokens, sz.MinViableSec, est.TokS, maxSteps, est.TotalSec, est.MinTurnSec, ask)
}

// joinAdmissionNotes concatenates the admission steps' notes the way the
// delegation door does — "; " between them, empties dropped — so one
// admission_note can carry what the pre-flight AND the warm-up each found.
func joinAdmissionNotes(notes ...string) string {
	var kept []string
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, "; ")
}

// withCoherence stamps the post-warm seat coherence probe (register D-118)
// under the wire's own field name, beside admission_note. Empty = the probe did
// not run (policy "off", a warm seat under the default "cold" policy, or no
// admission budget left) and the key is absent, exactly as on the delegation
// wire.
func withCoherence(out map[string]any, note string) {
	if note != "" {
		out["coherence_note"] = note
	}
}

// firstNonEmptyString returns the first non-blank of its arguments ("" when none).
func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleAgentRun(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Goal       string `json:"goal"`
		ReadRoot   string `json:"read_root"`
		MaxSteps   int    `json:"max_steps"`
		Model      string `json:"model"`
		TimeoutSec int    `json:"timeout_sec"`
		Profile    string `json:"profile"`
		Judge      bool   `json:"judge"`
		// Thinking (0.115.8): planner think-block policy for this run; empty =
		// the box's agent_thinking, else auto. Validated by name at the build.
		Thinking string `json:"thinking"`
		// SetupActions (ADR 0036 P2): tool calls replayed before the first
		// model turn, validated by name here — the same rule as a contract.
		SetupActions []core.AgentSetupAction `json:"setup_actions"`
		// ContextClass (ADR 0052): "" or "long" — ask the placement table for
		// the box's biggest long-context layer. Inert on a box with no layers.
		ContextClass string `json:"context_class"`
		// Route (register C-46): "" / local = this box; anything else goes
		// through the delegator as one contract.
		Route string `json:"route"`
		// AllowBrowse / BrowseHosts (ADR 0060): grant the `browse` tool on this
		// box's own browser. Local route only, the node's agent_allow_browse
		// opt-in, a configured lane and a non-empty host list are all required.
		AllowBrowse bool     `json:"allow_browse"`
		BrowseHosts []string `json:"browse_hosts"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// defer-not-crash: an empty goal is a clean defer, not an error.
	if strings.TrimSpace(in.Goal) == "" {
		return jsonResult(map[string]any{"deferred": true, "reason": "empty goal"})
	}
	if err := core.ValidateAgentSetupActions(in.SetupActions); err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	if in.AllowBrowse {
		if refusal := browseDoorRefusal(s.p.Cfg(), in.Route, true, in.BrowseHosts); refusal != "" { // agent_run: "" = this box
			return jsonResult(map[string]any{"deferred": true, "reason": refusal, "defer_class": core.DeferClassConfig})
		}
	} else if len(in.BrowseHosts) > 0 {
		return jsonResult(map[string]any{"deferred": true, "reason": "browse_hosts is set but allow_browse is not"})
	}
	// The call's own arguments as the fleet path takes them (namedseat.go). The model is the one the caller named: the
	// resolved planner below can be this box's default, which is not a name the fleet is asked for.
	call := agentRunFleetCall{Goal: in.Goal, Model: in.Model, ReadRoot: in.ReadRoot, MaxSteps: in.MaxSteps, TimeoutSec: in.TimeoutSec,
		Profile: in.Profile, Thinking: in.Thinking, ContextClass: in.ContextClass, SetupActions: in.SetupActions, Judge: in.Judge, AllowBrowse: in.AllowBrowse}
	route := strings.TrimSpace(in.Route)
	if route != "" && route != "local" {
		// register C-46 (S-27): agent_run always ran local, so a remote seat could
		// not be named from this door. With a route the goal becomes one contract
		// through the delegator's single-contract path: read_root and judge do
		// not travel (the executing node reads its own root), so this is for
		// self-contained goals and setup_actions. A model rides route=remote only
		// (register D-125): it selects the nodes whose agent seat has that name.
		return s.agentRunOnRoute(ctx, s.p.Cfg(), call, route)
	}
	cfg := s.p.Cfg()
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
	// Whether the read root exists as the call comes in (register C-79). A root the run's
	// tools later cannot open was either taken under the run, which is the box's fault, or
	// never there: a typo, which agent.Build lets through (it only makes the path absolute)
	// and which fails the same way at the first tool call. Only this stat can tell the two
	// apart, and only the first is infrastructure.
	rootExisted := false
	if fi, serr := os.Stat(absRoot); serr == nil && fi.IsDir() {
		rootExisted = true
	}
	// The planner follows agent_model (per-call > seat > workhorse;
	// config.AgentPlannerModel). The IN-LOOP offload tools follow the planner
	// too (0.115.18, register D-88): they used to stay on the workhorse "for
	// its economics", but the workhorse shares the planner's llama-swap and
	// loading it EVICTS the planner mid-run — measured 2026-09-10 on <node-b>:
	// three offload_triage calls cost four 3-minute reloads of the 27B seat
	// and the whole 900 s wall. pipeline.InLoopOffloadModel keeps the
	// workhorse only when it IS the planner (a single-model box).
	model, placed, refusal := placeAgentRun(cfg, in.Model, in.Goal, in.ContextClass, in.TimeoutSec)
	if refusal != nil {
		return jsonResult(refusal)
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	if maxSteps > 64 {
		maxSteps = 64 // a self-standing ceiling so the step budget doesn't rely solely on the timeout
	}
	timeout := agentTimeout(in.TimeoutSec, cfg)
	// THE WALL GATE (register D-102), BEFORE anything touches the seat. This
	// door runs its loop under a context deadline, so the wall is a hard kill
	// here (the delegation door's walls are expectations since ADR 0055), and
	// until now the door computed no sizing at all: a caller who named a wall the
	// seat could not hold learned it from `context deadline exceeded` after ten
	// steps (2026-09-15, a 420 s wall against an estimate of 928 s). The run is
	// sized from the seat's own rate with the delegation door's arithmetic
	// (pipeline.SizeRun reads one local file and dials nothing); a wall that
	// cannot hold even one tool step and a minimal final is refused with the
	// numbers, so a refusal costs no cordon wait, no load and no chat request.
	//
	// The floor is the INV-5 rider's minimum viable final (ADR 0050), the same
	// one the delegator applies to a remote placement — NOT the published
	// min_turn_sec, which is a cold load plus the configured max final and which
	// the rider forbids refusing on (a 900 s wall on a 7 tok/s 27B completes
	// its contracts and sits under it). Every wall above the floor runs, and the
	// result carries wall_estimate_sec / min_turn_sec / wall_note so a run that
	// then hits its wall says why. No rate = no floor = no refusal.
	wallSec := int(timeout / time.Second)
	sizing := s.p.SizeRun(core.AgentContract{MaxSteps: maxSteps, Thinking: in.Thinking}, model, wallSec)
	// deferSized is how EVERY deferral past this point leaves the door: it stamps the
	// run's sizing (wall_estimate_sec / min_turn_sec / wall_note) before the result is
	// built, so a capacity defer a caller re-places, a seat that fails the probe and a
	// run that died at its wall all say what the wall was weighed against, and the
	// published promise ("every result that reached the gate") has one place that keeps
	// it. A deferral that calls jsonResult itself drops the numbers silently;
	// TestEveryAgentRunDeferralPastTheWallGateGoesThroughTheSizingStamp fails on that.
	deferSized := func(dout map[string]any) (*mcp.CallToolResult, error) {
		withSizing(dout, sizing.Estimate)
		return jsonResult(dout)
	}
	if sizing.Refuses(wallSec) {
		dout := map[string]any{
			"deferred":    true,
			"defer_class": core.DeferClassBudget,
			"reason":      wallRefusalReason(cfg, in.TimeoutSec > 0, wallSec, model, maxSteps, sizing),
			"steps":       0,
		}
		withPlaced(dout, placed)
		return deferSized(dout)
	}
	// Fail LOUD when the resolved planner is not in the served roster — a seat
	// that silently fell back to the workhorse would ship the exact silent
	// downgrade this seat exists to cure. Roster unreachable = proceed (the
	// loop's first chat call surfaces the real transport error).
	if missing, checked := plannerUnserved(ctx, cfg.Endpoint, model); checked && missing {
		// The advice must match where the model CAME from: telling a caller whose
		// explicit model is unserved to "pass model explicitly" is circular.
		reason := fmt.Sprintf("agent planner model %q is not in the endpoint's served roster — fix agent_model/config or pass model explicitly", model)
		if in.Model != "" {
			reason = explicitModelRefusal(model)
			// A name this box does not serve may be the agent seat of a fleet node (register D-125): the fleet is asked, and
			// the run goes there; a name nobody runs keeps this sentence and says where the fleet looked. An explicit
			// route=local never asks (namedseat.go).
			named := call
			named.Model = model
			res, refusal, ferr := s.namedSeatOffBox(ctx, cfg, named, route, reason)
			if refusal == nil {
				// A run that went to the fleet is built in namedseat.go and is deliberately NOT stamped with this box's sizing
				// (it was weighed against another seat, and a routed call carries none, as the tool description says); that
				// is why this return is not deferSized, and why TestEveryAgentRunDeferralPastTheWallGateGoesThroughTheSizingStamp
				// still counts two jsonResult calls here. The refusal below is stamped as it always was.
				return res, ferr
			}
			return deferSized(refusal)
		}
		return deferSized(map[string]any{"deferred": true, "reason": reason})
	}
	// In-process offload (nil LEDGER; shared result cache) + the SHARED loop
	// builder — identical construction to the CLI and the standalone runner, so
	// the three drive modes stay at parity. Read-only front door: no
	// write/fetch/shell, no audit (the offload cannot write the ledger anyway).
	//
	// T2-D: the cache handle is the server's own already-open one. This process
	// holds the bbolt lock, so re-opening by path here would lose a lock race
	// against itself and silently fall back to no cache on every agent_run.
	offload := pipeline.NewInLoopOffloadForPlanner(cfg, model, timeout, s.p.Cache())
	audit := doorAuditFor(cfg, in.AllowBrowse)
	if audit.Refuse != "" {
		return deferSized(map[string]any{"deferred": true, "defer_class": string(core.DeferClassConfig), "reason": audit.Refuse})
	}
	if audit.Note != "" {
		log.Printf("agent_run: %s", audit.Note)
	}
	built, err := agent.Build(agent.BuildConfig{
		PlannerBase:   cfg.Endpoint,
		Model:         model,
		Timeout:       timeout,
		MaxSteps:      maxSteps,
		MaxTokens:     cfg.AgentMaxTokens,                                  // 0 => the loop default (1,024); a thinking seat wants 4096 (agent_max_tokens)
		Thinking:      firstNonEmptyString(in.Thinking, cfg.AgentThinking), // the call's own policy, else this box's agent_thinking
		Sampling:      cfg.AgentSampling,                                   // this seat's measured decoding policy (D-95b)
		SamplingFinal: cfg.AgentSamplingFinal,
		ReadRoot:      absRoot,
		Offload:       offload,
		NPU:           pipeline.NewLoopNPU(cfg),
		Accel:         pipeline.NewLoopAccel(cfg),
		EnvRules:      cfg.AgentEnvRules,
		SetupActions:  in.SetupActions,
		// Browse (ADR 0060): an agent door, so judged unattended; the audit trail
		// is required for the grant and is set only when browse is asked for.
		AllowBrowse:     in.AllowBrowse,
		BrowseHosts:     in.BrowseHosts,
		Browse:          pipeline.NewLoopBrowse(cfg, "agent_run"),
		BrowseTimeout:   pipeline.LoopBrowseTimeout(cfg),
		BrowseAgentDoor: true,
		AuditPath:       audit.Path,
		AuditAdvisory:   audit.Advisory,
		ReadFloor:       cfg.AgentReadFloor,
		AuditChain:      cfg.AuditChain,
		EvidenceReducer: cfg.AgentEvidenceReducer,

		// SF-02: a denial the enforcing trail causes names audit_all_doors.
		AuditEnforcedByKey: audit.Enforced,
	})
	if err != nil {
		return deferSized(map[string]any{"deferred": true, "reason": "building agent: " + err.Error()})
	}
	if in.AllowBrowse && !built.BrowseGranted {
		return deferSized(map[string]any{"deferred": true, "defer_class": core.DeferClassConfig,
			"reason": "browse was asked for but not granted: " + strings.Join(built.Notes, "; ")})
	}
	defer built.EndAudit() // close this run's chain on the trail (SF-08, audit_chain)
	// Register the run and hold at the cordon (0.117.0, register D-93): a
	// draining or exclusive text hold, or a media lease, admits no NEW run;
	// running work completes. The wait runs on the caller's ctx with its own
	// deadline (the admission budget) and the wall below starts only after it
	// — waiting on the wall's context charged the cordon to the run (reviewer
	// finding, 0.117.0), the exact defect class D-64 removed from the other door.
	seatPins, _ := modelaffinity.PinsFor(model) // the run records its seat's cards (plan P5)
	act := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: model, Kind: "agent_run", Origin: agentRunOrigin(), Goal: in.Goal, MaxSteps: maxSteps, Phase: gpuactivity.PhaseAdmission, Devices: seatPins})
	defer act.End()
	// THE FENCE CHECK (register S-26), BEFORE the cordon below — the same read
	// the review lane has made since 0.125.0 (D-110) and the delegation door now
	// makes too. Under a lease this process does not hold and that refuses new
	// runs, the cordon cannot succeed: nothing this call can do inside its budget
	// releases another process's lease. Both agent doors polled that file for the
	// whole admission budget anyway — 47 rows at 300 s each in three days — and
	// then deferred `capacity`, which is the class a caller re-places on. The
	// verdict is on disk before the dial, so it is given now.
	//
	// An INHERITED lease is not a fence (`gpu reserve … -- <session>` sets
	// GPU_LEASE_EPOCH and its own work belongs on the cards it cleared), and a
	// plain non-fencing reservation still WAITS at the cordon: ADR 0032's "a
	// peer-held seat is waited for" governs every hold whose answer can change.
	//
	// The lease read is the CORDON's own armed directory (config.Load arms it),
	// not an independent resolution from this config: a pre-check that predicts
	// what AwaitRunSlot will do has to read what AwaitRunSlot reads, or the two
	// can disagree — and a process that never loaded a config has that gate
	// deliberately inert, which this check must be too.
	fence := s.foreignFence
	if fence == nil {
		fence = delegate.ForeignFence
	}
	if dir := modelaffinity.GPULeaseDir(); dir != "" {
		// Narrowed to the cards this seat is pinned to (plan P4), the same read AwaitRunSlot
		// makes below: a render on another card is not a fence on this seat.
		lease := modelaffinity.ScopeToModel(modelaffinity.InspectLease(dir), model)
		if fenced, why := fence(lease); fenced {
			dout := map[string]any{
				"deferred":    true,
				"defer_class": string(core.DeferClassCapacity),
				"reason": fmt.Sprintf("gpu busy: %s; no new run is admitted on this box until it is released (%s)",
					why, delegate.HolderLine(lease)),
				"steps": 0,
			}
			withPlaced(dout, placed)
			return deferSized(dout)
		}
	}
	admitStart := time.Now()
	admitDeadline := admitStart.Add(pipeline.AdmissionBudget(cfg.AgentAdmissionWaitSec))
	// cordonWait is the time this run actually spent at the cordon: an ungated
	// pass is nanoseconds, and the delegation door's own cordonWait applies the
	// same floor rather than stamping a wait that never happened.
	cordonWait := func() time.Duration {
		if w := time.Since(admitStart); w >= time.Millisecond {
			return w
		}
		return 0
	}
	if lerr := modelaffinity.AwaitRunSlot(ctx, cfg.Endpoint, model, admitDeadline); lerr != nil {
		// This is the pre-check's RACE WINDOW — a fencing lease taken between
		// its read and this one (the two share BlocksNewRun, so nothing else
		// reaches here). Whatever else it is, it is a run that spent its whole
		// admission budget, and it has to say so: a caller reading a bare
		// "gpu busy" cannot tell a 300 s wait from an instant refusal, and the
		// delegation door has stamped admission on this exact path since 0.117.0.
		dout := map[string]any{
			"deferred":    true,
			"defer_class": string(core.DeferClassCapacity),
			"reason":      "gpu busy: " + lerr.Error(),
			"steps":       0,
		}
		withAdmission(dout, cordonWait(), "held at the cordon for the admission budget")
		withPlaced(dout, placed)
		return deferSized(dout)
	}
	// The LOCAL run cap (register C-42): the fleet caps the jobs it sends
	// here, nothing capped the runs this door starts — sixteen could land on
	// one seat and spend their walls in the engine's queue. Wait for a slot
	// among the registered runs on the seat (this run's own record excluded,
	// FIFO), for as long as the run's own wall — the same rule as the
	// delegation door (register C-60) — and move the admission deadline out by
	// the time spent in line; a slot that never frees is a capacity defer,
	// re-placeable, never a refusal.
	if reg, rerr := gpuactivity.Open(cfg.GPULockPath, cfg.StateDir); rerr == nil {
		capStart := time.Now()
		capEnd := modelaffinity.SeatCapDeadline(ctx, capStart, timeout, admitDeadline)
		seatRuns := func(now time.Time, names ...string) []gpuactivity.Run {
			return reg.OnSeatPinned(now, seatPins, names...)
		}
		if serr := modelaffinity.AwaitSeatSlot(ctx, seatRuns, model, "", act.ID(), cfg.FleetConcurrencyLimit(), capEnd); serr != nil {
			dout := map[string]any{
				"deferred":    true,
				"defer_class": string(core.DeferClassCapacity),
				"reason":      "seat busy: " + serr.Error(),
				"steps":       0,
			}
			withAdmission(dout, cordonWait(), "held at the seat cap for the run's wall")
			withPlaced(dout, placed)
			return deferSized(dout)
		}
		admitDeadline = admitDeadline.Add(time.Since(capStart))
	}
	cordon := cordonWait()
	// ONE admission total for this door, exactly as the delegation door keeps
	// one: the cordon, the pre-flight, the warm-up and the coherence probe all
	// draw on the same budget and are all reported as admission, so the wall
	// below means run time and nothing else.
	admitted := cordon
	// SWAP PRE-FLIGHT — the step this door never had (register S-25). llama-swap
	// QUEUES, with no timeout of its own, any request that needs a model it is
	// still loading or that would evict a busy one; a run that dialled through
	// that queue spent its wall inside llama-swap and reported a wall timeout —
	// the 600 s failures several parallel sessions reported on 2026-09-01. The
	// delegation door has waited it out OUTSIDE the wall since 2026-09-02 (ADR
	// 0032); this one now calls the same function, on what is left of the same
	// admission budget, and reports it under the same wire field names.
	// Fail-open on a probe error, exactly as over there.
	preflight, preNote := pipeline.AwaitSeatAdmission(ctx, cfg.Endpoint, model, time.Until(admitDeadline))
	admitted += preflight
	if preNote != "" {
		log.Printf("agent_run: seat admission (%s): %s", model, preNote)
	}
	// Cold-load warm-up — the D-64 step the pipeline door has run since 0.115.11
	// and this door never did. A seat that is not loaded loads HERE, on what is
	// left of the admission budget, not inside the wall below; and the window
	// probe after it reads a loaded seat. Without it this door measured ctx_window
	// 8,192 cold and 114,688 warm minutes apart (2026-09-16: a 222 s vLLM cold
	// start outlasted the probe). Reported with the wire's own field names.
	coldLoad, warmNote, warmAttempted := pipeline.WarmSeat(ctx, cfg.Endpoint, model, time.Until(admitDeadline))
	admitted += coldLoad
	admitNote := joinAdmissionNotes(preNote, warmNote)
	// Post-warm COHERENCE probe (register D-118) — the same shared helper the
	// delegation door runs, on the same admission budget, BEFORE the wall
	// context below exists. A seat that is healthy by every other gate and
	// still numerically broken (2026-09-16/17: `<tool_call>!!!!...` to the cap
	// on a freshly loaded vLLM seat) defers here in seconds instead of
	// answering garbage for the whole wall.
	var coherenceNote string
	// "Did the warm-up attempt a load" comes from the warm-up itself, never from
	// its duration (a sub-tick load measures 0) and never from "a note exists"
	// (since W-08 the exits that warm NOTHING speak too). Both derivations were
	// tried and both mis-fire the probe.
	if pipeline.CoherenceProbeWanted(cfg, warmAttempted) {
		act.Phase("coherence-probe")
		v := pipeline.ProbeSeatCoherence(ctx, cfg, model, time.Until(admitDeadline))
		pipeline.RememberCoherence(cfg.Endpoint, model, v)
		if v.Ran {
			coherenceNote = v.Note
		}
		if v.Note != "" {
			log.Printf("agent_run: seat coherence (%s): %s", model, v.Note)
		}
		if v.Broken {
			// The door's own deferred shape, with the probe's time charged to
			// admission (it is admission: the wall has not started).
			dout := map[string]any{"deferred": true, "reason": v.Note, "steps": 0}
			withAdmission(dout, admitted+v.Spent, admitNote)
			withCoherence(dout, coherenceNote)
			withPlaced(dout, placed)
			return deferSized(dout)
		}
		admitted += v.Spent // charged to admission, never to the wall
		act.Phase("running")
	} else if note, ok := pipeline.RecallIncoherentSeat(cfg.Endpoint, model); ok {
		// The seat this process already caught, still resident (reviewer
		// finding, D-118): under the default "cold" policy a warm run is not
		// probed at all, and nothing unloads a broken seat — so every later
		// run would pay a wall for output already proved degenerate. Same
		// deferred shape as a live broken verdict, with no probe spent.
		coherenceNote = note
		log.Printf("agent_run: seat coherence (%s): %s", model, note)
		dout := map[string]any{"deferred": true, "reason": note, "steps": 0}
		withAdmission(dout, admitted, admitNote)
		withCoherence(dout, coherenceNote)
		withPlaced(dout, placed)
		return deferSized(dout)
	}
	// Budget compaction against the SERVED window (probe; conservative fallback
	// inside ResolveContextTokens when unanswerable) and run the measured-ON
	// ladder rungs — the same defaults as the CLI (flip decision 2026-07-24).
	// The resolved window is reported in the result so a fallback is visible.
	//
	// It runs on the ADMISSION deadline and BEFORE the wall context below, the
	// same placement the delegation door gives it (register S-24). The probe
	// carries a ten-minute cold-start budget by design — it is allowed to absorb
	// a load — so on the wall context a cold or slow seat could spend the run's
	// whole clock here and be filed as a wall timeout. The warm-up above usually
	// leaves it one cheap GET, but "usually" is what the note the warm-up now
	// returns exists to contradict: when residency could not be settled, this is
	// exactly the call that pays for it.
	probeStart := time.Now()
	pctx, pcancel := context.WithDeadline(ctx, admitDeadline)
	probed, probeOK, probeFence := agent.ProbeServedWindowChecked(pctx, cfg.Endpoint, model)
	probeCtxErr := pctx.Err() // read BEFORE the cancel, which would mask a spent deadline
	pcancel()
	admitted += time.Since(probeStart)
	if probeFence != nil {
		// A render or an exclusive hold took the card after the cordon and the
		// seat is not resident (2026-09-22): the probe was never sent (its route
		// would load the seat onto the held card) and waited out the admission
		// budget. The delegation door defers the same way, for the same reason.
		dout := map[string]any{
			"deferred":    true,
			"defer_class": string(core.DeferClassCapacity),
			"reason":      "gpu busy: " + probeFence.Error(),
			"steps":       0,
		}
		withAdmission(dout, admitted, joinAdmissionNotes(admitNote, "window probe held behind the GPU lease for the admission budget"))
		withPlaced(dout, placed)
		return deferSized(dout)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	built.Loop.WithObserver(act)
	// WHICH window this run budgets against, and where it came from. This door
	// discarded that line for months while measuring 8,192 cold and 114,688 warm
	// on the same seat, and nothing in either result said which it was.
	effCtx, ctxNote := agent.ResolveContextTokens(0, probed, cfg.AgentCtxTokens, probeOK)
	if ctxNote != "" {
		log.Printf("agent_run: %s", ctxNote)
	}
	if !probeOK && errors.Is(probeCtxErr, context.DeadlineExceeded) {
		// The fallback this change's own trade can cause: an admission budget
		// spent before the probe got its turn. It belongs in admission_note,
		// beside the steps that spent it.
		admitNote = joinAdmissionNotes(admitNote, "window probe ran out of admission budget; "+ctxNote)
	}
	// Real-tokenizer seam (TO-4): whole-message middle cut on the planner's own
	// served token counts; fail-open to the legacy estimate rung when the
	// endpoint has no /tokenize. Same wiring as the CLI, so the drive modes
	// cannot drift.
	built.Loop.WithContextTokens(effCtx).WithSkeletonPrune(true).WithGCFCompact(true).
		WithTokenizer(tokclient.New(cfg.Endpoint, model, 0))
	// Task profile. Until now this door could only produce bare `general` — the
	// one configuration MEASURED to fail (a 4B planner given every tool calls
	// none of them). An unknown name is a clean defer naming the valid ones, not
	// a silent fall back to the configuration we know does not work.
	// A caller that names none now falls back to the BOX's configured agent_profile
	// before general, so a small-seat box is never left on the un-narrowed set by
	// omission — which is what "the default" meant in every measurement that scored
	// 0%. Resolution is explicit > config agent_profile > general, and general is
	// still a no-op on the tool set (Loop.WithProfile), so this is byte-identical
	// behavior for any box that does not set the key.
	prof, perr := agent.LookupProfile(cfg.AgentTaskProfile(strings.TrimSpace(in.Profile)))
	if perr != nil {
		return deferSized(map[string]any{"deferred": true, "reason": perr.Error()})
	}
	built.Loop.WithProfile(prof)
	if in.Judge {
		built.Loop.WithBatchJudge(true)
	}
	// THE FINAL ANSWER FITS THE WALL THAT IS LEFT (register D-134, the cure D-102
	// left open). The wall gate above refuses only a wall too short for any answer;
	// a wall between that floor and the run's estimate is admitted (the INV-5 rider,
	// ADR 0050, forbids refusing on min_turn_sec), and this door's wall is a hard
	// deadline. Without the fit such a run spent its tool steps and then opened the
	// forced final at the configured budget (4x the step budget, up to 8,192 tokens)
	// however little wall remained: a 420 s wall against a seat at 17 tok/s asks for
	// 476 s of decode and is cut at the deadline with no answer. The delegation door's
	// node has installed the same fit since 0.122.1 (D-95); pipeline.InstallFinalFit is
	// the one copy both doors call. At the forced final step the loop now opens the
	// answer at the largest budget the remaining wall can decode at the seat's rate,
	// off the live clock, never above the configured budget and floored at 1,024. The
	// rate is the one the wall gate sized this run from, so a seat with no rate of its
	// own (a named or placed model) is left exactly as it was. startFit is the same
	// figure as it stands when the wall starts, published beside the answer.
	finalBudget, _ := seatrate.FinalBudgets(cfg.AgentMaxTokens, maxSteps, false)
	startFit := pipeline.InstallFinalFit(built.Loop, sizing.Estimate, finalBudget, wallSec, false)
	// nosemgrep note: Loop.Run returns a VALUE-type Result (zero value on error, never nil —
	// internal/agent/loop.go), so res.Steps / res.Effects on the error path below cannot
	// nil-deref. The deferred-ledger access is deliberate (see its own comment). This silences
	// a trailofbits invalid-usage-of-modified-variable false positive that the release gate's ad-hoc
	// scan re-flags on every touch of this file (semgrep is not in CI).
	res, rerr := built.Loop.Run(cctx, in.Goal) //nosemgrep: trailofbits.go.invalid-usage-of-modified-variable.invalid-usage-of-modified-variable
	if rerr != nil {
		// The DEFERRED path carries the effect ledger too — a run that died on
		// timeout with a tool abandoned mid-flight (EffectUnknown) is precisely
		// the run whose caller must not blindly retry. Dropping the ledger here
		// would hide the one record that matters most.
		dout := map[string]any{"deferred": true, "reason": rerr.Error(), "steps": res.Steps}
		if rootExisted && agent.RootGoneOf(rerr) != nil {
			// Register C-79: the read root was there when the call came in and the run's
			// tools can no longer open it. The directory was taken under the run, which no
			// action of the seat undoes: the box's fault, so the class is infrastructure.
			// The reason (rerr's text) opens core.RootGoneReason and names the root.
			dout["defer_class"] = string(core.DeferClassInfrastructure)
		}
		if modelaffinity.IsLeaseRefusal(rerr) {
			// A render or an exclusive hold took the card mid-run and the run's
			// next request waited it out (2026-09-22): congestion, re-placeable.
			dout["defer_class"] = string(core.DeferClassCapacity)
			dout["reason"] = "gpu busy: " + rerr.Error()
		}
		withAdmission(dout, admitted, admitNote)
		withCoherence(dout, coherenceNote)
		withPlaced(dout, placed)
		withFinalFit(dout, startFit, res) // and what the final answer's budget was fitted to (D-134)
		addEffects(dout, res.Effects)
		if len(res.RuleHits) > 0 {
			dout["rules_fired"] = len(res.RuleHits)
		}
		return deferSized(dout) // a run that died at its wall says what the wall was weighed against (D-102)
	}
	out := map[string]any{
		"output":      res.Output,
		"steps":       res.Steps,
		"stop_reason": res.StopReason,
		// POST-narrowing, and the profile that did it. built.Tools is a snapshot taken
		// before WithProfile runs, so reporting it would tell an ampere-6 caller "11
		// tools" on a run that advertised 3 — and with the box-level agent_profile the
		// narrowing can now happen with no caller action at all. Silently changing which
		// tools a run advertises must not be invisible on this door either.
		"tools":      len(built.Loop.AdvertisedTools()),
		"profile":    prof.Name,
		"model":      model,  // the resolved PLANNER seat — visibility is the cure for a silent seat (roast finding)
		"ctx_window": effCtx, // the window compaction budgeted against (probed, else configured, else the conservative fallback)
		// ...and WHICH of those three it was. A number alone cannot distinguish
		// "this seat serves 8,192" from "the probe could not answer, so we
		// assumed 8,192", and the two want opposite fixes.
		"ctx_window_note": ctxNote,
	}
	withAdmission(out, admitted, admitNote)
	withCoherence(out, coherenceNote)
	withSizing(out, sizing.Estimate)
	withFinalFit(out, startFit, res)
	withTruncation(out, res)
	if res.TokenizerPath != "" {
		// Which drop rung the ladder is on — same visibility rule as ctx_window:
		// a sticky fail-open downgrade must be reportable, not inferred.
		out["tokenizer_path"] = res.TokenizerPath
	}
	if res.CompactionsExhausted > 0 {
		out["compactions_exhausted"] = res.CompactionsExhausted // fit=false telemetry: best-effort over-budget requests were sent
	}
	addEffects(out, res.Effects)
	if !cfg.AgentEnvRules.IsZero() {
		// The seat's environment-rule table (ADR 0036) shaped this run — say
		// so, the way profile does: a narrowed or capped run must not be
		// invisible to its caller.
		out["env_rules"] = cfg.AgentEnvRules.Summary()
	}
	if len(res.RuleHits) > 0 {
		out["rules_fired"] = len(res.RuleHits)
	}
	if n := agent.SetupRan(res.Effects); n > 0 {
		out["setup_ran"] = n // replayed before the first turn; the trace's step-0 entries say which
	}
	if res.JudgeReport != "" {
		out["judge_report"] = res.JudgeReport // ADVISORY end-of-run audit of flagged effects
	}
	withPlaced(out, placed)
	return jsonResult(out)
}

// agentRunOrigin labels an agent_run registration: this host, this door.
func agentRunOrigin() string {
	hn, _ := os.Hostname()
	if hn == "" {
		hn = "local"
	}
	return hn + ":agent_run"
}
