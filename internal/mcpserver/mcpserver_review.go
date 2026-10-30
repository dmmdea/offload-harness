// offload_review_diff: the review handler, fleet dispatch helpers and review payload shaping.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/reviewlane"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

// handleReviewDiff is the MCP front door onto the clean-context review lane. It
// authors the contract (internal/reviewlane), runs it on the local seat through
// the SAME entry a local delegation placement takes (Pipeline.RunAgentContract),
// then parses, grounds and ranks the seat's findings before publishing them.
//
// House style throughout: every failure path is a deferred-shape result, never an
// MCP error (see handleAgentRun) — a caller told "the call failed" discards the
// work, while a caller told "deferred, here is why" reviews the diff itself, which
// is the correct next action every time this lane cannot deliver.
//
// Unlike handleAsk there is no `verified` verdict, and its absence is deliberate
// rather than an omission: an empty findings list is a CORRECT outcome here, so no
// acceptance check could distinguish a clean review from a review that never
// happened without punishing one of them. What stands in its place is a check the
// harness can actually make — a finding naming a file the diff never touched is
// dropped and counted (reviewlane.Ground), as is a line with no severity, file or why
// (reviewlane.DropHollow) — plus the refusal to publish an empty findings list unless the
// seat's own answer reads as a clean verdict, which holds for a list the filters emptied
// as much as for one the seat left empty.
func (s *Server) handleReviewDiff(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Diff        string `json:"diff"`
		DiffPath    string `json:"diff_path"`
		Task        string `json:"task"`
		MaxFindings int    `json:"max_findings"`
		ReadRoot    string `json:"read_root"`
		// SHA-bound evidence: optional, echoed and bound, never resolved.
		BaseSHA      string                   `json:"base_sha"`
		HeadSHA      string                   `json:"head_sha"`
		GateReceipts []reviewlane.GateReceipt `json:"gate_receipts"`
	}
	// The durable trace sits beside the ledger, wherever the operator put it, and
	// refuse seals every exit that comes before a diff was read: a refusal is a
	// diff-less NOT_RUN naming its reason, bound to no bytes, and it leaves its trace row like
	// every other result of this door. The deferral shape and reason are the ones this door
	// always gave; outcome, outcome_reason and evidence ride beside them.
	cfg := s.p.Cfg()
	tracePath := reviewlane.TracePathFor(cfg.LedgerPath, cfg.BaseDir())
	refuse := func(reason string) (*mcp.CallToolResult, error) {
		return jsonResult(reviewlane.Seal(map[string]any{"deferred": true, "reason": reason}, reviewlane.Refused(reason), reviewlane.Run{}, tracePath))
	}
	// Decoded here, not through parseArgs, so that arguments that do not decode are sealed too.
	if raw := req.Params.Arguments; len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return refuse("bad arguments: " + err.Error())
		}
	}
	// EXACTLY one source. Accepting both and silently preferring one would let a
	// caller review a diff it did not think it was reviewing — the review is only
	// worth anything if the caller knows which bytes were judged.
	hasInline, hasPath := strings.TrimSpace(in.Diff) != "", strings.TrimSpace(in.DiffPath) != ""
	switch {
	case hasInline && hasPath:
		return refuse("pass diff OR diff_path, not both — the review must be unambiguous about which bytes it judged")
	case !hasInline && !hasPath:
		return refuse("one of diff (inline text) or diff_path (a file holding a unified diff) is required")
	}
	diff := in.Diff
	// The second fingerprint check re-reads the diff right before the result is returned. Held
	// inline it cannot have moved; a diff_path file can, and is re-read the way it was first.
	recheck := func() (string, error) { return diff, nil }
	if hasPath {
		// read_root defaulting mirrors handleAsk and handleAgentRun: the server
		// working dir. The diff file goes through delegate.InlineContextPaths —
		// the ONE delegator-side confined reader (os.Root containment, 128 KiB
		// cap, an error naming the offending path) — rather than a bare ReadFile,
		// so this lane cannot become the one door that reads outside read_root.
		//
		// The three calls go through the reviewDiffIO seam (the real ones in production), so
		// the refusals behind them are exercised rather than assumed.
		fsio := s.reviewIO()
		readRoot := in.ReadRoot
		if readRoot == "" {
			wd, werr := fsio.Getwd()
			if werr != nil {
				return refuse("cannot determine working dir for read_root: " + werr.Error())
			}
			readRoot = wd
		}
		absRoot, aerr := fsio.Abs(readRoot)
		if aerr != nil {
			return refuse("bad read_root: " + aerr.Error())
		}
		docs, ierr := fsio.Read([]string{in.DiffPath}, absRoot)
		if ierr != nil {
			return refuse(ierr.Error())
		}
		if len(docs) != 1 {
			return refuse("diff_path read returned no content")
		}
		diff = docs[0].Text
		recheck = func() (string, error) {
			again, rerr := fsio.Read([]string{in.DiffPath}, absRoot)
			if rerr != nil {
				return "", rerr
			}
			if len(again) != 1 {
				return "", fmt.Errorf("diff_path read returned no content")
			}
			return again[0].Text, nil
		}
	}
	// SHA-bound evidence, built BEFORE the builder and before any seat: the diff's
	// fingerprint and the deterministic gate receipts exist first, so even a refusal below is
	// bound to the bytes it refused. seal stamps outcome + evidence on every exit from here on
	// (the exits above it went through refuse) and leaves the durable trace beside the ledger.
	ev, evErr := reviewlane.NewEvidence(diff, in.BaseSHA, in.HeadSHA, in.GateReceipts)
	seal := func(out map[string]any, run reviewlane.Run) (*mcp.CallToolResult, error) {
		run.Recheck = recheck
		return jsonResult(reviewlane.Seal(out, ev, run, tracePath))
	}
	if evErr != nil {
		return seal(map[string]any{"deferred": true, "reason": evErr.Error()}, reviewlane.Run{})
	}
	// Every caller-input refusal — no task, an empty diff, a diff over the byte
	// ceiling — arrives as one typed error from the builder, so the reason the
	// caller reads is the reason reviewlane wrote.
	contract, berr := reviewlane.BuildContract(in.Task, diff)
	contract.Door = "offload_review_diff"
	if berr != nil {
		return seal(map[string]any{"deferred": true, "reason": berr.Error()}, reviewlane.Run{})
	}
	ev = ev.WithPrompt(contract.Goal)
	// publish turns ONE seat's wire result into the lane's published answer (reviewPayload) and
	// seals it. It is shared by the local path and the fenced-seat fleet path on purpose: the
	// filters are the whole reason this lane's output is worth reading, and a remote review is
	// produced by a SMALLER seat than the local one more often than not, so a second copy of
	// these rules for the fleet path would be the copy that drifted.
	publish := func(wire core.AgentWireResult, extra map[string]any) (*mcp.CallToolResult, error) {
		out, delivered := reviewPayload(wire, diff, in.MaxFindings, extra)
		return seal(out, reviewlane.Run{Delivered: delivered, Wire: &wire, Extra: extra})
	}
	// The review's wall is the box's agent_timeout_sec when that is larger than
	// the wire default (0.115.21, register D-03 — the wall half of D-09): the
	// 300 s default sized the lane for a fast seat, and on the 30 tok/s 27B a
	// 51 KB diff spent 9 steps and timed out at exactly 300 s (2026-09-10
	// 20:4x) while a 13 KB one finished in 3. agent_timeout_sec is the number
	// the seat's owner set for this seat (600 on the reference box); the wire
	// ceiling still caps it.
	if wall := int(agentTimeout(0, s.p.Cfg()).Seconds()); wall > contract.TimeoutSec {
		contract.TimeoutSec = wall
		// The owner's number is explicit, never auto-sized (D-03). A box that
		// sets no agent_timeout_sec above the default leaves the marker on, and
		// the review contract is then sized by the seat's rate like any other.
		contract.TimeoutAuto = false
		if contract.TimeoutSec > core.AgentTimeoutSecCap {
			contract.TimeoutSec = core.AgentTimeoutSecCap
		}
	}
	// THE FENCE CHECK (register D-110), before any local loop is built. Under a
	// lease this process does not hold and that refuses new runs — an exclusive
	// text hold, a draining cordon, a media render — the local loop below cannot
	// answer: its own in-run fence check (agenttask.go, register S-26) defers
	// `capacity` at once ("gpu busy: ... until it is released"); before that check
	// landed it first waited the whole agent_lease_wait_sec at the cordon. Either
	// way, for the lease's ENTIRE length the one lane a lead reaches for at the
	// moment of deciding answered nothing, however idle the fleet was. The
	// verdict is on disk before the dial, so the review is offered to the fleet
	// first — the same sentence register D-94 wrote about the retry path.
	//
	// ForeignFence, not Fenced: the holder's own session (GPU_LEASE_EPOCH) keeps
	// the local path exactly as it was. Its lease exists to keep this work on
	// these cards.
	var fleetNote map[string]any
	// The local loop runs on the planner seat, so the fence is asked of that seat's cards
	// (plan P4): a render on another card does not send the review off the box.
	reviewSeatPins, _ := modelaffinity.PinsFor(cfg.AgentPlannerModel(""))
	if fenced, why := delegate.ForeignFenceFor(delegate.LocalLease(cfg.GPULockPath, cfg.StateDir), reviewSeatPins); fenced {
		wire, extra, note, ok := s.reviewOnFleet(ctx, contract, why)
		if ok {
			return publish(wire, extra)
		}
		// A fleet seat that WROTE the review and was stopped by the clock on its
		// structuring (F20: a cut final, a re-pack with no wall left) deferred it, and the
		// local leg below can only defer `capacity` on this very fence. Its lines are worth
		// more than that defer, so they get the same read a local review's would:
		// published, with the fleet's provenance, only when they survive the filters.
		// Anything else — and a salvage that comes to no review — falls through to the
		// local leg as before.
		if wire.Deferred {
			if out, delivered := reviewPayload(wire, diff, in.MaxFindings, extra); delivered {
				return seal(out, reviewlane.Run{Delivered: true, Wire: &wire, Extra: extra})
			}
		}
		// Nothing out there took it. The local leg runs as it would have: while the
		// fence holds it defers `capacity` at once, naming the holder (only a fence
		// taken after its check meets the cordon's wait), and the note says the fleet
		// WAS asked, so a caller reading "gpu busy" does not have to wonder whether
		// the fleet was tried.
		fleetNote = map[string]any{"fleet": note}
	}
	run := s.localAgent // test seam, shared with agent_delegate and offload_ask
	if run == nil {
		run = s.p.RunAgentContract
	}
	// No context deadline is imposed here: the contract's TimeoutSec is the wall,
	// the run's expectation (ADR 0055), and runAgentTask's liveness monitor owns the
	// deadline (a stall, or the safety ceiling), so wrapping it again would give the
	// run two budgets that could disagree.
	wire, rerr := run(ctx, contract, delegate.LocalOptions{})
	if rerr != nil {
		return seal(withReviewExtra(map[string]any{"deferred": true, "reason": rerr.Error()}, fleetNote), reviewlane.Run{})
	}
	return publish(wire, fleetNote)
}

