// run_retry.go - Retry policy: floors, retryable shapes and the alternative-node search for a second attempt.

package delegate

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// executionBudgetSec is the delegator's execution budget for a contract: its
// timeout_sec, or the wire CAP for a contract the caller left unsized
// (timeout_auto, register D-03). The executing node sizes that wall from its
// seat's measured rate anywhere inside the cap, so the delegator's clock — the
// poll deadline, the re-placement ledger and a retry's remainder — must hold
// the cap open rather than cut a 700 s wall the node chose at the 300 s default
// the wire happens to carry. A retry never carries the marker: its wall is what
// is left, an explicit number.
//
// The accepted cost (review, 2026-09-16): a node that acks and then dies
// silently is abandoned at the CAP on this path, 900 s instead of the 300 s the
// default gave — the delegator cannot tell "a slow seat under a properly sized
// wall" from "a dead node" without waiting. Bounded, rare, and written down;
// the tighter bound is the node's advertised seat_rate (health), the same
// arithmetic the retry floor already reads — a follow-up, not this change.
func executionBudgetSec(c core.AgentContract) int {
	switch {
	case c.TimeoutAuto:
		return core.AgentTimeoutSecCap
	case c.TimeoutSec <= 0:
		return core.AgentTimeoutSecDefault
	}
	return c.TimeoutSec
}

// retryFloorSec is the least remaining budget a verification retry starts
// with: the historical 10 s (minRetrySec), raised by config `agent_retry_min_sec`.
func (r *runner) retryFloorSec() int {
	if r.cfg.AgentRetryMinSec > minRetrySec {
		return r.cfg.AgentRetryMinSec
	}
	return minRetrySec
}

// noRetryNodeWhy is the words for a retry that found no node to go to.
func (r *runner) noRetryNodeWhy() string {
	why := "every eligible node was already tried or is not eligible"
	switch {
	case r.route == "local":
		why = "route local places nothing on another node"
	case r.retryOffLocal():
		why += "; " + r.retryOffLocalWhy()
	}
	return why
}

// firstPassRetryFloor is the floor a verification retry is judged against BEFORE a retry node is chosen: the configured one
// (agent_retry_min_sec, else the 10 s minimum) and nothing about the first attempt's seat. retryFloorOn applies the chosen
// node's own floor.
func (r *runner) firstPassRetryFloor() (int, string) {
	floor := r.retryFloorSec()
	return floor, retryFloorSource(floor)
}

// retryFloorFor (0.115.21, register D-03) is the seat's OWN floor when the
// first attempt published one — min_turn_sec: its cold load plus one turn at
// the final budget at its measured rate — and the configured floor otherwise.
// The larger wins: a box constant sized for the reference seat can sit under
// what a slower seat just measured for itself.
//
// A seat-down defer (ADR 0066) is the exception: its min_turn_sec is the DEAD
// seat's own (its cold load plus a turn at its measured rate), and the retry goes
// to another node, whose own floor retryFloorOn applies once it is chosen. Read
// here it refused the re-placement of the very defer it exists for — a flagship
// that honestly records a ~270 s cold load publishes a ~390 s min_turn, above
// any default contract — although the seat that is about to run the retry has
// nothing to do with it.
//
// A lost-root defer (register C-79) is the second: the seat served the run, but the fault
// is that run's directory, and the retry goes to another node, so the number read here
// would be a seat the retry never uses. The configured floor stands and the retry seat's
// own floor applies once it is chosen.
//
// The forced-final tool-call defer is the third, for the same reason as the first: the
// seat is the fault, and the retry goes to a node that is not it. A retry node that
// publishes no rate falls back to this function (retryFloorOn), so without the exemption
// the number of the seat that wrote the tool call refused a retry that exists because of
// that seat.
func (r *runner) retryFloorFor(first PlacedResult) (int, string) {
	floor := r.retryFloorSec()
	if m := first.Result.MinTurnSec; m > floor && !producedNothingDefer(first.Result) {
		return m, fmt.Sprintf(", min_turn_sec of the seat on %s", nodeLabel(first))
	}
	return floor, retryFloorSource(floor)
}

