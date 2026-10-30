// run_wait.go - Wait-for-capacity helpers: wait results, TTLs and the deferred results a wait can end in.

package delegate

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0052), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// hintSeatWithoutAWait is the remote hint's seat fallback (ADR 0078 decision 5) for a subtask the process gate turned
// away when the capacity wait cannot carry it: the operator switched the wait off, or the run is sheddable and never
// waits. The deal gave the subtask a node because its health showed headroom, and never asked the gate; this is the
// first place the hint learns that node is full for THIS process. A remote pin would defer here while the seat idles, and
// the hint's whole difference is that the seat may run what no remote can take, so with no wait to read the fleet in
// and take the seat on a later tick, the choice is the seat now or a defer (a shed, for a sheddable run).
//
// The seat is taken under the guards of the deal's own fallback (dealAutoRemoteRoom): the seat read free when the deal was
// made (hintSeatFree: no lease, no full line, no load in progress, no vLLM seat to evict), and it serves the contract's
// layer. The deal's picture can be minutes old, so the conditions that can change are read again now: no lease reserves
// or fences the seat, it is not at its run cap, loading or occupied, and its run-cap line has a free slot ahead of a
// newcomer. An unreadable seat keeps the deal's answer (free), as the deal does. ok=false leaves the caller's outcome (the
// shed, the capacity defer) as it was. Before the attempt nothing has run or been recorded: that includes a composite decision
// that asks the seat to wait, which is the wait's business and not this fallback's. The one ok=false after it is the seat's
// line filling between the fresh read and the run: the seat's admission then declines the subtask as capacity with no step
// run (capacityDeferRefusal, ADR 0063 decision 3), which is filed on the caller's refusals (the shed or the defer names it, as
// the capacity wait files the same answer) and not published as the seat's result. It was: a sheddable run read as a
// capacity defer of the local seat instead of the shed it was.
func (r *runner) hintSeatWithoutAWait(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals *[]string, why string) (PlacedResult, bool) {
	if !r.remoteHint() || !seed.gated || !r.hintSeatFree || pl.tried[""] {
		return PlacedResult{}, false
	}
	localView := r.localView()
	if !r.localServesLayer(localView, contract) {
		return PlacedResult{}, false
	}
	if Reserved(r.localLease(contract)) {
		return PlacedResult{}, false
	}
	if _, _, fenced := r.fencedLocal(contract); fenced {
		return PlacedResult{}, false
	}
	if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" || r.atRunCap(rd.inflight) || rd.loading {
		return PlacedResult{}, false
	}
	if free, _ := r.localSlotAhead(); !free {
		return PlacedResult{}, false
	}
	remaining := pl.remaining(start, budget)
	if remaining < minRetrySec {
		return PlacedResult{}, false
	}
	replaced := contract
	replaced.TimeoutSec = remaining
	replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
	reason := "the dealt node was full for this process and " + why + " — the idle local seat takes it instead, a remote hint falling back where no remote has room"
	if pl.waitNote != "" {
		reason += "; " + pl.waitNote
	}
	pr := r.attempt(ctx, i, replaced, &placement{view: localView, reason: reason})
	if pr.waitCapacity {
		return PlacedResult{}, false
	}
	pl.tried[""] = true
	pl.countAttempt(pr)
	if capacityDeferRefusal(pr) {
		// The line filled between the read and the run: the seat declined the job and ran nothing, so it is the refusal
		// the caller's shed or defer names, not the result it publishes.
		*refusals = append(*refusals, refusalLine(pr))
		r.noteRefusal(pl, pr)
		return PlacedResult{}, false
	}
	return annotateLanding(pr, *refusals), true
}

// localPlace is the place the local seat holds in line while a lease keeps the contract off it:
// the lease that holds it and, when the lease declares an end that has not passed, when that is.
func localPlace(local NodeView, what string, info gpulease.Info) PlaceWait {
	eta := 0
	for _, l := range info.Each() {
		if l.ExpiresAt.IsZero() || l.ExpiresAt.UnixMilli() <= 0 {
			continue
		}
		if left := int(time.Until(l.ExpiresAt).Seconds()); left > 0 && (eta == 0 || left < eta) {
			eta = left
		}
	}
	return PlaceWait{Node: local.NodeID, On: "lease", Detail: what + " - " + HolderLine(info), EtaSec: eta}
}