// reviewOnFleet offers ONE review to the fleet at route=remote (register D-110),
// for a lane whose own seat is fenced by someone else's lease.
//
// It hands the whole contract to delegate.RunWith — placement, the ctx-fit gate,
// the re-placement loop, the quarantine and the telemetry — rather than dialling
// a node itself, because every one of those rules is what makes a remote review
// worth publishing. The QUALITY FLOOR the plan states is exactly remoteEligible's:
// the seat is resident, the node advertises the agent lane, its card is not itself
// leased, and the contract's estimated tokens plus the loop's reserve fit the
// node's agent_ctx_tokens. A node that fails any of it is not asked.
//
// route "remote" and not "auto" is the other half of that floor: auto would fall
// back to the LOCAL seat when no remote qualified, which is the fenced seat this
// whole path exists to avoid — RunWith's remote route defers loudly instead and
// never places locally. So an accepted result here provably ran somewhere else.
//
// ok=false means "the fleet took nothing"; the note says why, and the caller keeps
// today's path. Every failure shape is a note rather than an error, for the same
// reason every other failure in this lane is a defer: the caller's next action is
// the same either way.
func (s *Server) reviewOnFleet(ctx context.Context, contract core.AgentContract, fence string) (core.AgentWireResult, map[string]any, string, bool) {
	return s.contractOnFleet(ctx, contract, "remote", fence)
}