// retryFloorOn (0.117.2, register D-46) is the floor of the seat the retry
// LANDS on: the configured floor raised by that seat's own min_turn — its
// remembered cold load plus one final turn at its measured rate, plus the
// re-pack term when the contract carries an output_schema. A remote node's
// numbers come from its health (`seat_rate`, published since 0.117.2); the
// local seat's from this box's seat-rates store. A seat with no numbers falls
// back to the first attempt's min_turn (retryFloorFor), and the note says so.
func (r *runner) retryFloorOn(first PlacedResult, alt placement, contract core.AgentContract) (int, string) {
	floor := r.retryFloorSec()
	repack := 0
	var minTurn int
	var src string
	switch {
	case alt.base != "":
		if sr := alt.view.SeatRate; sr != nil && sr.TokS > 0 {
			final := sr.MinTurnSec
			if b := alt.view.SeatBudget; b != nil {
				if len(contract.OutputSchema) > 0 {
					repack = b.FinalTokens
				}
				minTurn = seatrate.MinTurnFor(sr.ColdLoadSec, b.FinalTokens, repack, sr.TokS)
			} else {
				// A node publishes seat_rate and seat_budget together (one gate
				// on the health handler), so this branch waits for a future
				// node that publishes the rate alone: its own floor, no re-pack.
				minTurn = final
			}
			src = fmt.Sprintf(", min_turn_sec published by %s", alt.view.NodeID)
		}
	default:
		if known, ok := r.localSeatRate(); ok {
			step := r.cfg.AgentMaxTokens
			if step <= 0 {
				step = 1024
			}
			final := seatrate.FinalBudgetFor(step)
			if len(contract.OutputSchema) > 0 {
				repack = final
			}
			minTurn = seatrate.MinTurnFor(known.ColdLoadSec, final, repack, known.TokS)
			src = ", min_turn_sec of the local seat (seat-rates store)"
		}
	}
	if minTurn <= 0 {
		f, s := r.retryFloorFor(first)
		if s != "" && strings.Contains(s, "min_turn_sec") {
			s += " (the retry seat published no rate)"
		}
		return f, s
	}
	if minTurn > floor {
		return minTurn, src
	}
	return floor, retryFloorSource(floor)
}

// localSeatRate reads this box's remembered rate for the local agent seat.
func (r *runner) localSeatRate() (seatrate.Seat, bool) {
	root, err := gpulease.ResolveStateRoot(r.cfg.StateDir)
	if err != nil {
		return seatrate.Seat{}, false
	}
	store, _ := seatrate.Load(seatrate.Path(root))
	known := store.Get(strings.TrimSpace(r.cfg.AgentPlannerModel("")))
	return known, known.TokS > 0
}

// retryFloorSource names, for the retry note, where a raised floor came from.
func retryFloorSource(floor int) string {
	if floor > minRetrySec {
		return ", agent_retry_min_sec"
	}
	return ""
}

// retrySeatBusy reports whether the seat the retry would land on is already
// generating for another job. A LOCAL landing reads the seat's in-flight count
// through llama-swap (probeLocalBusy, fail-open to idle); a REMOTE landing asks
// the node's OWN CEILING from a FRESH health probe. Fresh, not the run's cached
// view: route=spread probes the fleet once before the batch starts, so the
// cached view cannot show the load a SIBLING subtask of the same batch has
// since put on that node — which is exactly how the 2026-09-10 retry joined a
// seat mid-generation. A failed probe falls back to the cached view (fail-open,
// like every placement read).
//
// The remote predicate is !provablyStartsNow(view), the same ceiling-aware rule
// gate.go ranks placements with — not `JobsRunning > 0`. Register D-46 shipped
// the RULE and not the threshold, and 0 is the wrong threshold on every node
// that runs more than one worker: a four-worker box with ONE job in flight
// refused every cross-seat retry although three workers were idle. "Busy" here
// means "the retry would queue", which is precisely what provablyStartsNow
// answers — and it answers it conservatively, so an unknown ceiling still reads
// as busy exactly where it used to.
func (r *runner) retrySeatBusy(ctx context.Context, alt placement) (bool, string) {
	if alt.base == "" {
		rd := r.probeLocalBusy(ctx)
		if rd.busy {
			if rd.occupiedBy != "" {
				return true, rd.why()
			}
			if rd.inflight == 0 {
				return true, rd.note // a load in progress: the count is unknown, the note says why
			}
			return true, fmt.Sprintf("%d in flight on the local seat", rd.inflight)
		}
		return false, ""
	}
	view, source := alt.view, "cached view"
	if fresh, err := FetchNodeView(ctx, alt.base, r.cfg.FleetAuthToken); err == nil {
		view, source = fresh, "fresh health"
	} else if ctx.Err() == nil {
		// The cached view is the round's own snapshot and can be minutes old: a wait of up
		// to agent_placement_wait_sec against a node that stopped answering must not look
		// like a wait against a node that kept saying "busy". Named in the note, and logged
		// once per node per run.
		source = "cached view; fresh health could not be read: " + err.Error()
		if _, warned := r.probeWarned.LoadOrStore("retry-seat:"+alt.base, struct{}{}); !warned {
			log.Printf("delegate: retry seat %s: fresh health could not be read (%v); judging it from the cached view", alt.base, err)
		}
	}
	if !provablyStartsNow(view) {
		return true, seatBusyNote(view, source)
	}
	return false, ""
}