// remoteLeasePlace is the place a remote node holds in line while a lease on the cards this
// contract needs fences it: the node's own reason, and the earliest end any such lease
// published (an overdue lease says nothing about when it frees).
func remoteLeasePlace(v NodeView, st *Subtask, detail string) PlaceWait {
	eta := 0
	for _, l := range v.leasesHolding(st, func(l LeaseView) bool { return !l.Overdue && l.RemainingSec > 0 }) {
		if eta == 0 || l.RemainingSec < eta {
			eta = l.RemainingSec
		}
	}
	return PlaceWait{Node: laneID(v), On: "lease", Detail: "fenced: " + detail, EtaSec: eta}
}

// placeKept stamps the places a wait ended behind onto the defer that ends it (GPU routing P7):
// the structured list, the soonest known end as a retry hint, and the same facts in prose, so a
// caller (or a model reading the reason) re-asks when it can succeed instead of guessing. The
// durable token that RESUMES a place across calls belongs to media admission (plan P13); this is
// the delegator's half, the facts a re-call is placed by. A wait that stood behind nothing it
// could name publishes nothing new.
func placeKept(pr PlacedResult, places map[string]PlaceWait) PlacedResult {
	if len(places) == 0 {
		return pr
	}
	list := make([]PlaceWait, 0, len(places))
	for _, p := range places {
		list = append(list, p)
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].Node != list[b].Node {
			return list[a].Node < list[b].Node
		}
		return list[a].On < list[b].On
	})
	soonest := 0
	parts := make([]string, 0, len(list))
	for _, p := range list {
		part := fmt.Sprintf("%s: %s (%s", p.Node, p.On, p.Detail)
		if p.EtaSec > 0 {
			part += ", expected to clear in " + etaPhrase(p.EtaSec)
			if soonest == 0 || p.EtaSec < soonest {
				soonest = p.EtaSec
			}
		}
		parts = append(parts, part+")")
	}
	clause := "; standing in line behind: " + strings.Join(parts, "; ")
	if soonest > 0 {
		clause += "; the soonest of those clears in about " + etaPhrase(soonest) + " (retry_after_sec)"
	}
	pr.PlaceKeeping, pr.RetryAfterSec = list, soonest
	pr.PlacementReason += clause
	pr.Result.Reason += clause
	return pr
}

// etaPhrase words a number of seconds for a reason an operator reads.
func etaPhrase(sec int) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	case sec >= 90:
		return fmt.Sprintf("%dm", (sec+30)/60)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// runDecided runs a decided-seed subtask on its decided seat once the capacity
// wait is over (drained, expired, or disabled): a forced LOCAL placement that
// carries dec (Wait cleared) so attempt() runs it rather than deciding again,
// with what is left of the budget after the wait's credit. waited marks the
// result as one that waited (Summary.Waited, capacity_wait_sec); a disabled
// wait ran at once and is not counted as one. A budget that cannot start the
// seat is a BUDGET defer when nobody refused the work (the wait is credited,
// so this is a contract whose timeout_sec was under the floor to begin with)
// and the established "placement refused" failure when nodes did.
func (r *runner) runDecided(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, idle time.Duration, dec placetable.Decision, waited bool, note string, waitStart time.Time) PlacedResult {
	remaining := pl.remaining(start, budget)
	if remaining < minRetrySec {
		if len(refusals) > 0 {
			return r.exhaustedSettled(contract, seed, pl, refusals, budgetSpent(remaining, budget, len(refusals)), waitStart)
		}
		local := r.localView()
		reason := fmt.Sprintf("%s: %s — the decided seat %s was not started", budgetSpent(remaining, budget, pl.attempts), note, dec.Seat)
		pr := PlacedResult{
			Node: local.NodeID, Seat: dec.Seat, PlacementReason: seed.PlacementReason, Placed: placedOrNil(dec), Unplaced: true,
			waited: waited, CapacityWaitSec: idle.Seconds(),
			Result: core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion,
				NodeID:        local.NodeID,
				Seat:          dec.Seat,
				Deferred:      true,
				DeferClass:    core.DeferClassBudget,
				Reason:        reason,
				Placed:        placedOrNil(dec),
			},
		}
		return r.settle(contract, pr, pl, waitStart)
	}
	replaced := contract
	replaced.TimeoutSec = remaining
	replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
	dec.Wait = false
	forced := placement{view: r.localView(), reason: note, decided: &dec}
	pr := r.attempt(ctx, i, replaced, &forced)
	pl.tried[""] = true
	pl.countAttempt(pr)
	if !waited {
		return pr
	}
	return r.landedAfterWait(pr, idle, refusals)
}

