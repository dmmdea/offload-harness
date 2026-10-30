// run_capacity.go - Placement with capacity waiting: placeAndRun and awaitCapacity with their probe-failure notes.

package delegate

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// replacementExhaustedPrefix opens the message for a subtask NO NODE TOOK. It
// is a stable grep key and is deliberately distinct from the two deadline
// sentences 0.100.0 already produces, because they are three different facts
// about three different failures:
//
//	"placement refused"  — every node the delegator was willing to ask said no
//	                       (or could not be reached). No seat ever saw the
//	                       contract, so there is nothing to defer about.
//	"queue deadline"     — ONE node accepted it and never started it.
//	"poll deadline"      — ONE node started it and never finished it.
//
// It is an Err (Summary.Failed, non-zero CLI exit, IsError on the MCP surface)
// and never a defer, for the same reason the queue deadline is: a defer
// manufactures an AgentWireResult shaped like something a SEAT produced, and
// the only class that would fit — `budget` — teaches every consumer that the
// seat needed more time. No seat was ever asked.
const replacementExhaustedPrefix = "placement refused"

// placeAndRun runs ONE placement and, for as long as the node REFUSES the job
// at dispatch, re-places the subtask on another node — other eligible remotes
// first, then the local seat.
//
// WHY ONLY AT DISPATCH, and why that is a safety property rather than a
// convenience: a refusal is a NON-ACK. The node never took ownership, so no
// seat anywhere can be running the contract, and re-placing it cannot produce
// two concurrent runs. Everything that happens after a 202 — a poll 404, a
// queue deadline, a poll deadline — leaves a job the node may still hold, and
// re-placing THOSE would be the delegator arranging a double run. They stay
// exactly as they were.
//
// The one exception is a job the NODE says it never ran (ADR 0064): the queue
// deadline at which the node confirmed it took the job back (DELETE
// /fleet/jobs/{id}), and a poll that reads the node's own `reaped` / `withdrawn` /
// `not started` record. No seat holds such a job and none ever will, so it is filed
// as the capacity refusal it amounts to (refuseAsWithdrawn, refuseAsNeverRan) and
// re-placed like any other, with the time it sat queued credited back. A withdraw
// the node did not confirm changes nothing: the job may still start, so the result
// stays what it was.
//
// The one honest residual: when both dispatch attempts fail at TRANSPORT level
// (status 0), the first POST may have landed and had its ack lost, so the
// abandoned node could still run the contract once. That costs wasted compute
// on a node nobody is polling any more and the alternative is losing the work
// with certainty. It is safe because the fleet agent loop is built with no
// write/run/fetch capability (internal/pipeline/agenttask.go) and the node
// registers no delegate tool (internal/fleetnode/tasks.go) — so a duplicate run
// has nothing to duplicate. Note where that guarantee LIVES, though: in another
// package, and agenttask.go already anticipates a v2 delegate tool. If the
// fleet lane ever gains a mutating tool, this residual stops being free.
//
// WHAT THE BUDGET DOES AND DOES NOT BOUND. Every placement is handed what is
// LEFT of the contract's timeout_sec, re-measured immediately before dispatch,
// so no seat is ever promised execution time the subtask no longer owns. That
// is a claim about EXECUTION, not about end-to-end wall, and the difference is
// real rather than pedantic: two legs of a placement are bounded but are not
// charged to timeout_sec, because neither can be known before it is paid —
//
//	selection  fetchViews probes remotes SEQUENTIALLY at fetchNodeViewTimeout
//	           each. Bounded below by a ctx of whatever the contract has left,
//	           which is what stops a roster of blackholing nodes from spending
//	           a budget the subtask no longer owns.
//	dispatch   up to dispatchAttempts × dispatchRequestTimeout before a
//	           transport verdict.
//
// and 0.100.0's queued-time credit already extends the poll wall past
// timeout_sec by design (bounded by queueBudgetFor). So the wall a subtask can
// consume is timeout_sec + pollGrace + queued credit + this placement overhead,
// all bounded, and the loop CONVERGES because every blocking leg is charged to
// the next measurement and the floor is re-checked immediately before dispatch.
// Anything that says timeout_sec is an end-to-end wall ceiling is wrong; it is
// the execution budget.
func (r *runner) placeAndRun(ctx context.Context, i int, contract core.AgentContract, forced *placement, start time.Time, budget int, pl *placements) PlacedResult {
	// Why an earlier attempt of this subtask waited when nothing refused it belongs to
	// THAT attempt: the verification retry is another placeAndRun over the same ledger.
	pl.waitNote = ""
	pr := r.attempt(ctx, i, contract, forced)
	if pr.waitCapacity {
		// Nothing was dispatched: the only placement was a seat a text lease
		// reserves. The capacity wait owns it from here (0.113.18) — before this
		// the subtask waited on the LOCAL lease alone and then deferred, even
		// when a remote had freed up in the meantime.
		//
		// A DECIDED sentinel (the pair's long seat waiting for a busy agent
		// seat to drain, ADR 0052) carries no refusal: nobody declined the
		// work, so the decision's reason must not be tallied as a replacement.
		// Neither does a gate turn-away or a deal's overflow (pl.waitNote): the
		// node was never asked, so their reason says why the subtask waits and
		// is no refusal, is not counted in Summary.Replaced, and names no node
		// that "refused". A lease sentinel keeps its long-standing convention.
		refusals := []string{pr.pendingReason}
		pl.waitNote = ""
		switch {
		case pr.decided != nil:
			refusals = nil
		case pr.gated || pr.overflow:
			refusals, pl.waitNote = nil, pr.pendingReason
		}
		return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, "")
	}
	// Recorded whatever the outcome — a SUCCESSFUL placement must exclude its
	// own seat from the verification retry just as firmly as a refused one.
	pl.tried[pr.ranBase] = true
	pl.countAttempt(pr)
	if !r.isReplaceable(pr) {
		return pr
	}
	refusals := []string{refusalLine(pr)}
	r.noteRefusal(pl, pr)
	for {
		// What the iteration spends after the last attempt returned (selection, mostly) is
		// the span a closing row for a chain the call deadline ended would measure.
		iterStart := time.Now()
		remaining := pl.remaining(start, budget)
		if remaining < minRetrySec {
			return r.exhaustedSettled(contract, pr, pl, refusals, budgetSpent(remaining, budget, len(refusals)), iterStart)
		}
		// Selection BLOCKS (fetchViews). Bound it by what is actually left, so
		// the probe cannot outlive the budget it is probing on behalf of.
		selCtx, cancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
		// The re-placement will be dispatched with what is LEFT of the budget, so its
		// candidates are judged against that wall - feasibility and the patience the
		// backlog gate holds a node to - not against the contract's original one: a node
		// that fits the whole budget but not the remainder would be handed a job the
		// delegator then gives up on while the node runs it.
		left := contract
		left.TimeoutSec, left.TimeoutAuto = remaining, false
		next, why, ok := r.replacementNode(selCtx, left, pl, len(refusals))
		cancel()
		if !ok {
			// Every node that could take it is FULL, not broken — or the one
			// seat left is reserved by a text lease (whatever the refusals
			// were: a 409 elsewhere does not make the lease less releasable):
			// wait for something to free rather than fail on a snapshot of one
			// minute. A roster that only 404s or is unreachable is exhausted.
			//
			// So is a roster whose only seat left is a lease's when this box declares no layer the
			// contract names (register A-108): the lease frees a seat that could never run it, and the
			// wait keeps that seat out (localCan), so it could place nothing and would spend its whole
			// TTL to end as a capacity defer saying no node had room. `why` already says it.
			reserved := r.route != "remote" && !pl.tried[""] && r.localServesLayer(r.localView(), contract) &&
				Reserved(r.localLease(contract))
			if pl.capacityRefusal || reserved {
				return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
			}
			return r.exhaustedSettled(contract, pr, pl, refusals, why, iterStart)
		}
		// RE-MEASURE. `remaining` above was true when taken and the probe may
		// have consumed most of it; committing the stale number to the wire is
		// exactly how a seat gets handed time the subtask no longer has. The
		// ledger is not touched until this check passes, so a placement
		// abandoned here costs neither a slot in the bound nor an exclusion.
		remaining = pl.remaining(start, budget)
		if remaining < minRetrySec {
			return r.exhaustedSettled(contract, pr, pl, refusals, budgetSpent(remaining, budget, len(refusals)), iterStart)
		}
		pl.tried[next.base] = true
		if next.base != "" {
			// REMOTE re-placements only. Charging the local fall-back against
			// the bound would contradict the whole reason local is reserved: a
			// wide roster could then spend the bound before reaching the one
			// seat that is always able to take the work. Local is held to one
			// use per subtask by pl.tried instead.
			//
			// HONESTLY: under today's control flow this guard changes no
			// outcome, and the mutation battery confirmed it — charging local
			// too is semantically inert. Proof: replacementNode returns local
			// only when the untried remote pool is EMPTY (so no later remote
			// placement is possible at all, `tried` only grows) or when the
			// bound is ALREADY spent (used >= maxRemoteReplacements, so one
			// more makes no difference). It is kept because it encodes the
			// documented rule at the point the rule applies, and it becomes
			// load-bearing the moment the bound, the ordering or local's
			// reservation changes. It is not claimed as tested.
			pl.used++
		}
		replaced := contract
		replaced.TimeoutSec = remaining
		replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
		if r.beforeForced != nil && next.base != "" {
			r.beforeForced(next.base)
		}
		pr = r.attempt(ctx, i, replaced, &next)
		if pr.waitCapacity {
			// The local last resort's composite decision must WAIT (the
			// pair's long seat over a busy agent seat), or the process gate
			// turned the dispatch away: nothing ran, so this is the capacity
			// wait's from here, carrying the refusals that emptied the roster —
			// never a published sentinel.
			if pr.gated {
				// Nothing was sent to that node, so it is neither tried nor
				// spent against the re-placement bound.
				delete(pl.tried, next.base)
				if next.base != "" {
					pl.used--
				}
				pl.waitNote = pr.pendingReason
			}
			return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
		}
		pl.countAttempt(pr)
		pr.Replacements = len(refusals)
		if !r.isReplaceable(pr) {
			// Some node TOOK it (or the call's deadline ended the chain before one was asked:
			// annotateLanding). Whether its answer was any good is the four outcome buckets'
			// business, not this loop's.
			return annotateLanding(pr, refusals)
		}
		refusals = append(refusals, refusalLine(pr))
		r.noteRefusal(pl, pr)
	}
}