// seatBusyNote renders WHY the retry seat would queue, from the node's own
// published numbers. Two shapes because a ceiling of 0 is UNKNOWN, never a
// limit: printing "jobs_running 1 of max_concurrent_jobs 0" would state a
// capacity the node never advertised.
func seatBusyNote(v NodeView, source string) string {
	if v.MaxConcurrentJobs > 0 {
		return fmt.Sprintf("jobs_running %d of max_concurrent_jobs %d, jobs_queued %d (%s)",
			v.JobsRunning, v.MaxConcurrentJobs, v.JobsQueued, source)
	}
	return fmt.Sprintf("jobs_running %d, queue_depth %d, no published ceiling (%s)",
		v.JobsRunning, v.QueueDepth, source)
}

// minRetrySec is the least timeout_sec budget a retry is worth starting with: a
// cold seat needs seconds to load and a contract that cannot finish in this
// would only add a budget defer on top of the verified failure.
const minRetrySec = 10

// retryable: the node answered and the ANSWER was the problem — a verified
// wrong result, or a seat honestly abstaining. A transport failure, a budget
// defer, or a broken/misconfigured stack is not something another seat fixes,
// and a contract-classed defer is the caller's to fix.
//
// The TWO infrastructure defers that ARE retryable at admission are the
// admission-time ones: the coherence defer (register D-118) and the warm-up defer
// (register C-76, R-05a). The seat itself is broken (it answers nonsense, or its
// process did not start) and it was caught before the contract's wall started, so
// the budget is still there to fund a retry — which is precisely the case another
// node fixes. What the node's admission spent getting there is credited back in
// runOne (admissionCredit), because the cold load that leads to either verdict
// would otherwise eat most of a default budget before the retry floor is
// applied. The general infrastructure rule is untouched; see IncoherentSeatDefer
// and SeatWarmFailedDefer.
//
// The third is the seat-down defer (ADR 0066, SeatDownDefer): the seat's engine
// went down under the run and did not come back inside the node's own bounded
// wait. Same argument — a property of THIS seat, a sound contract, the cure is
// another node — and the wait the node spent on the dead seat is credited back
// the same way, with the wall of a delegator-side rescue that failed first (a seat
// lost in the structured re-pack is rescued before it is re-placed).
//
// The fourth is the lost-root defer (register C-79, RootGoneDefer): the run's own
// workspace root, which the executing node materialized for it, could not be
// opened, so the seat never read its documents. The fault is that run's
// directory, not the seat and not the contract, and the same contract placed
// again materializes a root of its own. Before the node filed it as
// infrastructure the seat's "I could not read the file" was a failed
// verification, which this function retries: the better class must not take the
// recovery with it.
//
// The fifth is the forced-final tool-call defer (FinalToolCallDefer): the seat answered the tool-less final step with a
// tool call, even after the loop re-asked it once. A property of THIS seat, a sound contract that had read what it
// needed, and a class (`budget`) this function otherwise leaves alone.
func retryable(pr PlacedResult) bool {
	if pr.Err != "" {
		return false
	}
	if len(pr.AcceptanceFailures) > 0 {
		return true
	}
	if admissionDefer(pr.Result) || SeatDownDefer(pr.Result) {
		return true
	}
	if RootGoneDefer(pr.Result) || FinalToolCallDefer(pr.Result) {
		return true
	}
	return pr.Result.Deferred && pr.Result.DeferClass == core.DeferClassAbstention
}

// FinalToolCallDefer reports whether a result is the forced-final tool-call defer: a `budget` defer whose reason
// carries core.ForcedFinalToolCallReason, i.e. the run reached its last step, which offers no tools and asks for the
// answer, and the seat wrote a tool call instead, again after the loop re-asked it once with the offence quoted.
//
// It is the fifth defer worth a second placement, and for the same reason as the seat-down one: the fault is a
// property of THIS seat (one seat family did it in 35 of 106 forced finals; the seats of every other family in none of
// 241) and the contract is sound, so the same contract on another node is the cure. Unlike a plain step-budget defer
// (a task too large, which another seat does not fix), the run had read what it needed and its wall was mostly
// unspent. It is a node-filed defer, an OBSERVED terminal, so nothing still runs the contract. It matches on the
// CONSTANT the loop writes (the phrase sits behind the "step budget exhausted (N steps): " grep key, so Contains and
// not HasPrefix), never on other prose.
func FinalToolCallDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassBudget && strings.Contains(r.Reason, core.ForcedFinalToolCallReason)
}

