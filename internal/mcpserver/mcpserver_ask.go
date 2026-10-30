// offload_ask: the ask handler, its answer grading and the retry path.
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

	"github.com/dmmdea/offload-harness/internal/askcache"
	"github.com/dmmdea/offload-harness/internal/askjob"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// handleAsk is the MCP front door onto the one-call ask lane. It authors the
// contract (internal/askjob), runs it on the local seat through the SAME entry
// a local delegation placement takes (Pipeline.RunAgentContract), evaluates the
// generated acceptance, and returns {answer, evidence} plus the verdict. A first
// attempt that ran on this box's seat and failed that acceptance gets the second
// chance agent_delegate gives a failed verification, on another seat (askRetry).
//
// House style throughout: every failure path is a deferred-shape result, never
// an MCP error (see handleAgentRun) — a caller told "the call failed" discards
// the work, while a caller told "deferred, here is why" reads the files itself,
// which is the correct next action every time this lane cannot deliver.
//
// The acceptance is evaluated HERE and not left implicit. This lane runs one
// local seat rather than going through delegate.Run, so nothing downstream
// would ever check it — and a generated acceptance check that nothing evaluates
// is decoration, which is precisely the "reads as verified" failure the whole
// grounded-anchor design exists to prevent.
func (s *Server) handleAsk(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Question string   `json:"question"`
		Paths    []string `json:"paths"`
		ReadRoot string   `json:"read_root"`
		Route    string   `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// read_root defaulting mirrors handleAgentRun and handleAgentDelegate: the
	// server working dir.
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
	// Every caller-input refusal — empty question, no paths, over a cap, a path
	// outside read_root, no groundable anchor — arrives as one typed error from
	// the builder, so the reason the caller reads is the reason askjob wrote.
	contract, berr := askjob.BuildContract(in.Question, in.Paths, absRoot)
	contract.Door = "offload_ask"
	if berr != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": berr.Error()})
	}
	// THE RESULT CACHE, and its position in this function is the design.
	//
	// It is consulted AFTER BuildContract and not before, because the key is the resolved
	// file CONTENT and BuildContract is what resolves it — reading the files is what makes
	// a hit provably safe, and that read is microseconds against the 46-75 s of seat time a
	// hit skips. Keying on the caller's paths instead would be cheaper by nothing that
	// matters and would make an answer about a file's old bytes servable after an edit.
	//
	// Every refusal BuildContract can raise (no anchor, over a cap, outside read_root)
	// therefore still happens on every call, cached or not: a refusal is never short-
	// circuited, only a finished answer is.
	//
	// What this does NOT do: make a DIFFERENT question over the same files any cheaper. It
	// pays on an exact repeat and nothing else. Warm-context reuse across different
	// questions would need a pinned llama-swap slot, which trades seat availability for
	// cache warmth and was declined.
	cacheKey := askcache.Key(strings.TrimSpace(in.Question), absRoot, contract.Context)
	if cached, hit := s.askCache.Get(cacheKey); hit {
		// Never present a cached answer as a fresh run. The caller decides whether a
		// re-read is warranted, and the adoption instrument has to be able to tell a
		// seat that ran from a seat that did not.
		cached["cache_hit"] = true
		return jsonResult(cached)
	}
	// Known gap: no single-flight / in-flight dedup on a miss. Two concurrent identical asks
	// (same cacheKey) that both arrive before either has stored a result will both fall
	// through here and both pay full seat time — the second one's Put simply overwrites the
	// first's. Unlikely today: MCP over stdio serves one request at a time per connection, so
	// nothing in this codebase can actually issue that second call concurrently. If a caller
	// shape ever changes that (a concurrent client, or a fan-out that awaits offload_ask
	// itself), the fix is a per-key in-flight map — e.g. golang.org/x/sync/singleflight —
	// wrapped around the run(ctx, contract) call below, coalescing concurrent misses on one
	// key into a single seat run.
	run := s.localAgent // test seam, shared with agent_delegate
	if run == nil {
		run = s.p.RunAgentContract
	}
	// No context deadline is imposed here: the contract's TimeoutSec is the wall,
	// the run's expectation (ADR 0055), and runAgentTask's liveness monitor owns the
	// deadline (a stall, or the safety ceiling), so wrapping it again would give the
	// run two budgets that could disagree.
	var wire core.AgentWireResult
	var fleetExtra map[string]any
	now := s.askNow // test seam
	if now == nil {
		now = time.Now
	}
	var started, ended time.Time // the local attempt's span, which sizes what a retry may spend
	route := strings.TrimSpace(in.Route)
	namedRoute := route // what the caller asked for, before the occupied-seat fallback below
	// An ask names no route most of the time, and "no route" meant this box's agent seat
	// even when loading it would unload another vLLM seat holding the cards: on the
	// reference box, opencode's three-card seat, whose session then paid a cold load
	// (2026-10-06). The contract is self-contained, so in that case it takes the
	// delegator's auto placement, which deals to a remote with room, waits in line when
	// every remote is full, and comes back here only when no remote can take it. An
	// explicit route:"local" still runs here.
	routeNote := ""
	if route == "" {
		if seat := s.askOccupant(ctx); seat != "" {
			route = "auto"
			routeNote = "no route named and the local seat is occupied: loading it would evict the loaded vLLM seat " + seat + ", so the ask took route=auto"
		}
	}
	if route != "" && route != "local" {
		// register C-46: the contract is self-contained (the files ride inline), so it
		// can run wherever the delegator places it.
		w, extra, note, ok := s.contractOnFleet(ctx, contract, route, "")
		if !ok {
			deferred := map[string]any{"deferred": true, "reason": note, "route": route}
			if routeNote != "" {
				deferred["route_note"] = routeNote
			}
			return jsonResult(withFinishedAnswer(deferred, w))
		}
		if routeNote != "" {
			extra["route_note"] = routeNote
		}
		wire, fleetExtra = w, extra
	} else {
		started = now()
		w, rerr := run(ctx, contract, delegate.LocalOptions{})
		ended = now()
		if rerr != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": rerr.Error()})
		}
		wire = w
	}
	if wire.Deferred {
		return jsonResult(withFinishedAnswer(map[string]any{
			"deferred":    true,
			"reason":      wire.Reason,
			"defer_class": wire.DeferClass,
			"seat":        wire.Seat,
			"steps":       wire.Steps,
		}, wire))
	}
	answer, evidence, failures := gradeAskWire(contract, wire)
	if strings.TrimSpace(answer) == "" {
		// A non-deferred result with neither prose nor a structured answer has nothing
		// to publish. Returning answer:"" with no deferred key would read to the caller
		// as "the seat answered, and the answer is nothing" — the silent shape this
		// lane's defer-not-crash posture exists to avoid. It should not be reachable
		// (the loop defers on an empty final message), which is exactly why it must not
		// be the one path that fails quietly if it ever becomes reachable.
		return jsonResult(map[string]any{
			"deferred":    true,
			"reason":      "the seat returned no answer (stop_reason " + wire.StopReason + ")",
			"defer_class": core.DeferClassAbstention,
			"seat":        wire.Seat,
			"steps":       wire.Steps,
		})
	}
	// THE CROSS-SEAT RETRY. A first attempt that ran on this box's seat and failed its acceptance
	// was terminal here, while agent_delegate retries a failed verification on another seat. The
	// fleet path needs nothing added: its engine already retried inside the call, and
	// contractOnFleetAt now says so (retried_on, retry_note).
	//
	// Only when the caller named NO route: a named route:"local" is the caller saying the material stays
	// on this box (offload_ask has no pin_reason channel), so its failed first attempt is published as it
	// is and the goal and the inlined files never leave for a retry.
	retriedOn, retryNote := "", ""
	if len(failures) > 0 && fleetExtra == nil && namedRoute == "local" {
		retryNote = "retry skipped: route local was named; the ask stays on this box"
	} else if len(failures) > 0 && fleetExtra == nil {
		r := s.askRetry(ctx, contract, wire, failures, ended.Sub(started))
		retriedOn, retryNote = r.node, r.note
		if r.won {
			wire, answer, evidence, failures, fleetExtra = r.wire, r.answer, r.evidence, nil, r.extra
		}
	}
	out := map[string]any{
		"answer":      answer,
		"evidence":    evidence,
		"verified":    len(failures) == 0,
		"acceptance":  contract.Acceptance, // what "verified" actually means, published so it can be judged
		"seat":        wire.Seat,
		"steps":       wire.Steps,
		"stop_reason": wire.StopReason,
		// Published as FALSE rather than omitted: an absent field reads as unknown, and
		// "was this answer computed just now" is exactly the question a caller — and the
		// adoption instrument — must never have to guess at. It rides the ANSWER shape
		// only; a deferred result carries no cache_hit because a defer is never stored,
		// so it is always a fresh run and the field would have nothing to distinguish.
		"cache_hit": false,
	}
	out = withReviewExtra(out, fleetExtra)
	// Set after the fleet block, so the ask's own account of its two attempts is the one published
	// when the fleet's engine retried inside the retry as well (askRetry keeps that note inside its own).
	if retriedOn != "" {
		out["retried_on"] = retriedOn
	}
	if retryNote != "" {
		out["retry_note"] = retryNote
	}
	if len(failures) > 0 {
		out["acceptance_failures"] = failures
	}
	// Only a SUCCESSFUL, non-deferred result is stored, and every other exit from this
	// handler returned above: a defer, a refusal and a runner error are all statements
	// about this minute rather than about these files, and caching one would turn a
	// transient seat failure into a lane that stays dead for the rest of the connection.
	//
	// A verified:false answer IS cached — the seat ran and answered, the citation check
	// simply did not match — so an identical repeat returns the same unverified answer
	// instead of re-rolling the seat. That is the deliberate cost of caching by content:
	// a caller who wants another attempt changes the question, which is a different key.
	// The entry is the answer as published, so one that went through the cross-seat retry
	// carries retried_on and retry_note as they are: a repeat never retries again.
	s.askCache.Put(cacheKey, out)
	return jsonResult(out)
}

// gradeAskWire decodes one attempt's wire result into the pair the caller is shown and grades
// exactly that pair against the contract's acceptance. Every attempt of one ask goes through it,
// so the cross-seat retry is decoded and graded exactly like the first attempt.
//
// The contracted deliverable is the structured pair. When the re-pack seat could not be reached
// the loop's prose still arrived, so it is returned as the answer rather than thrown away — with
// evidence empty, which is exactly what nonempty:evidence then reports, so the caller is never
// handed unchecked prose under a green verdict. An answer that is blank after that has nothing to
// publish: the caller checks for it (a blank pair is graded here like any other and fails).
//
// THE INVARIANT: grade exactly the text the caller is shown — the two fields returned, nothing
// else. Everything about this function is that one rule.
//
// core.evalText prefers wire.Output whenever it is non-empty, and runAgentTask always
// sets Output before the re-pack, so grading the wire as it arrives grades the loop's
// final PROSE, which this handler never publishes. Both ways of getting that wrong
// have now been measured, one per direction:
//
//   - grading the prose: it is longer than the condensed pair and so likelier to
//     contain a frequent token, giving verified:true beside a published answer that
//     cites nothing — "reads as verified while nothing verified it";
//   - blanking Output to fall through to the raw bytes: when the re-pack emits
//     {"answer":"","evidence":"<text>"} — schema-legal, and its own system prompt
//     says "Use empty values when a field is absent" — the fallback to the prose would
//     publish PROSE as the answer while only the JSON gets graded, giving verified:false
//     on a properly cited answer.
//
// Building the graded text from the DECODED fields closes both, and closes a third
// hole for free: the gbnf grammar permits \uXXXX escapes, so an anchor plainly
// readable in the published evidence could be invisible in the raw bytes.
//
// Joined with a newline so a match cannot span the seam between two fields, and
// Structured is left intact because nonempty:evidence reads it. With no Structured at
// all, Answer already holds the prose and Evidence is empty, so this degenerates to
// grading the prose — which is exactly what that path publishes.
//
// delegate.Run grades prose and PUBLISHES prose, so it is coherent and untouched;
// this is the first lane that publishes only the structured pair.
func gradeAskWire(contract core.AgentContract, wire core.AgentWireResult) (answer, evidence string, failures []string) {
	var structured struct {
		Answer   string `json:"answer"`
		Evidence string `json:"evidence"`
	}
	if len(wire.Structured) > 0 {
		// Error deliberately ignored, not swallowed: the re-pack only publishes
		// Structured after validating it against this contract's own schema, so a
		// decode failure here would mean a shape that cannot occur — and if it ever
		// did, the fallback below still publishes the loop's prose rather than an
		// empty answer, which is the right outcome either way.
		_ = json.Unmarshal(wire.Structured, &structured)
	}
	if structured.Answer == "" {
		structured.Answer = wire.Output
	}
	graded := wire
	graded.Output = structured.Answer + "\n" + structured.Evidence
	return structured.Answer, structured.Evidence, delegate.EvalAcceptance(contract, graded)
}

// askRetryOutcome is what the cross-seat retry of one ask came to. won means the retry is clean
// and its wire, pair and fleet block replace the first attempt's; otherwise the first attempt
// stands and only note (and node, when a node ran the retry) rides it.
type askRetryOutcome struct {
	won              bool
	wire             core.AgentWireResult
	answer, evidence string
	extra            map[string]any
	node, note       string
}

// askRetryFloorSec is the least wall a cross-seat retry of an ask is worth starting with: the
// floor agent_delegate applies to its verification retry before it picks a node, the configured
// agent_retry_min_sec and never under delegate's 10 s minimum. (The seat-aware floor delegate
// applies after choosing a node belongs to the engine, which applies it to its own retry inside
// the call.)
func askRetryFloorSec(cfg config.Config) int {
	if cfg.AgentRetryMinSec > 10 {
		return cfg.AgentRetryMinSec
	}
	return 10
}

// askRetry gives a failed LOCAL first attempt the second chance agent_delegate gives a failed
// verification: the same contract on the fleet, route "remote", whose first placement never lands
// on this box's seat. The delegator's own retry inside that call is kept off the seat too
// (RunOptions.RemoteRetryOnly, through contractOnFleetAt): a bare remote route would fall back to
// this box when the fleet node's answer also fails, and the first attempt already failed here, so
// a caller who did not name route local would get a third run on this box's GPU under a note that
// still says "retry on <node>". The fleet's own skipped retry rides in that note instead. It
// mirrors the delegator's retry (mergeAttempts, runOne): the retry lives inside the contract's own wall, so it is handed what
// the first attempt left (timeout_sec, never auto) and is skipped under the floor; a clean retry
// is published with retried_on and a note naming the first attempt's verdict; a retry that also
// fails leaves the FIRST answer, unverified, with the retry's fate in the note; a fleet that took
// nothing leaves a "retry skipped" note and no retried_on, which would name a node that never ran it.
// The retry's wire is decoded and graded by gradeAskWire, exactly like the first attempt's.
func (s *Server) askRetry(ctx context.Context, contract core.AgentContract, first core.AgentWireResult, firstFailures []string, elapsed time.Duration) askRetryOutcome {
	// The budget is the one delegate charges the same contract (executionBudgetSec): an ask never
	// names its wall, so its contract is auto-sized and owns the cap.
	budget := contract.TimeoutSec
	switch {
	case contract.TimeoutAuto:
		budget = core.AgentTimeoutSecCap
	case budget <= 0:
		budget = core.AgentTimeoutSecDefault
	}
	remaining := budget - int((elapsed.Milliseconds()+999)/1000)
	if floor := askRetryFloorSec(s.p.Cfg()); remaining < floor {
		return askRetryOutcome{note: fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after the first attempt (floor %ds)", remaining, budget, floor)}
	}
	retry := contract
	retry.TimeoutSec = remaining
	retry.TimeoutAuto = false // a retry's wall is what is left: explicit, never auto
	w, extra, why, ok := s.contractOnFleetAt(ctx, retry, "remote", "", nil, true)
	node, _ := extra["node"].(string)
	if !ok && node == "" {
		return askRetryOutcome{note: "retry skipped: " + why}
	}
	// What the fleet's own engine did inside the retry rides after the ask's account of it.
	inner, _ := extra["retry_note"].(string)
	withInner := func(note string) string {
		if inner == "" {
			return note
		}
		return note + "; the fleet's own retry: " + inner
	}
	label := node
	if label == "" {
		label = "(unnamed node)"
	}
	outcome := ""
	switch {
	case !ok:
		outcome = fmt.Sprintf("deferred (%s): %s", w.DeferClass, w.Reason)
	default:
		answer, evidence, failures := gradeAskWire(contract, w)
		switch {
		case strings.TrimSpace(answer) == "":
			outcome = "returned no answer (stop_reason " + w.StopReason + ")"
		case len(failures) > 0:
			outcome = fmt.Sprintf("failed_verification: %v", failures)
		default:
			return askRetryOutcome{won: true, wire: w, answer: answer, evidence: evidence, extra: extra, node: node,
				note: withInner(fmt.Sprintf("first attempt on %s failed_verification: %v; this result is the retry", askSeatLabel(first), firstFailures))}
		}
	}
	return askRetryOutcome{node: node, note: withInner("retry on " + label + " also " + outcome + "; this result is the first attempt")}
}

// askSeatLabel names the seat a first attempt ran on for the retry note.
func askSeatLabel(w core.AgentWireResult) string {
	if w.Seat == "" {
		return "the local seat"
	}
	return w.Seat
}