// capacityRefusal: the node declined because it has no room right now (queue
// full, leased, draining, shed) — the refusals a capacity wait can outlast.
func capacityRefusal(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
}

// placementPollInterval is how often the capacity wait re-reads the fleet's
// health; refusalCooldown keeps a node that just refused for capacity off the
// candidate list for a moment when its refusal carried no Retry-After of its
// own, because its health may still advertise the room it just denied (the
// snapshot is stale by construction — see saturated). A Retry-After replaces it
// (noteCooldown).
var (
	placementPollInterval = 3 * time.Second
	refusalCooldown       = 10 * time.Second
)

// probeTickBound bounds ONE capacity-wait tick's fleet probe: WHAT IS LEFT of
// the wait, and deliberately nothing tighter. The wait used to hand fetchViews
// the raw run context while every other call site wrapped it in a
// remaining-budget one, so a probe could outlive the wait it was serving.
//
// Two tighter bounds were tried and both were wrong, for reasons worth keeping:
//
//   - TWICE THE POLL INTERVAL (6 s in production). That sits below
//     fetchNodeViewTimeout (15 s), which is this repo's own boundary between
//     slow and down — "a node that cannot answer inside this is down, not busy".
//     A remote whose health takes 6–15 s under load was therefore cancelled on
//     EVERY tick for the whole wait; it never became a candidate, and it was
//     never negative-cached either, because noteDeadProbe rightly refuses to
//     blame a node for the caller giving up. The wait then expired saying "no
//     node had room", which was false.
//   - min(fetchNodeViewTimeout, remaining). Same number as the per-base bound,
//     and that is the trap: the per-base context is DERIVED from this one, so
//     the two deadlines land on the same instant and the tick's fires first
//     (it was created first). Every probe failure then looks like the caller
//     giving up, nothing is ever attributable to a node, the negative cache
//     stays empty, and a dead base is re-dialled at full cost on every tick —
//     measured at 30 dials across one compressed wait.
//
// Nothing tighter is NEEDED, which is what makes the simple bound the right
// one: the fan-out is concurrent and each goroutine is capped at
// fetchNodeViewTimeout, so one black-holed remote costs a tick that bound ONCE
// and is then skipped by the 30 s negative cache — never the sum over the
// roster, and never the whole TTL. This bound's only job is the one no other
// bound covers: a probe must not outlive the wait.
func probeTickBound(deadline time.Time) time.Duration {
	left := time.Until(deadline)
	if left <= 0 {
		// The caller re-checks the deadline immediately after; a zero-length
		// context would cancel the probe before it was sent.
		left = time.Millisecond
	}
	return left
}