// decidedDefer is the result for a LOCAL placement the composite decision
// refused (ADR 0052): a guard refused the layer, or no layer window holds the
// contract. Deferred under the decision's class with its reason, the placed
// block naming the guard or the largest window on the wire AND in the ledger
// row — never a silent trim, never a fall-back seat (spec D4). Nothing ran:
// Unplaced, and the seat named is the one that was refused (the planner seat
// never saw the contract).
func (r *runner) decidedDefer(local NodeView, dec placetable.Decision, reason string) PlacedResult {
	seat := local.AgentSeat
	if dec.Seat != "" {
		seat = dec.Seat
	}
	placed := placedOrNil(dec)
	return PlacedResult{
		Node: local.NodeID, Seat: seat, PlacementReason: reason, Placed: placed, Unplaced: true,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			NodeID:        local.NodeID,
			Seat:          seat,
			Deferred:      true,
			DeferClass:    dec.DeferClass,
			Reason:        dec.Reason,
			Placed:        placed,
		},
	}
}

// landedAfterWait annotates a result some node TOOK after a capacity wait (or that the call's
// deadline cut before one was asked: annotateLanding).
func (r *runner) landedAfterWait(pr PlacedResult, idle time.Duration, refusals []string) PlacedResult {
	pr.waited = true
	pr.CapacityWaitSec = idle.Seconds()
	return annotateLanding(pr, refusals)
}

// annotateLanding files the refusal history on the attempt that ENDED a chain of refusals without
// being refused itself. Usually a node took it, and the subtask was re-placed once per refusal.
// The exception is an attempt the call's deadline cut before any node was asked (an Unplaced cut,
// ADR 0065: attempt starts nothing once the deadline has passed): nothing took it, so "re-placed
// after N refusals" would claim a placement that never happened, and Summary.Replaced would count
// it. It is filed the way exhausted() files a chain nobody took: one re-placement fewer than
// refusals (the placement the deadline ended is not one), the note worded for a subtask nobody
// ran, and no note when nothing was re-placed (a single refusal with nowhere to go).
//
// Only the deadline's cut is read this way. A subtask the local seat's own decision deferred was
// placed there (the seat was asked), and one the deadline cut while a node was running it was
// taken: both keep the wording and the count they had.
func annotateLanding(pr PlacedResult, refusals []string) PlacedResult {
	if pr.deadlineCut && pr.Unplaced {
		pr.Replacements = max(len(refusals)-1, 0)
		pr.ReplacementNote = ""
		if pr.Replacements > 0 {
			pr.ReplacementNote = replacementNote(refusals, false)
		}
		return pr
	}
	pr.Replacements = len(refusals)
	if len(refusals) > 0 {
		pr.ReplacementNote = replacementNote(refusals, true)
	}
	return pr
}

// waitEvidence is what a capacity wait learned besides its refusal chain, for the defer
// that ends it: why it waited when nothing refused it (note), the probes that failed
// during it, the nodes the backlog gate held out, and the nodes their own refusal's
// cooldown kept out. None of it is a refusal.
type waitEvidence struct {
	note       string
	probeFails map[string]*probeFailTally
	heldOut    map[string]string
	cooling    map[string]string
	// call is set when the call's deadline bounded the wait (ADR 0073), so the defer says that
	// and does not tell the caller to raise a setting that never bounded it.
	call *callWaitInfo
	// held is how many subtasks of the call had not started when a wait that kept its configured bound began
	// (capacityWait.held): the defer says why a call with time left stopped waiting.
	held int
}

// capacityWait is how long and until when one subtask's capacity wait runs, and what bounds it.
type capacityWait struct {
	// wait is how long the wait may run; 0 means there is nothing to wait for: the operator
	// switched the wait off, or (call set) the call has less left than a placed job needs.
	wait time.Duration
	// deadline is when the wait ends.
	deadline time.Time
	// call is non-nil when the call's deadline, not agent_placement_wait_sec, bounds the wait.
	call *callWaitInfo
	// held is how many subtasks of the call had not started when the wait began; non-zero means the wait
	// kept its configured bound, and not the call's, because it holds a run slot they are waiting for.
	held int
}