// producedNothingDefer reports whether a result is the defer of a run that delivered no answer at all, so that its second
// placement is the job's only way to be delivered and not an optional second opinion on an answer the subtask already
// holds: the seat-down defer (ADR 0066), the lost-root defer (register C-79) and the forced-final tool-call defer. Such a
// retry is placed the way the seat-down one always was (ADR 0066 decision 3): the busy check of the retry seat neither waits
// nor skips (a skipped retry is a lost job, and the node's own queue is the line), the failed seat's own min_turn_sec is not
// the retry seat's floor, and a local line with no free slot is left for an untried remote.
func producedNothingDefer(r core.AgentWireResult) bool {
	return SeatDownDefer(r) || RootGoneDefer(r) || FinalToolCallDefer(r)
}

// SeatDownDefer reports whether a result is the seat-down defer (ADR 0066,
// register C-72): an `infrastructure` defer whose reason carries
// core.SeatDownReason, i.e. the executing node's seat went down under the run —
// the engine hung with work outstanding, or died and llama-swap no longer serves
// it — and the node's own bounded wait and one re-issue did not bring it back.
//
// It is the THIRD infrastructure defer that is worth a retry, after the two
// admission-time ones (IncoherentSeatDefer, SeatWarmFailedDefer), and for the same
// reason: the fault is a property of THIS seat and the contract itself is sound,
// so the same contract on another node is the cure. It is safe to re-place
// because a node-filed defer is an OBSERVED terminal — the node reported it, so
// nothing is still running the contract (the "never re-place after a 202" rule is
// about jobs whose outcome nobody observed). It matches on the CONSTANT the
// producer writes, never on prose. A node without ADR 0066 never emits it, and
// keeps emitting `stalled:` for the same outage until it is upgraded — for such a
// node this changes nothing.
func SeatDownDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.SeatDownReason)
}

// skipsRetryAsResearchAcceptanceOnly reports a placed result the verification
// retry must leave alone: a research page whose only problem is its own checks
// (a shape, an item count). Those are graded against the fetched page, so a
// second node given the same page mostly repeats the verdict; before the anchor
// fix of 0.141.1, 209 of a week's 271 such failures were the checks' own faults,
// and each retry was a whole second run of five to thirteen minutes.
//
// A failed DOCUMENT FINGERPRINT is the exception: the answer is about another
// document, which is a fact about the node that wrote it (strikeOnFingerprint
// quarantines a node for it), and another node is the cure. An abstention, an
// admission-time defer (coherence, warm-up) and the seat-down defer are likewise
// facts about the SEAT and keep their retry. A fingerprint the contract itself can
// never close (an unknown the intake lint warns about: fingerprintFailedByNode)
// is no such fact, and another node would grade the same unknown, so it is skipped.
func skipsRetryAsResearchAcceptanceOnly(contract core.AgentContract, pr PlacedResult) bool {
	return researchDoors[contract.Door] && pr.Err == "" && !pr.Result.Deferred &&
		len(pr.AcceptanceFailures) > 0 && !fingerprintFailedByNode(contract, pr.Result, pr.AcceptanceFailures)
}

// failsDocumentFingerprint reports whether any failed acceptance is the DOCUMENT
// FINGERPRINT (research.AnchorCheck tags its regex with the named group
// `docanchor`).
func failsDocumentFingerprint(failures []string) bool {
	for _, f := range failures {
		if strings.Contains(f, docAnchorTag) {
			return true
		}
	}
	return false
}

// IncoherentSeatDefer reports whether a result is the admission-time coherence
// defer: an `infrastructure` defer whose reason carries core.IncoherentSeatReason,
// i.e. the executing node asked its freshly loaded seat one bounded question
// and the seat answered with the NaN shape (a run of one repeated byte, an
// unparsable tool call, or nothing at all at the cap with no hidden reasoning
// reported), or the same verdict remembered about a seat it already caught.
//
// It matches on the CONSTANT the producer writes, never on prose: pipeline
// (and the MCP door) build the reason from core.IncoherentSeatReason, so the
// two sides cannot drift. A pre-D-118 node never emits it and is unaffected.
//
// Why it is worth a retry when no other infrastructure defer is: the contract
// spent seconds, not its wall, and the fault is a property of THIS seat — the
// same contract on another node is the cure, and re-placing it is the whole
// reason the probe fires before the wall instead of after it.
func IncoherentSeatDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.IncoherentSeatReason)
}