// askOccupant names the declared vLLM seat that loading this box's agent seat would
// unload (the seat guard's verdict, the reading the cascade uses), or "" when the load
// would evict nothing, the guard is off, or the reading is unknown (an unknown reading
// names no seat, and the ask then runs local exactly as it always did).
func (s *Server) askOccupant(ctx context.Context) string {
	cfg := s.p.Cfg()
	model := strings.TrimSpace(cfg.AgentPlannerModel(""))
	if model == "" {
		return ""
	}
	check := s.askSeatGuard
	if check == nil {
		check = seatguard.Shared(cfg).Check
	}
	if v := check(ctx, model); v.Protect {
		return v.Seat
	}
	return ""
}

// contractOnFleet runs ONE contract through the delegator at the named route —
// the single-contract RunWith path reviewOnFleet built for the fence, now also
// behind `route` on agent_run and offload_ask (register C-46, S-27: those doors
// always ran local, so a remote seat could not be named from this box's door at
// all). The extras say where it ran.
//
// It hands the engine NO rescue of a finished answer whose structured re-pack failed
// (register C-66, PR-4), on purpose: docs/systems/coding-agent.md records the three
// doors that do not wire it and why (agent_run's routed contract carries only a flat
// {answer} schema the delegator requires of every remote run, the review lane's fenced
// fallthrough must not queue behind the lease that fences it, and offload_ask keeps its
// own handling of a finished answer, so that its prose still reaches the caller). A
// deferred result is therefore handed back beside its reason (ok=false), so that caller
// can keep the answer it carries (withFinishedAnswer).
func (s *Server) contractOnFleet(ctx context.Context, contract core.AgentContract, route, fence string) (core.AgentWireResult, map[string]any, string, bool) {
	// remotes nil: RunWith reads the configured delegate_remotes, which is the
	// fleet this box is a delegator for. A review names no node of its own.
	return s.contractOnFleetAt(ctx, contract, route, fence, nil, false)
}