// callWaitInfo is what a wait bounded by the call's deadline was given, for the words of the
// defer that ends it.
type callWaitInfo struct {
	left    time.Duration // the call's remaining time when the wait began
	reserve time.Duration // what the wait leaves unspent, for a placed job to run in
	noTime  bool          // the call had no more than the reserve left, so the wait did not run
}

// words says what bounded the wait, in the order a reader of a defer needs it: the call's own
// clock, the reserve, and that the setting a caller might reach for does not apply.
func (c *callWaitInfo) words() string {
	if c.noTime {
		return fmt.Sprintf("the call had only %s left, no more than the %s a placed job needs to run in, so it did not wait", c.left.Round(time.Second), c.reserve.Round(time.Second))
	}
	return fmt.Sprintf("bounded by the call's deadline, not by agent_placement_wait_sec: the call had %s left when the wait began, less the %s a placed job needs to run in", c.left.Round(time.Second), c.reserve.Round(time.Second))
}

// placementWaitDefault is DefaultPlacementWait as the engine reads it: a var so a test can
// compress the built-in wait the way it compresses placementPollInterval. Production never
// mutates it.
var placementWaitDefault = config.DefaultPlacementWait

// placementTTL is the capacity wait of a call that has no whole-call deadline:
// agent_placement_wait_sec, else the built-in 120 s, and 0 when the operator switched the wait
// off. A call that has a deadline waits until it instead (capacityWaitFor); a wait that holds a
// run slot while subtasks of the call have not started holds the slot for this TTL and then
// gives it up and runs on to the call's horizon (awaitCapacity, extendHeld); the retry's wait
// (awaitRetrySeat) and a composite decision's eviction wait keep this TTL under any call
// (ADR 0073).
func (r *runner) placementTTL() time.Duration {
	if r.cfg.AgentPlacementWaitSec == 0 {
		return placementWaitDefault
	}
	return r.cfg.PlacementWait()
}

// capacityWaitFor decides how long a subtask's capacity wait runs (ADR 0073, completing the
// change ADR 0063 and ADR 0065 name as next): a call that has a whole-call deadline waits until
// that deadline less callWaitReserve, whatever agent_placement_wait_sec says, so band-0 work
// never defers for capacity while the call still has time to place it; a call that has none
// (the CLI verbs, agent_run, offload_ask) waits agent_placement_wait_sec as before.
//
// Three things keep the TTL under a deadline. A negative agent_placement_wait_sec is the
// operator's off switch for every call, and the deadline does not turn it back on. A decided
// seed (a composite box's pair seat draining) keeps it because its expiry RUNS the contract on
// the decided seat, which needs the time the reserve would take away. And a wait that holds a
// run slot while subtasks of the call have not started holds that slot for the bound it had
// before the deadline (the larger of the TTL and agent_lease_wait_sec) unless the call ends
// sooner. That bound no longer ENDS the wait (amendment 2026-10-10, register C16): at it the
// wait gives the slot up and runs on to the horizon like the last subtask to start (awaitCapacity,
// extendHeld), so a subtask in line for a node is never refused while the call has time.
//
// agent_lease_wait_sec is part of that bound, never of the call's: a wait the call bounds ends
// at the horizon even when the lease wait is longer, because a wait past the deadline helps no
// one.
func (r *runner) capacityWaitFor(start time.Time, decided bool) capacityWait {
	// The wait a call with no deadline has, and the ceiling of every wait that is not call-bound: the larger of
	// agent_placement_wait_sec and agent_lease_wait_sec, as it always was.
	base := max(r.placementTTL(), time.Duration(r.cfg.AgentLeaseWaitSec)*time.Second)
	w := capacityWait{wait: base, deadline: start.Add(base)}
	if r.cfg.AgentPlacementWaitSec < 0 || decided {
		return w
	}
	horizon, ok := r.call.waitHorizon()
	if !ok {
		return w
	}
	bound := max(horizon.Sub(start), 0)
	// A wait holds one of the call's run slots (RunWith's sem), so while subtasks of the call have not started
	// - the ones waiting for a slot and the ones of later chunks - it may not run to the horizon: they would
	// start late, or never, and with less than the reserve left. Such a wait holds the slot for the bound it
	// had before ADR 0073, unless the call's own end comes sooner, and awaitCapacity then gives the slot up
	// and runs the wait on to the horizon instead of ending it (the 2026-10-10 amendment). The last subtask to
	// start waits for the horizon holding its slot, since nothing is behind it.
	if held := r.call.unstarted(); held > 0 && base <= bound {
		w.held = held
		return w
	}
	// The call is the bound, and a longer agent_lease_wait_sec does not extend it: a wait past the call's
	// deadline cannot help anyone, the call is over before it ends.
	left, _ := r.call.timeLeft()
	w.deadline, w.wait = horizon, bound
	w.call = &callWaitInfo{left: left, reserve: callWaitReserve, noTime: bound <= 0}
	return w
}