// SeatWarmFailedDefer reports whether a result is the admission-time warm-up
// defer: an `infrastructure` defer whose reason carries core.SeatWarmFailedReason,
// i.e. the executing node's warm request was refused with a server error and there
// was positive evidence the seat's process did not start (a busy answer, a seat
// that reads starting or another model mid-swap never produce it). Like the
// coherence defer it is a property of THIS seat, was caught before the wall
// started, and is cured by the same contract on another node.
func SeatWarmFailedDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.SeatWarmFailedReason)
}

// admissionDefer is either of the two defers the node files before its wall
// starts, on evidence about its own seat, and that another node may cure.
func admissionDefer(r core.AgentWireResult) bool {
	return IncoherentSeatDefer(r) || SeatWarmFailedDefer(r)
}

// admissionCredit is the node-side admission time an admission defer already
// spent — its cordon wait, pre-flight, cold load and the probe or warm-up itself
// — which the subtask's execution budget must not be charged for: the contract's
// wall never started, and the wire carries admission_wait_sec for exactly this.
// Zero for every other result: no other shape has a claim on the credit, and a
// node that reports no admission (a pre-D-118 node, or an unmeasured one) is
// credited nothing rather than guessed at.
//
// A seat-down defer (ADR 0066) is credited its admission, the wait the node
// spent on the downed seat (seat_down_wait_sec) AND the wall a failed
// delegator-side rescue spent (rescueSpent): time the contract provably did not
// spend working, on a seat that could not serve it or on this box's attempt to
// save a finished answer. The last term is the one a seat lost in the structured
// re-pack depends on: the loop's recovery does not cover the re-pack, so that
// defer reports no wait of its own, and the rescue that runs first on this box's
// clock may cold-load its own seat and run a completion to its allowance —
// minutes that, charged to the retry budget, made the retry floor refuse the
// re-placement the defer is promised. The credit is bounded by one full contract
// wall (core.AgentTimeoutSecCap): neither a node's number nor a slow rescue buys
// the retry more than that.
//
// A lost-root defer (register C-79) is credited its admission, bounded the same way, and
// nothing else: the node's cold load happened before the contract's wall began, so the
// retry's budget must not be charged for it, while the wall the run spent working before
// its directory went was work (the fault is the box's, but that time is the contract's).
func admissionCredit(pr PlacedResult) time.Duration {
	secs := 0.0
	switch {
	case admissionDefer(pr.Result):
		secs = pr.Result.AdmissionWaitSec
	case RootGoneDefer(pr.Result):
		secs = pr.Result.AdmissionWaitSec
		if secs > core.AgentTimeoutSecCap {
			secs = core.AgentTimeoutSecCap
		}
	case SeatDownDefer(pr.Result):
		secs = pr.Result.AdmissionWaitSec + pr.Result.SeatDownWaitSec + pr.rescueSpent.Seconds()
		if secs > core.AgentTimeoutSecCap {
			secs = core.AgentTimeoutSecCap
		}
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// retryFenceWhy is why a retry is not placed on a fenced local seat: dialling it would hold the
// retry at the model-affinity cordon for agent_lease_wait_sec, and it would end as a capacity
// defer anyway (D-94). It is a fact about the seat, read from the lease record, so it stays true
// when the call's deadline ended the read of the fleet.
const retryFenceWhy = "a retry placed there would wait out agent_lease_wait_sec at the affinity cordon and defer as capacity anyway"

// localReservedPrefix opens the clause alternativeNode returns for a local seat it left out of a
// retry because a text lease RESERVES it (register C-81), as "the local seat is fenced (...)" does
// for a fence. retryHeldWhy tells the two apart by this opening: the one coupling between the clause
// and the closing reason of the note, and a string match on purpose, because alternativeNode returns
// the clause as text and a typed return would reach past its helpers. The opening is edited here,
// where reservedClause and retryHeldWhy both read it, and TestRetryHeldWhyTellsAReservationFromAFence
// fails with this coupling named when a clause that reservedClause writes stops being told as one.
const localReservedPrefix = "the local seat is reserved ("

// reservedClause names a reservation for a retry in the sentence replacementNode ends a refused
// re-placement with ("..., and the local seat is reserved (...)"), which replacementNode spells out
// itself; TestAReservedSeatReadsTheSameInARefusedReplacementAndASkippedRetry keeps the two copies
// identical. The first placement's own reasons ("local seat reserved (...); no eligible remote - ...")
// word a reservation differently and are not this clause.
func reservedClause(info gpulease.Info) string {
	return localReservedPrefix + HolderLine(info) + ")"
}

// retryReservedWhy is why a retry is not placed on a reserved local seat. It is not the fence's
// reason: the affinity gate admits the load of a plain reservation, so a dial would not wait at the
// cordon, it would run, on the cards the holder reserved. It says that and nothing wider: the note
// reaches callers, and "a reservation keeps every placement off them" is false for route=local (the
// caller's explicit choice, never gated) and for the holder's own child (the inherited lease exempts
// it), so the words stay about this retry.
const retryReservedWhy = "a retry placed there would run on the cards the holder reserved"

// retryHeldWhy closes the note of a retry that was not placed because a lease holds the local seat:
// the reservation's reason for the clause reservedClause writes, and the fence's for any other.
func retryHeldWhy(clause string) string {
	if strings.HasPrefix(clause, localReservedPrefix) {
		return retryReservedWhy
	}
	return retryFenceWhy
}

// alternativeNode picks the node a retry runs on: the best eligible remote
// when the first attempt ran locally (probing the fleet now if this run has
// not yet), the local seat when it ran remotely, unless a lease keeps the retry off the
// seat, a fence (D-94) or a text reservation (C-81), when the best untried remote takes
// it instead. ok=false when no different node can take the contract; the string is then
// the clause naming the lease ("the local seat is fenced (...)" or "the local seat is
// reserved (...)") when the local seat was left out for it, "" otherwise. runOne words
// the note around it (retryHeldWhy).
//
// Note the asymmetry and its cost: recovering a wrong REMOTE answer puts the
// work back on the local box the harness exists to keep free — so a placement
// the fit score reads too cheap is paid for in local GPU time, not just in
// latency. That is accepted deliberately (local is the one seat always able to
// take the contract, and a second remote hop would need a fresh gate pass
// mid-timeout), but it is a real cost, not a free retry.
//
// The one exception is a remote call whose retry is kept off the local seat (retryOffLocal). A
// REASONED remote pin (remotePinned, ADR 0078 decision 3) is one: the caller said why this call
// runs on a fleet node, and the ledger rows and offload_status say placement obeyed it, so its
// second chance is another fleet node or none, never the local seat. RunOptions.RemoteRetryOnly is
// the other, for a caller that has no reason to give and still must not get the work back on the
// box it moved it from. The bare remote route of any other caller keeps the asymmetry above.
//
// It consults the SUBTASK's placement ledger, which is what makes "a DIFFERENT
// node" true rather than merely intended: a seat the first attempt already RAN
// on — including one it reached by re-placement, and including the local seat — is
// excluded here. Without that, a first attempt that ended up local after two
// remotes refused could be "retried" on local again. A seat that only DECLINED the
// subtask (the local seat's capacity defer, ADR 0063 decision 3) took nothing and is
// not excluded: see the local branch below.
func (r *runner) alternativeNode(ctx context.Context, first PlacedResult, contract core.AgentContract, pl *placements) (placement, string, bool) {
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()
	why := attemptOutcome(first)
	if !first.ranLocal {
		// A reasoned remote pin never retries on the local seat (ADR 0078 decision 3), and neither does a remote call that
		// asked for its retry to stay on a fleet node (RunOptions.RemoteRetryOnly). Checked before the layer, fence and
		// reservation questions, which are all about a seat this call may not use: the retry is the best untried remote, and
		// with none there is no retry (runOne words the note). Keyed on those two and not on r.route == "remote": the
		// bare remote route keeps remote -> local (retry_reserved_test.go, TestRunRetryRemoteFailureFallsBackToLocal).
		if r.retryOffLocal() {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why + " — " + r.retryOffLocalWhy()}, "", true
			}
			return placement{}, "", false
		}
		// A contract that names a layer this box does not declare (register A-108) has no local retry:
		// the seat would only defer it by name. A different node that declares the layer is the retry,
		// and with none there is no retry.
		if !r.localServesLayer(localView, contract) {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				because := " — the local seat declares no layer " + contract.Layer
				if seatless(localView) {
					because = " — " + noLocalSeat
				}
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why + because}, "", true
			}
			return placement{}, "", false
		}
		// D-94: the local seat is only a retry target if its lease would ADMIT
		// the run. Read fresh — the deal's snapshot can be minutes old by the
		// time a first attempt has failed somewhere else — and read the VERDICT
		// rather than dialling: a fenced seat answers a dial by holding the
		// request at the affinity cordon for agent_lease_wait_sec and then
		// deferring as capacity, which is five minutes of a retry's budget spent
		// discovering something the lease record already said.
		lease := r.localLease(contract)
		if fenced, fence := Fenced(lease); fenced {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
						" — the local seat is fenced: " + fence}, "", true
			}
			return placement{}, fmt.Sprintf("the local seat is fenced (%s — %s)", fence, HolderLine(lease)), false
		}
		// C-81: a plain text reservation does not FENCE (the affinity gate admits the load, which is
		// why Fenced above leaves it alone), but it RESERVES the cards. A first placement on route auto
		// or spread never takes the local seat under one (auto waits for the holder, spread deals
		// without the seat) and neither does replacementNode's last resort, and the forced local
		// placement below is never read against the lease again, since attempt() runs a forced
		// placement as given. So without this check the retry of an attempt that ran on a fleet node,
		// a verification retry or a seat-down re-issue alike, was dialled onto cards a measurement had
		// reserved. The reading is replacementNode's: Reserved, which exempts the holder's own child.
		// An untried remote is the retry's place; with none the retry is not placed, as for a fence,
		// and the note names the holder (replacementNode's refusal for a reserved seat is the same
		// sentence, and its caller then waits in the capacity wait, which a retry does not have).
		if Reserved(lease) {
			held := reservedClause(lease)
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
						" — " + held}, "", true
			}
			return placement{}, held, false
		}
		// A defer that produced nothing (producedNothingDefer) is not held for a busy retry seat
		// (runOne, ADR 0066 decision 3): the node's own queue is the line, and its 503 is re-placed at once. That is
		// true of a REMOTE alternative and false of the local seat, which answers no 503:
		// a run forced onto it joins the run-cap line and waits there for the run's whole
		// wall before it defers as capacity, a line the subtask could not leave (the
		// state replacementNode declines to re-place into, ADR 0063 decision 2). So while
		// that line has no free slot ahead of a newcomer, an untried remote is the better
		// place for the retry. With none, the local seat is the only place there is, and
		// joining its line beats losing the job.
		if producedNothingDefer(first.Result) {
			if free, note := r.localSlotAhead(); !free {
				if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
					return placement{view: chosen, base: base,
						reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
							" — the local seat's run-cap line has no free slot (" + note + ")"}, "", true
				}
			}
		}
		// No pl.tried[""] check here, on purpose. It used to be stated as a proof:
		// a LOCAL placement is always terminal for its chain (only runRemote sets
		// `refused`), so pl.tried[""] implies first.ranLocal and this branch cannot
		// run with local already used. The proof stopped holding with ADR 0063
		// decision 3: the local seat's own capacity defer is re-placeable
		// (isReplaceable, capacityDeferRefusal), so placeAndRun records the seat as
		// tried, re-places the subtask on a remote, and that remote's failed answer
		// is the first attempt this branch sees. The local seat is then the
		// DIFFERENT node the retry asks for: it declined the job and never ran it,
		// and pl.ran, the exclusion the retry's premise needs, does not hold it.
		//
		// A retry back onto it is wanted, not a leak. A guard on pl.tried[""] would
		// refuse a seat-down defer its only other place, and lose the job, and
		// refuse a verification retry its second opinion. What such a guard is for,
		// not rejoining a line that is still full, is done at the seat the retry
		// lands on: the run-cap line check above for a seat-down defer, and
		// retrySeatBusy and awaitRetrySeat in runOne for a verification retry (by
		// llama-swap's in-flight count, for the local seat).
		// TestARetryMayReturnToALocalSeatThatOnlyCapacityDeferredTheSubtask drives
		// the case. The other direction, a retry's own chain falling back onto a
		// local seat already used, is guarded in replacementNode.
		return placement{view: localView, reason: "retry on local after " + nodeLabel(first) + " " + why}, "", true
	}
	if r.route == "local" {
		return placement{}, "", false
	}
	chosen, base, found := r.remoteAlternative(ctx, st, pl)
	if !found {
		return placement{}, "", false
	}
	return placement{view: chosen, base: base, reason: "retry on " + chosen.NodeID + " after local " + why}, "", true
}