// probeFailTally is one base's probe failures during a capacity wait: how many
// ticks it failed, and the most recent reason. Counted per base rather than
// appended per tick because a 40-tick wait against one dead node would
// otherwise render the same sentence forty times into the defer reason.
type probeFailTally struct {
	n    int
	last string
}

// probeFailureNote renders the per-base probe failures a capacity wait
// accumulated, or "" when every tick's probe answered. Sorted by base so the
// sentence is stable across runs (map order is not).
//
// It exists because the tick used to discard probeErrs with `_`: a wait spent
// entirely on remotes that never answered ended as "no node had room … 0
// refusal(s)", which told an operator to add a node when the nodes they had
// were failing to answer. A probe failure is NOT a refusal — nobody declined
// the work — so it is reported as its own clause.
func probeFailureNote(fails map[string]*probeFailTally) string {
	if len(fails) == 0 {
		return ""
	}
	bases := make([]string, 0, len(fails))
	for base := range fails {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	total := 0
	for _, base := range bases {
		f := fails[base]
		total += f.n
		if f.n == 1 {
			parts = append(parts, fmt.Sprintf("%s: %s", base, f.last))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s (last of %d)", base, f.last, f.n))
	}
	return fmt.Sprintf("; %d probe(s) failed during the wait: %s", total, strings.Join(parts, "; "))
}

// heldOutNote renders the nodes the last tick of a capacity wait passed over
// because they could not START the job inside the caller's patience, with the
// arithmetic each one printed, or "" when none was. A held-out node is not a
// refusal - it was re-read every tick and simply never fit - so it is its own
// clause, exactly as a probe failure is.
func heldOutNote(held map[string]string, already string) string {
	if len(held) == 0 {
		return ""
	}
	bases := make([]string, 0, len(held))
	for base := range held {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	for _, base := range bases {
		// The deal's own verdict line (a refusal-chain entry) may already carry
		// this node's arithmetic verbatim; print it once.
		if !strings.Contains(already, held[base]) {
			parts = append(parts, held[base])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; held out by the backlog gate (re-read every tick): " + strings.Join(parts, "; ")
}

// awaitCapacity is the delegator's queue (0.113.18, L5 of the fleet-flow
// chapter): a subtask every fitting node has just refused for CAPACITY — or
// whose only placement is a seat a text lease reserves — waits here, until its
// call's deadline less a reserve when the call has one, else for a TTL
// (agent_placement_wait_sec, or agent_lease_wait_sec when that is longer; see
// capacityWaitFor, ADR 0073),
// and is placed on the FIRST node that frees: the local seat once the lease
// clears, or a remote whose health says it has room (hasRoom). A node that
// passes the check and still refuses (its snapshot was stale) is one more tick,
// not a verdict. The idle time is credited to the contract's budget
// (placements.credit); the attempts themselves are charged as always.
//
// A SHEDDABLE run (priority -1) never waits: measurement traffic that finds no
// idle node is shed at once, class capacity, so it can never queue in front
// of — or behind — production work anywhere in the fleet.
//
// seed is the last refused attempt (or the sentinel); its published fields
// are reused the way exhausted reuses them. refusals is every refusal so far,
// oldest first; why is the sentence the refusal chain ended with (exhausted's
// tail), used verbatim when the wait is switched off.
//
// With the wait DISABLED (agent_placement_wait_sec < 0, and no lease wait)
// the outcome is exactly the pre-0.113.18 one: "placement refused" for a
// refused chain, the holder-naming deferral for a reserved seat.
func (r *runner) awaitCapacity(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, why string) (res PlacedResult) {
	// The fleet was failing its health probe when the sentinel was raised (seed.remotesUnreachable).
	// Whatever ends the wait carries that unless a remote answered while it waited: a local run
	// after the lease cleared, the defer that ends it, a budget defer. `seed` is replaced by
	// refused attempts, so the fact is taken here, and answered is declared up here so the
	// deferred stamp can read it. A remote that answered means the fleet was not dead.
	deadFleet := seed.remotesUnreachable
	answered := map[string]bool{}
	defer func() {
		if deadFleet && len(answered) == 0 {
			res.remotesUnreachable = true
		}
	}()
	localView := r.localView()
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	// p2cSeed is W-11's ranking seed for every betterRemote comparison this
	// wait makes: minted ONCE so the draw stays consistent across the wait's
	// own ticks (never named `seed` — that identifier is this function's own
	// PlacedResult parameter).
	p2cSeed := mintP2CSeed()
	waitStart := time.Now()
	// decided is the composite decision behind the seed (ADR 0052, council
	// R1): the decided seat exists and holds the contract, it would only evict
	// a busy seat. Held in a local so a remote refusal inside the wait (which
	// replaces `seed`) cannot turn the decided wait into the lease wait — the
	// lease branch would then run local through attempt(), re-decide, and
	// hand back the sentinel as a result.
	decided := seed.decided
	if r.priority < core.BandNormal {
		// A sheddable run never waits, so a remote hint the process gate turned away has no tick to take the idle seat in:
		// it takes it here, under the deal's own guards, or it is shed.
		if pr, ok := r.hintSeatWithoutAWait(ctx, i, contract, start, budget, pl, seed, &refusals, fmt.Sprintf("sheddable work (priority %d) does not wait", r.priority)); ok {
			return pr
		}
		return r.settle(contract, r.shedResult(localView, seed, refusals), pl, waitStart)
	}
	// How long the wait runs, and what bounds it (ADR 0073): a call that has a whole-call deadline waits
	// until it, less the reserve a placed job needs; any other call waits agent_placement_wait_sec.
	cw := r.capacityWaitFor(waitStart, decided != nil)
	wait := cw.wait
	// slot is this subtask's place in its call's width, when it came through RunWith. A wait that is held
	// to its TTL because it holds that place while subtasks of the call have not started does not END at
	// the TTL: it gives the place up and runs on to the call's horizon (extendHeld, ADR 0073 amendment
	// 2026-10-10, register C16). leftAtStart is the call's time when the wait began, for the words of the
	// defer that finally ends it.
	slot := runSlotFrom(ctx)
	leftAtStart, _ := r.call.timeLeft()
	gaveSlotUp := false
	if wait <= 0 {
		if cw.call != nil {
			// Neither the operator's off switch nor a chain of refusals that ended: the call has less left
			// than a placed job needs, so there is nothing to wait for (and a job placed now could only be
			// cut). A plain lease reservation keeps its holder-naming deferral; every other place in line
			// is the capacity defer, saying why it did not wait.
			if seed.waitCapacity && !seed.gated && !seed.overflow && !seed.fenced {
				return r.settle(contract, r.reservedDefer(localView, r.localLease(contract), 0, seed.pendingReason, cw.call), pl, waitStart)
			}
			return r.settle(contract, r.capacityDefer(localView, seed, 0, 0, refusals, waitEvidence{note: pl.waitNote, call: cw.call}), pl, waitStart)
		}
		if decided != nil {
			// The wait is switched off: a zero TTL. The expiry rule applies at
			// once — RUN on the decided seat (it holds the contract; only the
			// eviction was worth waiting for), never a capacity defer and
			// never the holder-naming deferral (no lease is held).
			note := fmt.Sprintf("capacity wait disabled (agent_placement_wait_sec=%d) — running on the decided seat %s at once; it evicts %s",
				r.cfg.AgentPlacementWaitSec, decided.Seat, decided.Evicts)
			return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, 0, *decided, false, note, waitStart)
		}
		if seed.gated || seed.overflow {
			// A remote hint the process gate turned away has no wait to take the idle seat in either: the operator switched
			// it off (agent_placement_wait_sec), and the choice is the seat or a defer.
			if pr, ok := r.hintSeatWithoutAWait(ctx, i, contract, start, budget, pl, seed, &refusals,
				fmt.Sprintf("the capacity wait is switched off (agent_placement_wait_sec=%d)", r.cfg.AgentPlacementWaitSec)); ok {
				return pr
			}
			// Nothing is held and nobody refused: the subtask simply has nowhere to
			// wait. That is a capacity defer - the fleet is healthy, it was not this
			// contract's turn - never the holder-naming deferral of a lease nobody holds.
			return r.settle(contract, r.capacityDefer(localView, seed, 0, 0, refusals, waitEvidence{note: pl.waitNote}), pl, waitStart)
		}
		if seed.waitCapacity {
			if seed.fenced {
				return r.settle(contract, r.fencedDefer(localView, seed.pendingReason, 0), pl, waitStart)
			}
			return r.settle(contract, r.reservedDefer(localView, r.localLease(contract), 0, seed.pendingReason, nil), pl, waitStart)
		}
		return r.exhaustedSettled(contract, seed, pl, refusals, why, waitStart)
	}
	deadline := cw.deadline
	spanStart := waitStart
	var idle time.Duration
	// extendHeld is what the TTL of a wait that holds a run slot does instead of ending it. The TTL was
	// the old rule's price for not starving the subtasks behind this one (ADR 0073 decision 9), and the
	// price was the subtask: it ended as a capacity defer while the call had most of its time left and a
	// node freeing a minute later would have taken it. Now the wait gives its place in the call's width
	// up when a subtask of the call has not started (so that one can begin), keeps waiting to the call's
	// horizon like the last subtask to start, and takes a place again only to dispatch (ensureSlot), so
	// the number of attempts the call has open at once is still its width. false = nothing to extend:
	// the wait was not capped by a held slot, or the call's horizon is no later than now.
	extendHeld := func() bool {
		if cw.held == 0 || cw.call != nil || decided != nil {
			return false
		}
		horizon, ok := r.call.waitHorizon()
		if !ok || !horizon.After(time.Now()) {
			return false
		}
		if r.call.unstarted() > 0 && slot.release() {
			gaveSlotUp = true
		}
		cw.call = &callWaitInfo{left: leftAtStart, reserve: callWaitReserve}
		deadline, wait = horizon, horizon.Sub(waitStart)
		return true
	}
	// ensureSlot takes the place back before a dispatch when the wait gave it up. blocked says it had to
	// wait for one, so the room the caller saw is old and the tick is read again; ok=false is the wait
	// (or the call) ending first.
	ensureSlot := func() (ok, blocked bool) {
		if !gaveSlotUp {
			return true, false
		}
		return slot.acquire(ctx, deadline)
	}
	// yieldSlot gives the place up again once an attempt has been refused and the wait goes on.
	yieldSlot := func() {
		if gaveSlotUp {
			slot.release()
		}
	}
	// stLeft is st with the wall the NEXT dispatch would carry: what is left of the
	// budget once the idle time so far is credited (never under the retry floor). The
	// candidates of a tick are judged against it - the same wall re-placement uses - so
	// a node is asked only when it can start the job inside the wait the dispatched
	// contract will actually be given.
	stLeft := func() Subtask {
		c := contract
		c.TimeoutSec = max(remainingSec(start.Add(pl.credit+time.Since(spanStart)), budget), minRetrySec)
		c.TimeoutAuto = false
		return Subtask{Contract: c, EstTokens: st.EstTokens}
	}
	// credit moves the idle span just ended onto the ledger, so an attempt
	// started now is measured against a budget that does not include it.
	credit := func() {
		now := time.Now()
		idle += now.Sub(spanStart)
		pl.credit += now.Sub(spanStart)
		spanStart = now
	}
	// The nodes' cooldowns live on the run (r.cool): an instant per base,
	// JITTERED once when the refusal happened (noteCooldown) so K dispatchers that
	// all refused the same node do not all re-ask it on the same beat, and set from
	// the node's own Retry-After when it sent one. Every subtask of the run reads
	// them: "node A is full" is a fact about A, not about the subtask that met it.
	//
	// heldOut is, per base, the LATEST reason a node that had room was passed over
	// because it could not start the job inside the caller's patience
	// (startsWithinPatience). Such a node is re-read every tick and asked the moment
	// its backlog fits; the reason is only kept so a wait that ends without placing
	// anywhere can print the arithmetic.
	heldOut := map[string]string{}
	// cooling is, per base, the LATEST reason an otherwise eligible node was not asked
	// this wait because its own refusal put it on cooldown: its health may advertise
	// room on every tick while a Retry-After longer than the wait keeps it out, and a
	// defer that said only "no node had room" would not be what happened.
	cooling := map[string]string{}
	// A node whose Retry-After cooldown a health read ended early (cooldowns.lift) is narrated on the
	// placement reason of the dispatch it allowed: cooldowns.liftNarration says when a lift still explains one.
	// probeFails is every base whose health probe failed DURING the wait, by
	// base: the tick used to drop these on the floor, so a wait that never
	// reached a single node reported "0 refusal(s)".
	probeFails := map[string]*probeFailTally{}
	// answered (declared at the top) is every base whose probe returned a view at least once in
	// this wait.
	var lease gpulease.Info
	// localCan: this box can run the contract at all. One that names a layer the box does not declare
	// (register A-108) never takes the local seat in this wait, whatever the seat's load or lease: the
	// wait is for a node that declares the layer.
	localCan := r.localServesLayer(localView, contract)
	// overflow: a deal kept this subtask off the local seat - because it read busy, or because
	// the deal had already spent an idle seat's run-cap line (ADR 0076) - and handed it over
	// because every node with room was already dealt to its headroom. The seat stays off limits
	// until it stops reading busy (localStillBusy) and its run-cap line has a free slot
	// (localSlotAhead, read every tick) - the wait exists to spare the subtask that pile-up, not
	// to rebuild it at tick zero.
	overflow := seed.overflow
	// dealBusy: the deal read the local seat busy (spread's busy rule, route=auto's busy
	// formula) and dealt around it. A subtask it sent to a remote that then turned out full
	// (the process gate, a refusal) stands in this wait exactly like an overflow subtask and
	// keeps the same rule: the seat is taken only once it stops reading busy. Before this only
	// an overflow subtask kept it, so a gate turn-away took the busy seat at the first tick
	// (the TestOverflowStaysOffABusySeatWhoseLoadBecomesUnreadable CI flake, also on main).
	dealBusy := overflow || r.dealReadLocalBusy()
	// reservedSeen: the wait began under, or saw, a text lease on the local seat -
	// it names the placement "lease cleared" when the seat opens. A gate turn-away
	// and a deal's overflow are not leases: no lease is held.
	reservedSeen := seed.waitCapacity && !seed.gated && !seed.overflow && !seed.fenced
	// fencedSeen: the wait began under, or saw, a lease that fences every local seat the contract
	// could run on (fencedLocal) - it names the placement "fence cleared" when the seat opens.
	fencedSeen := seed.fenced
	// occupiedSeen: the wait saw another vLLM seat occupy the local cards (the seat it names).
	occupiedSeen := ""
	// noteSuffix carries why the subtask waited, when nothing refused it, onto the
	// placement reasons the wait writes.
	noteSuffix := ""
	if pl.waitNote != "" {
		noteSuffix = "; " + pl.waitNote
	}
	// budgetGone ends the wait when the contract has no budget left for another
	// attempt. After refusals that is the chain's own exhausted message; with none -
	// a gate turn-away or a deal's overflow that no node ever refused, so nothing was
	// refused and nothing ran - it is a budget defer, as runDecided files it, and
	// never a `placement refused` failure naming a refusal nobody made.
	budgetGone := func(remaining int) PlacedResult {
		if len(refusals) > 0 {
			return r.exhaustedSettled(contract, seed, pl, refusals, budgetSpent(remaining, budget, len(refusals)), waitStart)
		}
		return r.settle(contract, r.waitBudgetDefer(localView, seed, idle, budgetSpent(remaining, budget, pl.attempts), pl.waitNote), pl, waitStart)
	}
	// places is, per dial base ("" = the local seat), the place in line this wait last saw the
	// subtask standing in (GPU routing P7): what the node was held behind and when that is
	// expected to clear. Each part is replaced only when its own reading succeeds (the local
	// seat's when the lease is read, the remotes' when the fleet probe answers), so a tick the
	// deadline cuts short leaves the last good reading standing, and only that survives to the
	// defer that ends the wait, which publishes it (placeKept).
	places := map[string]PlaceWait{}
	// fleetFirst: a remote hint (ADR 0078) is placed remotes-first, and a subtask the process gate turned away has had
	// no look at the fleet since the deal: its node is full for THIS process, which says nothing about the others. So
	// the first tick reads the fleet before it takes the idle seat. Every other way into the wait (a deal's overflow,
	// a refusal, a lease) has just established that no remote has room, and keeps the seat's place in line at once.
	fleetFirst := r.remoteHint() && seed.gated
	for wait > 0 && ctx.Err() == nil {
		if cw.call != nil && !time.Now().Before(deadline) {
			// The last tick of a TTL wait still looks once more after its sleep; a wait bounded by the call
			// does not, because what it would place now is placed in the reserve it exists to keep.
			break
		}
		if decided == nil && r.route != "remote" && !pl.tried[""] && !fleetFirst {
			delete(places, "")
			lease = r.localLease(contract)
			if Reserved(lease) {
				reservedSeen = true
				if localCan {
					places[""] = localPlace(localView, "reserved by a lease", lease)
				}
			}
			// A fence on every seat of the chain keeps the local seat out of the wait too: the
			// seat would turn the run away at its own pre-check (fencedLocal, GPU routing P7).
			// A box that cannot run the contract at all (it declares no layer the contract
			// names) is no place in line, whatever leases it holds.
			fence, fenceWhy, fenced := r.fencedLocal(contract)
			fenced = fenced && localCan
			if fenced {
				fencedSeen = true
				places[""] = localPlace(localView, "fenced ("+fenceWhy+")", fence)
			}
			// The local seat is a candidate once no text lease reserves it AND its
			// run-cap line has a free slot ahead of a newcomer (localSlotAhead) - a
			// full seat is a line this subtask could not leave, and the wait exists to
			// spare it that. Where there is no other node to spare it for (route=local,
			// or no remotes configured) the seat's own line IS the wait, bounded by the
			// run's wall, and the subtask joins it as soon as no lease reserves it.
			free, _ := r.localSlotAhead()
			if r.route == "local" || len(r.remotes) == 0 {
				free = true
			}
			// With no node to wait for (route=local, or no remotes) the seat's own decision still
			// answers a layer it does not declare, by deferring it naming the layer; otherwise the
			// seat is no candidate for it.
			if !localCan && r.route != "local" && len(r.remotes) > 0 {
				free = false
			}
			// A subtask a deal kept off the seat for reading busy stays off it while
			// the seat still reads busy by the deal's own reading: the registry above
			// counts only delegated runs, and a seat busy with anyone else's requests
			// shows nothing there.
			// Whatever started the wait (a refusal, a lease, a deal, no remote at all), a seat
			// another vLLM seat occupies is no candidate: taking it unloads that seat
			// (probeLocalBusy's occupiedBy; operator 2026-10-06: "wait in line"). It becomes one
			// when the occupant leaves. Read first, so the reason can name the occupant.
			occupied := false
			if !Reserved(lease) && !fenced && free && r.route != "local" {
				if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" {
					occupied, occupiedSeen = true, rd.occupiedBy
					places[""] = PlaceWait{Node: localView.NodeID, On: "seat", Detail: rd.why()}
				}
			}
			stillBusy := false
			if !occupied && dealBusy && free && !Reserved(lease) {
				// The overflow keeps the deal's whole reading; a subtask that reached the wait
				// another way (the gate, a refusal) keeps only the seat's load: a lease is judged
				// by the wait itself above (Reserved, fenced), and one this process holds is no
				// reason to stay off the seat.
				readLease := lease
				if !overflow {
					readLease = gpulease.Info{}
				}
				stillBusy, _ = r.localStillBusy(ctx, readLease)
			}
			if !Reserved(lease) && !fenced && free && !stillBusy && !occupied {
				if ok, blocked := ensureSlot(); !ok {
					break
				} else if blocked {
					continue
				}
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return budgetGone(remaining)
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				// Reaching here means the local seat could not take the subtask when the
				// wait began - reserved by a text lease, its run-cap line full (an
				// unreserved local seat with a free slot is taken by replacementNode before
				// any wait), or busy when a deal dealt the run - and now can.
				reason := fmt.Sprintf("local seat had no free run-cap slot, one freed after %s — running local (capacity wait)", idle.Round(time.Second))
				switch {
				case reservedSeen:
					reason = fmt.Sprintf("local seat was reserved, lease cleared after %s — running local (capacity wait)", idle.Round(time.Second))
				case fencedSeen:
					reason = fmt.Sprintf("local seat was fenced by a lease, fence cleared after %s — running local (capacity wait)", idle.Round(time.Second))
				case occupiedSeen != "":
					reason = fmt.Sprintf("local seat was occupied by the vLLM seat %s, which left after %s — running local (capacity wait)", occupiedSeen, idle.Round(time.Second))
				case r.remoteHint() && !r.dealReadLocalBusy():
					// The deal kept the subtask off a seat that never read busy: a remote hint prefers the remotes
					// (ADR 0078), and none had room.
					reason = fmt.Sprintf("no remote had room and the local seat was free after %s — a remote hint falls back to it (capacity wait)", idle.Round(time.Second))
				case dealBusy:
					reason = fmt.Sprintf("local seat was busy when the deal kept this subtask off it, idle after %s — running local (capacity wait)", idle.Round(time.Second))
				}
				forced := placement{view: localView, reason: reason + noteSuffix}
				pr := r.attempt(ctx, i, replaced, &forced)
				if pr.waitCapacity {
					// The lease cleared, and the composite decision now asks
					// to wait for a busy pair seat to drain: the same wait
					// continues as a DECIDED one (nothing ran, nothing to
					// record); the tick below re-decides and runs the seat.
					decided = pr.decided
					continue
				}
				spanStart = time.Now() // the attempt is charged; the wait resumes here
				pl.tried[""] = true
				pl.countAttempt(pr)
				if !r.isReplaceable(pr) {
					return r.landedAfterWait(pr, idle, refusals)
				}
				// The local seat deferred it as capacity after all (its line filled between
				// the read and the run): one more refusal, and the wait goes on for a
				// remote - the local seat is tried now and is not asked twice.
				seed = pr
				refusals = append(refusals, refusalLine(pr))
				r.noteRefusal(pl, pr)
			}
		}
		if r.route != "local" {
			// The tick's probe may not outlive the wait it serves; the per-base
			// bound inside the fan-out caps what any ONE remote can cost
			// (probeTickBound explains why nothing tighter belongs here).
			tickCtx, cancelTick := context.WithTimeout(ctx, probeTickBound(deadline))
			views, bases, _, failed, readAfter := r.fleetReadForWait(tickCtx)
			cancelTick()
			fleetFirst = false // the fleet has been read once: from the next tick the seat may take its place in line
			if ctx.Err() == nil && !time.Now().Before(deadline) && extendHeld() {
				continue // the TTL ended mid-probe and the wait runs on: the probe may have been cut by it, so read again
			}
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				// The tick's probe is bounded by the wait's own deadline, and by the
				// context the wait runs under (the call's deadline, ADR 0065, or the
				// caller giving up), so a probe still in flight when either ended may
				// have failed because the WAIT or the CALL ended, not because its node
				// is down. For a node that answered earlier in this wait that is no
				// evidence at all: keep what the previous tick learned (cooling down,
				// held out) for the defer's reason. A node that never answered is still
				// named: nobody could ask it.
				for base, why := range failed {
					if answered[base] {
						continue
					}
					f := probeFails[base]
					if f == nil {
						f = &probeFailTally{}
						probeFails[base] = f
					}
					f.n, f.last = f.n+1, why
				}
				break
			}
			for _, b := range bases {
				answered[b] = true
			}
			// A base that did not answer is not a node with no room — it is a
			// node nobody could ask. Kept per base so the defer can say so.
			for base, why := range failed {
				f := probeFails[base]
				if f == nil {
					f = &probeFailTally{}
					probeFails[base] = f
				}
				f.n, f.last = f.n+1, why
			}
			best := -1
			prior := fleetTokSPrior(views)
			now := time.Now()
			clear(heldOut) // only THIS tick's read decides who is held out
			clear(cooling)
			for base := range places {
				if base != "" {
					delete(places, base)
				}
			}
			sn := stLeft()
			for j, v := range views {
				if pl.excluded[bases[j]] || pl.ran[bases[j]] {
					continue
				}
				if eligible, word, detail := eligibilityVerdict(sn, v); !eligible {
					// A node a lease fences for this contract is a place in line (it frees when the
					// lease ends); one that can never run the contract is not.
					if word == "lease" {
						places[bases[j]] = remoteLeasePlace(v, &sn, detail)
					}
					continue
				}
				// A node that refused for capacity is not asked again before its
				// cooldown ends, unless this tick's read - taken after the refusal - proves it
				// has a free worker (cooldowns.lift): its Retry-After was the node's own
				// estimate of when one would free, and a later read that shows one free has
				// outdated it. Health that proves nothing (an unknown, a saturated node, a
				// backlog) never lifts it: the snapshot may still advertise the room the
				// node just denied.
				if until, isCooling := r.cool.heldUntil(bases[j], now); isCooling {
					if _, liftedNow := r.cool.lift(bases[j], v, readAfter, now); !liftedNow {
						cooling[bases[j]] = fmt.Sprintf("%s: cooling down after its own refusal (%s left)", laneID(v), until.Sub(now).Round(time.Second))
						places[bases[j]] = PlaceWait{Node: laneID(v), On: "cooldown", Detail: "cooling down after its own refusal", EtaSec: int(until.Sub(now).Seconds())}
						continue
					}
				}
				patience, clamp := r.patience(sn.Contract, v)
				if !hasRoom(v, false) || !processGate.available(bases[j], admissionCeiling(v)) {
					places[bases[j]] = PlaceWait{Node: laneID(v), On: "queue", Detail: whyNoRoom(v, false, patience, clamp)}
					continue
				}
				// Room is not enough: a job sent to a node whose backlog outlasts what the
				// caller will wait is one the delegator abandons while the node runs it
				// anyway. Held out - re-read next tick, never refused for good.
				if ok, why := startsWithinPatience(v, patience); !ok {
					why += clamp
					heldOut[bases[j]] = laneID(v) + ": backlog (" + why + ")"
					places[bases[j]] = PlaceWait{Node: laneID(v), On: "backlog", Detail: why, EtaSec: int(queueWaitFor(v))}
					continue
				}
				if best < 0 || betterRemote(p2cSeed, &sn, prior, v, views[best]) {
					best = j
				}
			}
			if best >= 0 {
				if ok, blocked := ensureSlot(); !ok {
					break
				} else if blocked {
					continue
				}
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return budgetGone(remaining)
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				forced := placement{view: views[best], base: bases[best],
					reason: fmt.Sprintf("capacity wait → %s (room after %s)", views[best].NodeID, idle.Round(time.Second)) + liftedClause(r.cool.liftNarration(bases[best], time.Now())) + noteSuffix}
				if r.beforeForced != nil {
					r.beforeForced(bases[best])
				}
				pr := r.attempt(ctx, i, replaced, &forced)
				spanStart = time.Now() // the attempt is charged; the wait resumes here
				if pr.gated {
					// Another Run of this process took the node's last slot between the read
					// and the dispatch: nothing was sent, so nothing is tried, counted or
					// cooled - the wait goes on and reads the fleet again next tick.
				} else {
					pl.tried[bases[best]] = true
					pl.countAttempt(pr)
					if !r.isReplaceable(pr) {
						return r.landedAfterWait(pr, idle, refusals)
					}
					seed = pr
					refusals = append(refusals, refusalLine(pr))
					// The refusal cools the node (its own Retry-After, else refusalCooldown); a
					// non-capacity answer excludes it from the rest of the wait.
					r.noteRefusal(pl, pr)
					if capacityRefusal(pr.refusalStatus) || capacityDeferRefusal(pr) {
						eta := 0
						if until, cool := r.cool.heldUntil(bases[best], time.Now()); cool {
							eta = int(time.Until(until).Seconds())
						}
						places[bases[best]] = PlaceWait{Node: laneID(views[best]), On: "queue", Detail: "refused the dispatch for capacity", EtaSec: eta}
					}
				}
				// Fall through to the sleep: a refusal is charged (its attempt
				// wrote telemetry and spent budget), so re-asking is paced by the
				// poll interval — never a tight loop of dispatches.
			}
		}
		if !time.Now().Before(deadline) && !extendHeld() {
			break
		}
		yieldSlot()
		tick := jitteredWithin(placementPollInterval, time.Until(deadline))
		select {
		case <-ctx.Done():
		case <-time.After(tick):
		}
		// The decided seed's tick (after the sleep — the decision that sent
		// the subtask here was made moments ago, so the first re-read is one
		// poll interval later): re-run the decider with fresh readings and run
		// on the decided seat the moment nothing has to be evicted. A text
		// lease that appeared mid-wait keeps the seat off limits; a decision
		// that now DEFERS (a guard flipped) ends the wait as that defer.
		if decided != nil && r.route != "remote" && ctx.Err() == nil && !Reserved(r.localLease(contract)) {
			dec := r.decide(ctx, contract, st)
			switch {
			case dec.Defer:
				credit()
				pr := r.decidedDefer(localView, dec, seed.PlacementReason)
				pr.waited, pr.CapacityWaitSec = true, idle.Seconds()
				return r.settle(contract, pr, pl, waitStart)
			case !dec.Wait:
				credit()
				note := fmt.Sprintf("capacity wait → local seat %s (%s drained after %s)", dec.Seat, decided.Evicts, idle.Round(time.Second))
				return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, idle, dec, true, note, waitStart)
			}
			decided = &dec // the freshest reason, for the expiry note
		}
	}
	credit()
	if decided != nil && r.route != "remote" && !pl.tried[""] {
		if info := r.localLease(contract); Reserved(info) {
			// A text lease took the cards during the wait: the holder is real,
			// and running on the reserved seat is the one thing the lease
			// forbids. The established deferral, naming the holder.
			return r.settle(contract, r.reservedDefer(localView, info, idle, decided.Reason, nil), pl, waitStart)
		}
		// TTL expiry for a decided seed RUNS on the decided seat (the plan's
		// rule): the seat holds the contract and is free to load; the wait was
		// only ever about sparing the seat it evicts. Never capacityDefer —
		// "no node had room" would be false.
		note := fmt.Sprintf("capacity wait expired after %s (agent_placement_wait_sec=%d) with %s still busy — running on the decided seat %s; it evicts %s",
			idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, decided.Evicts, decided.Seat, decided.Evicts)
		return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, idle, *decided, true, note, waitStart)
	}
	if r.route != "remote" && localCan && Reserved(lease) {
		// Nothing freed and the local seat is still reserved: the established
		// deferral, naming the holder (class infrastructure — a human's timing
		// decision), with the refusals appended.
		return r.settle(contract, placeKept(r.reservedDefer(localView, lease, idle, "no eligible remote had room — "+strings.Join(refusals, "; ")+noteSuffix, cw.call), places), pl, waitStart)
	}
	return r.settle(contract, placeKept(r.capacityDefer(localView, seed, idle, wait, refusals, waitEvidence{note: pl.waitNote, probeFails: probeFails, heldOut: heldOut, cooling: cooling, call: cw.call, held: cw.held}), places), pl, waitStart)
}