// contractOnFleetAt is contractOnFleet narrowed to remotes: the nodes the engine may
// ask, which REPLACE the configured roster for this call (delegate.RunWith) and are
// checked against it (RosterOnly), so a door can only narrow the fleet, never add to
// it. agent_run names the nodes whose agent seat is the model the caller asked for
// (register D-125). nil remotes is the configured roster, as before.
//
// A narrowed call is also a REASONED remote pin (delegate.PinOperator): the caller
// named the model, so the placement is theirs and a second chance for the contract is
// another of those nodes or none. A bare route=remote keeps the engine's remote ->
// local retry, and for a named seat that retry runs the goal on THIS box's own agent
// seat, a different model: the door then withholds that clean answer for a seat
// mismatch and blames the node, after this box's GPU did the whole goal. Only the
// retry changes; the first placement of route=remote already never falls back to local.
//
// remoteRetryOnly asks for the same stop without the pin: the engine's retry of this call goes to
// another fleet node or is not run, and never to this box's seat (RunOptions.RemoteRetryOnly). It
// records no pin_reason, so the call is still counted as unreasoned in offload_status. Only a door
// whose caller already moved the work off this box says it (askRetry); every other door keeps the
// bare route's remote -> local retry.
func (s *Server) contractOnFleetAt(ctx context.Context, contract core.AgentContract, route, fence string, remotes []string, remoteRetryOnly bool) (core.AgentWireResult, map[string]any, string, bool) {
	dispatch := s.reviewFleet // test seam
	if dispatch == nil {
		dispatch = delegate.RunWith
	}
	local := s.localAgent
	if local == nil {
		local = s.p.RunAgentContract
	}
	opts := &delegate.RunOptions{Quarantine: s.quarantine, Tenant: s.tenant, PinTally: s.pins, RosterOnly: len(remotes) > 0, RemoteRetryOnly: remoteRetryOnly}
	if len(remotes) > 0 && route == "remote" {
		opts.PinReason = delegate.PinOperator
	}
	results, _, err := dispatch(ctx, s.p.Cfg(), local, []core.AgentContract{contract}, route, remotes, opts)
	switch {
	case err != nil:
		return core.AgentWireResult{}, nil, "the fleet could not be asked: " + err.Error(), false
	case len(results) != 1:
		return core.AgentWireResult{}, nil, fmt.Sprintf("the fleet dispatch returned %d results for one review", len(results)), false
	}
	pr := results[0]
	if pr.Err != "" {
		return core.AgentWireResult{}, nil, "the fleet failed the review: " + pr.Err, false
	}
	extra := map[string]any{
		"executed_on": "fleet",
		"node":        pr.Node,
		"placement":   pr.PlacementReason,
		// The fence is published beside the result: a review that did NOT run on
		// this box has a different provenance, and the reader is owed the reason
		// it moved.
		"fence": fence,
	}
	// The id the node's own result named it by, when that is not the one the placement used ("node" above).
	if rep := delegate.NodeReported(pr); rep != "" {
		extra["node_reported"] = rep
	}
	if pr.Seat != "" {
		extra["seat"] = pr.Seat
	}
	// A second attempt the delegator ran inside this call (a failed verification retried on another
	// node): said here, so every door that publishes the block says it too. Both are omitted when no
	// retry ran, and a retry that was considered and skipped carries a note and no node.
	if pr.RetriedOn != "" {
		extra["retried_on"] = pr.RetriedOn
	}
	if pr.RetryNote != "" {
		extra["retry_note"] = pr.RetryNote
	}
	if pr.Result.Deferred {
		// The ordinary shape: "route=remote: no eligible remote" — nothing out
		// there fits the diff, or nothing is up. Verbatim, because the reason
		// distinguishes "no remotes configured" from "they answered and the diff
		// does not fit their window", and those need different fixes.
		//
		// The provenance map rides a deferral too (F20): the review lane can still
		// salvage a fleet seat's written answer from one (reviewPayload), and must say
		// which node wrote it. The doors that publish a bare defer read extra only on
		// success, so they never see it.
		return pr.Result, extra, pr.Result.Reason, false
	}
	return pr.Result, extra, "", true
}