// remoteAlternative picks the best UNTRIED remote for a retry, or reports that
// there is none. It is Place with the local node forced out of contention
// (localBusy=true), so the preference order is the fleet's own — capacity first,
// then a provably free execution slot (an IDLE node beats one that would queue),
// then queue depth, then utilization. Route local has no remotes by definition
// and is handled by the caller.
func (r *runner) remoteAlternative(ctx context.Context, st Subtask, pl *placements) (NodeView, string, bool) {
	if r.route == "local" {
		return NodeView{}, "", false
	}
	// Always read the fleet (the probe memo dedupes siblings): a retry runs after a
	// whole attempt, and route=spread's run-start snapshot is minutes old by then.
	views, bases, _ := r.fetchViews(ctx)
	freshViews, freshBases := untried(views, bases, pl.tried)
	chosen := Place(mintP2CSeed(), st, r.localView(), freshViews, true)
	if chosen.Local {
		return NodeView{}, "", false
	}
	return chosen, baseFor(chosen, freshViews, freshBases), true
}

// nodeLabel names an attempt's node for an operator-facing annotation. An
// attempt NO node took (an exhausted re-placement) has no node to name — say so
// rather than rendering an empty string mid-sentence.
func nodeLabel(pr PlacedResult) string {
	if pr.Node == "" {
		return "(no node took it)"
	}
	return pr.Node
}