// coolingNote renders the nodes the last tick of a capacity wait did not ask because
// their own refusal put them on cooldown, or "" when none. Like a held-out node they
// are re-read every tick and asked when the cooldown ends; a Retry-After longer than
// the wait means that never happens inside it, and the defer says so.
func coolingNote(cooling map[string]string) string {
	if len(cooling) == 0 {
		return ""
	}
	bases := make([]string, 0, len(cooling))
	for base := range cooling {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	for _, base := range bases {
		parts = append(parts, cooling[base])
	}
	return "; not asked again inside the wait (re-read every tick, asked when the cooldown ends): " + strings.Join(parts, "; ")
}

// capacityDefer is the TTL outcome: every node that could run the subtask was
// full for the whole wait. Deferred, class capacity — the fleet is healthy and
// the contract is sound; it was not this contract's turn — with every refusal
// and the wait named so the caller can re-run, widen the wait, or add a node.
//
// note is why the subtask waited when nothing refused it (PlacedResult.gated and
// .overflow: a process-gate turn-away, a deal's overflow): narration, never counted
// among the refusals.
func (r *runner) capacityDefer(local NodeView, seed PlacedResult, idle, wait time.Duration, refusals []string, ev waitEvidence) PlacedResult {
	chain := fmt.Sprintf("%d refusal(s)", len(refusals))
	if len(refusals) > 0 {
		chain += ": " + strings.Join(refusals, "; ")
	}
	if ev.note != "" {
		chain += "; waiting because: " + ev.note
	}
	slotNote := ""
	if ev.held > 0 && ev.call == nil {
		// The call had time left, and this wait ended anyway: it held a run slot that subtasks of the call were
		// waiting for (ADR 0073). Saying so keeps "no node had room within 2m0s" from reading as the call's own limit.
		slotNote = fmt.Sprintf("; %d subtask(s) of the call had not started, so this wait could not hold its run slot to the call's deadline", ev.held)
	}
	reason := fmt.Sprintf("capacity wait: no node had room within %s (waited %s; agent_placement_wait_sec=%d; %s%s%s%s%s) — re-run later, raise agent_placement_wait_sec, or add a node",
		wait, idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, chain,
		heldOutNote(ev.heldOut, strings.Join(refusals, "; ")), coolingNote(ev.cooling), probeFailureNote(ev.probeFails), slotNote)
	if ev.call != nil {
		// The call's deadline bounded this wait (ADR 0073). The advice to raise agent_placement_wait_sec
		// would be wrong: that key never bounded it. What helps is a later call or a bigger fleet.
		reason = fmt.Sprintf("capacity wait: no node had room before the call's deadline (waited %s; %s; %s%s%s%s) — re-run later or add a node",
			idle.Round(time.Second), ev.call.words(), chain,
			heldOutNote(ev.heldOut, strings.Join(refusals, "; ")), coolingNote(ev.cooling), probeFailureNote(ev.probeFails))
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, waited: true, Unplaced: true, CapacityWaitSec: idle.Seconds(),
		Replacements: len(refusals),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// fencedDefer is the result for a contract whose only local seats a lease FENCES, with the wait
// switched off: deferred, class capacity (the seat's own pre-check files the same class, and the
// delegator re-places it), the fence and its holder named. It never runs the contract.
func (r *runner) fencedDefer(local NodeView, why string, waited time.Duration) PlacedResult {
	reason := why + fmt.Sprintf("; waited %s (agent_placement_wait_sec=%d) - raise agent_placement_wait_sec, add a remote, or wait for the lease to end", waited.Round(time.Second), r.cfg.AgentPlacementWaitSec)
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, PlacementReason: reason, Unplaced: true,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// waitBudgetDefer is the outcome of a wait that ran out of contract budget before
// any node was asked and none refused the subtask (a gate turn-away, a deal's
// overflow): nobody declined the work and nothing ran, so it is a BUDGET defer,
// as runDecided files it, and never a `placement refused` failure.
func (r *runner) waitBudgetDefer(local NodeView, seed PlacedResult, idle time.Duration, spent, note string) PlacedResult {
	reason := spent
	if note != "" {
		reason += " — " + note
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, waited: true, Unplaced: true, CapacityWaitSec: idle.Seconds(),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassBudget,
			Reason:        reason,
		},
	}
}

// shedResult is the sheddable run's outcome when no node had an idle slot:
// deferred at once, class capacity, marked shed.
func (r *runner) shedResult(local NodeView, seed PlacedResult, refusals []string) PlacedResult {
	reason := fmt.Sprintf("shed (priority %d): no node had an idle slot for sheddable work (%d refusal(s): %s) — sheddable contracts take idle capacity only; re-run when the fleet is quieter or drop the priority flag",
		r.priority, len(refusals), strings.Join(refusals, "; "))
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, shed: true, Unplaced: true, Replacements: len(refusals),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// budgetSpent is the exhausted-message tail for a subtask that ran out of
// contract budget rather than out of nodes. One function because it is now
// produced from two places in the loop (before selection and after it), and two
// copies of a sentence drift.
func budgetSpent(remaining, budget, placements int) string {
	return fmt.Sprintf("%ds of the %ds timeout_sec budget was left after %d placement(s), under the %ds floor for another",
		remaining, budget, placements, minRetrySec)
}

// noEligibleRemote turns "nothing eligible" into the cause an operator can act
// on, plus the defer class that goes with it. Distinct situations used to share
// one manufactured sentence that always blamed the gate:
//
//	no remotes configured              → config: nothing was ever asked to run this
//	some remote never answered         → infrastructure: the fleet is down/unreachable
//	all answered, none offers the lane → config: the NODE side really is the reason
//	all answered, none advertises a ceiling → config: an operator sets agent_ctx_tokens
//	the node side is demonstrably fine → contract: only then is the CALLER at fault
//
// DEFAULT TO LOUD — the one rule this function now encodes. The contract check
// used to run FIRST, and two of its three conditions had no node-side guard at
// all, so a caller's missing output_schema published a quiet contract-side
// verdict on a run where NOT ONE node had answered: {succeeded:1,
// infrastructure:0} at exit 0 over a fleet that had been unreachable for a week,
// while the identical fleet state WITH a schema counted infrastructure. So the
// order is inverted and the rule is one line: a QUIET class requires POSITIVE
// evidence that the node side is fine (every configured remote answered, at
// least one offers the agent lane, and it advertises a real ceiling). Absence of
// evidence about the fleet is never evidence about the contract. When both are
// true the reason names both and the CLASS is the loud one — a false alarm costs
// an operator one look, a silent failure costs a night.
// reservedDefer is the result for a contract that could only have run on a
// seat a TEXT lease reserves: deferred, class infrastructure (the box needs a
// human's timing decision, not a rewritten contract), the holder named so the
// caller can wait, route elsewhere, or ask. It never runs the contract.
//
// call is non-nil when the call's deadline bounded the wait (ADR 0073): the advice to set
// agent_lease_wait_sec longer would then be wrong, because the wait already ran as long as the
// call could spare.
func (r *runner) reservedDefer(local NodeView, info gpulease.Info, waited time.Duration, why string, call *callWaitInfo) PlacedResult {
	reason := why
	if !strings.Contains(why, "reserved") {
		reason = "local seat reserved (" + HolderLine(info) + "); " + why
	}
	if call != nil {
		reason += fmt.Sprintf("; waited %s (%s) — add a remote, release the lease, or re-run when it ends", waited.Round(time.Second), call.words())
	} else {
		reason += fmt.Sprintf("; waited %s (agent_lease_wait_sec=%d) — set agent_lease_wait_sec to wait longer, add a remote, or release the lease", waited.Round(time.Second), r.cfg.AgentLeaseWaitSec)
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat,
		PlacementReason: reason,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassInfrastructure,
			Reason:        reason,
		},
	}
}