// withFinishedAnswer adds a deferred result's finished answer to the payload a door
// publishes for it, flagged schema_miss, when the node said the loop FINISHED and only
// its structuring failed (register C-66, C-80). The answer is the loop's own prose: it
// is never graded and never presented as an answer, only kept beside the defer so the
// caller is not handed a bare "deferred" for work that was done. offload_review_diff does
// not use it: what that lane publishes went through its grounding filters, and the raw
// prose has not.
func withFinishedAnswer(out map[string]any, w core.AgentWireResult) map[string]any {
	if w.SchemaMiss && strings.TrimSpace(w.Output) != "" {
		out["output"], out["schema_miss"] = w.Output, true
	}
	return out
}

// withReviewExtra folds the fleet block (node, placement, seat, fence — or the
// note saying the fleet took nothing) into one published review result. Empty
// values are dropped rather than published as "": a blank `node` would read as a
// node that failed to identify itself.
func withReviewExtra(out map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// reviewPayload is the review lane's publish decision, apart from the transport so the fenced-seat fleet
// path can put the same question to a deferred result. It reads the seat's findings — from the
// structured object, or, for a deferral the CLOCK caused on an answer the seat had written,
// straight from its raw lines (reviewlane.Salvage) — then drops hollow lines, grounds, dedupes,
// folds same-line restatements, ranks and caps them, applies the clean-verdict gate and words the
// notes. delivered is true when the payload is a REVIEW (findings, or the seat's own clean
// verdict) and false when it is a deferral.
func reviewPayload(wire core.AgentWireResult, diff string, maxFindings int, extra map[string]any) (out map[string]any, delivered bool) {
	// bareDefer is the deferral exactly as the node filed it. Every deferral the lane cannot
	// turn into a review keeps this shape, so a caller that read "gpu busy" or "seat down"
	// before still reads it.
	bareDefer := func() (map[string]any, bool) {
		return withReviewExtra(map[string]any{
			"deferred":    true,
			"reason":      wire.Reason,
			"defer_class": wire.DeferClass,
			"seat":        wire.Seat,
			"steps":       wire.Steps,
		}, extra), false
	}
	var lines []string
	salvaged := ""
	if wire.Deferred {
		// A deferral is a review only when the clock ended the structuring of an answer the
		// seat had written (F20, 2026-10-09: three of five review defers in 26 hours were a
		// cut or un-repacked answer whose lines the lane threw away). Everything else — a
		// busy card, a seat that failed, a re-pack that answered the wrong shape — is the
		// bare defer it always was. The raw prose is still never published: only what
		// survives the filters below is.
		if salvaged, lines = reviewlane.Salvage(wire); salvaged == "" {
			return bareDefer()
		}
	} else {
		var structured struct {
			Findings []string `json:"findings"`
		}
		// A non-deferred run with an OutputSchema always carries Structured
		// (runAgentTask defers on every re-pack failure), so neither branch below
		// should be reachable. They defer rather than publishing an empty findings
		// list precisely because of what that list would MEAN: "this reviewer found
		// nothing" is the one shape a caller might read as reassurance, so it must
		// never be what a broken result degrades into.
		if len(wire.Structured) == 0 {
			return withReviewExtra(map[string]any{
				"deferred": true, "reason": "the seat returned no structured findings (stop_reason " + wire.StopReason + ")",
				"defer_class": core.DeferClassAbstention, "seat": wire.Seat, "steps": wire.Steps,
			}, extra), false
		}
		if uerr := json.Unmarshal(wire.Structured, &structured); uerr != nil {
			return withReviewExtra(map[string]any{
				"deferred": true, "reason": "could not decode the seat's findings: " + uerr.Error(),
				"defer_class": core.DeferClassAbstention, "seat": wire.Seat, "steps": wire.Steps,
			}, extra), false
		}
		lines = structured.Findings
	}
	rep := reviewlane.Report(lines, diff, maxFindings)
	if !wire.Deferred {
		// The structured re-pack is a copy of the seat's answer, and a lossy one (F1, root
		// cause, 2026-10-09). Probed live on the fleet seats with this lane's exact prompt and
		// schema: the grammar-lane re-pack SPLITS lines (a qwen3.6 seat's one perfect
		// `severe | file:line | claim | why` line became two bare strings, claim and why, and two
		// mimo-9b seats' five lines became ten, with no re-issue involved); and a mimo-9b seat that
		// repeated a line was read by the loop as a cut final and re-issued with "return the
		// same JSON object that was asked for", so it invented a JSON object per finding
		// that the re-pack then kept one string of, its claim. Either way every finding reached
		// ParseFindings with no severity, file or why, and was hollow. The seat's own answer
		// held them all. So the answer is read too (reviewlane.RawLines: pipe lines, or the
		// JSON objects a seat falls back to), and whichever reading keeps MORE findings through
		// the same filters is the one published; a tie keeps the re-pack, which is what every
		// run that worked has always shown (it also strips a preamble, where the raw reading
		// would count it as a hollow line).
		if raw := reviewlane.Report(reviewlane.RawLines(wire.Output), diff, maxFindings); raw.Survivors() > rep.Survivors() {
			rep, salvaged = raw, reviewlane.SalvagedRepackFlattened
		}
	}
	// THE GATE. A zero-finding result is only published as a clean review when the seat's
	// OWN raw answer says so. A structurally valid but UNEARNED empty array is reachable
	// and indistinguishable from a real clean review at every other field. Until 0.115.8
	// agent/loop.go returned stop_reason "done" as soon as the model stopped requesting
	// tools, with no check that the final message carried content, and agenttask.go
	// special-cased only "budget", so "done" with an empty Output reached the re-pack,
	// which extracted findings from an empty string and returned a schema-valid
	// {"findings":[]}. Since 0.115.8 an empty final is a NAMED stop (reasoning_starved /
	// empty) that agenttask.go defers before any re-pack, so that path is closed at the
	// source; this gate stays for the shape it can still see — a seat that produced TEXT
	// which re-packed to nothing — and is unchanged.
	// wire.Output is the ONE field that differs, and it was previously read by the re-pack
	// and then discarded. So it is read here, once, for the explicit NONE verdict the prompt
	// already asks for — never as a judgement about the answer's quality.
	//
	// It fires WHATEVER the filters dropped (0.178.0). It used to stand down when anything had
	// been dropped, on the reasoning that a run which produced text is not the broken-run
	// failure; but producing text is exactly what a hollow or invented review does. On
	// 2026-10-09 a small seat answered a 228-line diff with four claim-only lines, every one
	// dropped, and the caller got a success carrying a note: a hollow review published as a
	// clean one, in a lane whose contract is that a broken run never looks like a clean diff.
	// A list emptied by filtering is not a review any more than a list the seat left empty, so
	// both face the same test, and only the seat's own explicit NONE earns an empty list.
	//
	// An answer the completion budget cut can never read as clean, whatever words it holds: the
	// seat was stopped before it finished, so a "no issues" inside it is not an all-clear.
	readsClean := reviewlane.VerdictReadsClean(wire.Output) && salvaged != reviewlane.SalvagedOutputTruncated
	if len(rep.Findings) == 0 && !readsClean {
		if salvaged != "" && rep.Filtered() == 0 {
			// Nothing in the salvaged text was even finding-shaped: there is nothing to add
			// to the node's own deferral, which says what happened to the answer.
			return bareDefer()
		}
		reason := unearnedReason(rep)
		if salvaged != "" {
			// The caller loses nothing it used to be told: the drops AND why the answer
			// reached the lane unstructured.
			reason = "the seat's answer reached the lane unstructured (" + wire.Reason + "); read from its raw lines, " + reason
		}
		return withReviewExtra(withReviewCounts(map[string]any{
			"deferred":    true,
			"reason":      reason,
			"defer_class": core.DeferClassAbstention,
			"seat":        wire.Seat,
			"steps":       wire.Steps,
			"stop_reason": wire.StopReason,
		}, rep), extra), false
	}
	findings := rep.Findings
	if findings == nil {
		// [] rather than null: "no findings" is a real answer here, and a JSON
		// null would read to a caller as a missing field.
		findings = []reviewlane.Finding{}
	}
	out = map[string]any{
		"findings":       findings,
		"reviewed_bytes": len(diff),
		"seat":           wire.Seat,
		"steps":          wire.Steps,
		"stop_reason":    wire.StopReason,
	}
	withReviewCounts(out, rep)
	var notes []string
	if salvaged != "" {
		out["salvaged"] = salvaged
		notes = append(notes, salvageNote(salvaged, wire.StopNote))
	}
	if len(findings) == 0 {
		// Said in words, because this is the result most easily misread. Which words
		// depends on WHY the list is empty: "found nothing" beside a non-zero drop count
		// is simply false — the reviewer found things and the harness discarded them. Since
		// 0.178.0 that combination is reachable only when the seat's own answer read as a
		// clean NONE verdict yet it also wrote lines that were all dropped (a self-
		// contradicting answer, or a re-pack that invented a line the answer never held);
		// every other emptied list defers above.
		if rep.DroppedUngrounded > 0 || rep.DroppedEcho > 0 || rep.DroppedHollow > 0 || rep.DroppedDuplicate > 0 {
			notes = append(notes, "this reviewer's answer read as a clean NONE verdict, yet it also wrote findings and NONE survived filtering — they named files the diff does not touch, echoed the prompt's own template back, or carried no severity, file or why. That is a signal about the reviewer, not about the diff: nothing here says the change is correct, and nothing here says it is wrong")
		} else {
			notes = append(notes, "this reviewer found nothing in the diff — that is not a verification that the change works, which stays yours")
		}
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return withReviewExtra(out, extra), true
}

// salvageNote says, in words, what a salvaged review is. The flag beside it is the
// machine-readable half; this is the half a reader of the list cannot miss. stopNote is the
// node's own evidence for a cut (a repetition loop reads as a cut with finish "stop", and
// naming it keeps "cut at the completion budget" from being the only explanation on offer).
func salvageNote(kind, stopNote string) string {
	switch kind {
	case reviewlane.SalvagedOutputTruncated:
		why := ""
		if stopNote != "" {
			why = " (" + stopNote + ")"
		}
		return "the seat's final answer was cut at the completion budget" + why + ", so the structured re-pack was skipped and these findings were read from the complete lines of its raw answer (the line cut mid-way was dropped). The list may be incomplete: whatever the seat had not yet written is unreviewed."
	case reviewlane.SalvagedRepackFlattened:
		return "the structured re-pack flattened the seat's findings (a bare sentence apiece, or one line split into its fields, with the severity, file, line and why gone), so these findings were read from the seat's own answer instead, which kept them. They went through the same filters as any other result."
	}
	return "the wall had no time left for the structured re-pack, so these findings were read straight from the lines of the seat's finished answer, which is complete. They went through the same filters as any other result."
}

// withReviewCounts publishes a review's counts on the same terms for every payload that
// carries them: present when non-zero, absent when not. Surfacing one and swallowing the
// others was an asymmetry with no justification — "we found more than we are showing you"
// is one situation, and truncating silently while counting drops loudly just moved the
// blind spot. A deferral carries them too (0.178.0): when filtering emptied the list the
// caller is owed WHY, and the counts are the evidence the reason names.
func withReviewCounts(out map[string]any, rep reviewlane.Result) map[string]any {
	if rep.DroppedUngrounded > 0 {
		out["dropped_ungrounded"] = rep.DroppedUngrounded
	}
	if rep.DroppedEcho > 0 {
		out["dropped_echo"] = rep.DroppedEcho
	}
	if rep.DroppedHollow > 0 {
		out["dropped_hollow"] = rep.DroppedHollow
	}
	if rep.DroppedDuplicate > 0 {
		out["dropped_duplicate"] = rep.DroppedDuplicate
	}
	if rep.TruncatedByCap > 0 {
		out["truncated_by_cap"] = rep.TruncatedByCap
	}
	return out
}

// unearnedReason words the deferral of a review that came to no finding and no clean
// verdict. With nothing dropped, the seat wrote nothing usable (the broken-run shape the
// gate was built for). With drops, it wrote finding-shaped lines and the lane's filters took
// every one: the reason says how many and which filter, so the caller can tell a hollow
// review (no severity, file or why) from an invented one (files the diff never touched)
// before it goes to review the diff itself. Only hollow, ungrounded and echo drops can
// empty a list — a duplicate keeps one survivor and the cap keeps at least one.
func unearnedReason(rep reviewlane.Result) string {
	var parts []string
	if rep.DroppedHollow > 0 {
		parts = append(parts, fmt.Sprintf("%d had no severity, file or why", rep.DroppedHollow))
	}
	if rep.DroppedUngrounded > 0 {
		parts = append(parts, fmt.Sprintf("%d named files the diff never touched", rep.DroppedUngrounded))
	}
	if rep.DroppedEcho > 0 {
		parts = append(parts, fmt.Sprintf("%d handed the prompt's own template back", rep.DroppedEcho))
	}
	if len(parts) == 0 {
		return "the seat produced no findings and its raw answer did not read as a clean NONE verdict — likely a broken run, not a clean diff; review it yourself"
	}
	return fmt.Sprintf("none of the seat's %d findings survived the lane's filters (%s) and its raw answer did not read as a clean NONE verdict — a hollow or invented review, not a clean diff; review it yourself",
		rep.Filtered(), strings.Join(parts, "; "))
}