// jobLabel names a subtask's job for a log line ("(unnamed)" before it has one).
func jobLabel(pr PlacedResult) string {
	if pr.JobID == "" {
		return "(unnamed)"
	}
	return pr.JobID
}

// attemptOutcome names an attempt's outcome for the retry annotations.
func attemptOutcome(pr PlacedResult) string {
	switch {
	case pr.Err != "":
		return "failed: " + pr.Err
	case pr.Result.Deferred:
		return "deferred (" + pr.Result.DeferClass + "): " + pr.Result.Reason
	case len(pr.AcceptanceFailures) > 0:
		return fmt.Sprintf("failed_verification: %v", pr.AcceptanceFailures)
	}
	return "succeeded"
}

// mergeAttempts publishes the better attempt: a clean second attempt wins
// (and is marked recovered); otherwise the FIRST attempt stands — its
// verified-wrong answer is still the more informative artifact — annotated
// with what the retry did. Both attempts were recorded by finish() already.
//
// The published attempt is not the only one that says what became of the page: the
// per-page cap counts an issue by what a seat that RAN it produced, and a first attempt
// that stands as a seat-down defer was never run by a seat that could tell. The retry's
// own verdict is carried on the published result (retryRanAndFailed) for the cap.
func mergeAttempts(first, second PlacedResult) PlacedResult {
	clean := second.Err == "" && !second.Result.Deferred && len(second.AcceptanceFailures) == 0
	var published PlacedResult
	if clean {
		second.RetriedOn = second.Node
		second.RetryNote = "first attempt on " + nodeLabel(first) + " " + attemptOutcome(first) + "; this result is the retry"
		second.retryRecovered = true
		published = second
	} else {
		first.RetriedOn = second.Node
		first.RetryNote = "retry on " + nodeLabel(second) + " also " + attemptOutcome(second) + "; this result is the first attempt"
		first.retryRanAndFailed = pageIssueFailed(second)
		published = first
	}
	published.retried = true
	return carryReplacements(first, second, published)
}

// carryReplacements folds BOTH attempts' re-placement history onto whichever
// one is published. A node that refused the first attempt still refused it when
// the retry is what gets published, and dropping that would make
// summary.replaced silently under-report a fleet shedding load — the exact
// blindness this release exists to remove.
//
// The note is assembled in CHRONOLOGICAL order — first attempt, then retry —
// independently of which attempt won. The predecessor folded "the loser" onto
// "the winner" and joined with "; then ", so on the losing-retry path it
// rendered the retry's refusals BEFORE the first attempt's under a word that
// asserts the opposite order.
func carryReplacements(first, second, published PlacedResult) PlacedResult {
	total := first.Replacements + second.Replacements
	if total == 0 {
		return published
	}
	published.Replacements = total
	published.ReplacementNote = joinNotes(first.ReplacementNote, second.ReplacementNote)
	return published
}

// joinNotes concatenates two replacement notes in the order given, tolerating
// an empty one on either side.
func joinNotes(earlier, later string) string {
	switch {
	case earlier == "":
		return later
	case later == "":
		return earlier
	}
	return earlier + "; then " + later
}

// baseFor resolves the dial base of a chosen remote view ("" when absent).
// Exact-value comparison, not NodeID: NodeID is neither guaranteed unique nor
// guaranteed present (a node may omit it), and tried is keyed by dial base
// elsewhere in this file — so the only safe match is the full view itself.
// NodeView carries a slice field (ServedModels) since 0.113.0, which == can no
// longer compare, hence reflect.DeepEqual here instead of ==.
func baseFor(chosen NodeView, views []NodeView, bases []string) string {
	for i := range views {
		if reflect.DeepEqual(views[i], chosen) {
			return bases[i]
		}
	}
	return ""
}
