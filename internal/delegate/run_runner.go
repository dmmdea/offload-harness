// run_runner.go - The per-run runner state, its placement decision and the per-contract runOne flow.

package delegate

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0052), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

type runner struct {
	cfg     config.Config
	local   LocalRunner
	route   string
	remotes []string
	// pin is what the caller asked of placement with route local or remote (ADR 0078), resolved once at intake.
	// route above is the route the run APPLIES: a reasonless local or remote through a door that offers pin_reason
	// is a hint and is applied as auto (pin.go), so every site below that reads r.route sees the route placement
	// uses, and pin is what the ledger rows, the results and the pin accounting say the caller asked.
	pin pinCall
	// tally is RunOptions.PinTally; nil = the call's pins are not counted.
	tally *PinTally
	// remoteRetryOnly is RunOptions.RemoteRetryOnly; retryOffLocal is what reads it.
	remoteRetryOnly bool
	// hintSeatFree is, for a remote hint, whether the local seat read free (no lease, no full line, no load in
	// progress, no vLLM seat to evict) when the deal was made, before the hint dealt it as unavailable: the deal
	// may fall back to the seat only then. Written once before any goroutine starts, read-only after.
	hintSeatFree bool
	// intent is the delegator-death durability ledger (Option A, intent.go).
	// nil = inert (state root unresolvable); dispatch never depends on it.
	intent *intentLedger
	led    *ledger.Ledger
	// pair reports each subtask to NVIDIA PAIR's Jobs list (pairevents.go);
	// inert unless pair_workloads_enabled and PAIR is installed.
	pair *pairworkloads.Emitter
	// ledgerUnopened marks the TOTAL-loss case: a LedgerPath was configured and
	// ledger.Open failed, so there is no handle to try. record() must still
	// count a lost row per result — with the plain `if r.led != nil` guard it
	// counted nothing, LedgerRowsLost stayed 0, omitempty dropped it, and the
	// worst possible telemetry outcome published byte-identically to the best.
	ledgerUnopened bool
	// warnCorpus/warnLedger fire at most ONE warning each per Run. Once, not
	// per subtask: an 8-subtask fan-out against a full disk would otherwise
	// print the same line eight times and bury the results it is warning about.
	warnCorpus sync.Once
	warnLedger sync.Once
	// warnMarker is the same once-per-run rule for the dispatch marker (telemetry.go
	// recordStarted). A marker is a row the run does not OWE, so its loss stays out of
	// the ledgerLost tally; the warning is its only trace.
	warnMarker sync.Once
	// probeWarned bounds fetchViews' per-remote failure warning to ONCE PER
	// BASE PER RUN. fetchViews runs inside runOne, so the warning fired once
	// per remote PER SUBTASK — an 8-subtask fan-out against two dead remotes
	// printed sixteen identical lines, the exact hazard warnCorpus/warnLedger
	// exist to avoid. Keys are base URLs; values are unused.
	probeWarned sync.Map
	// probeMu guards the two probe caches fetchViews keeps FOR THIS RUN.
	//
	// probeMemo is the fleet snapshot the run's sibling subtasks share
	// (fetchViewsMemoTTL): route=spread always amortised its probe with one
	// fetch at run start, and auto/remote/re-placement/retry each paid their
	// own — per subtask, serially, at fetchNodeViewTimeout per unreachable
	// remote. probeDead is the negative cache: a base that failed at the
	// TRANSPORT is not re-dialled for probeNegativeTTL, and its reason is
	// replayed into probeErrs so the placement note still names it.
	//
	// Both are per RUNNER, which is per Run: a snapshot must never outlive the
	// call that took it, and a box that was down an hour ago must not be
	// pre-judged by a Run starting now.
	probeMu   sync.Mutex
	probeMemo *probeSnapshot
	probeDead map[string]deadProbe
	// cool is the run's per-node cooldown (ADR 0063): a node that refused a
	// dispatch for CAPACITY is not asked again before the instant recorded here.
	// Shared by every subtask of the run, because "node A is full" is a fact
	// about the node, not about the subtask that found out.
	cool cooldowns
	// localSlotMu/localSlotAt/localSlotFree memoise the local seat's run-cap
	// reading for fetchViewsMemoTTL: the capacity wait asks once per tick per
	// waiting subtask, and twelve waiters must not each read the registry.
	localSlotMu   sync.Mutex
	localSlotAt   time.Time
	localSlotFree bool
	localSlotNote string
	// localSlotWarn logs the local run registry's unreadable fail-open once per run.
	localSlotWarn sync.Once
	// localBusyMu/localBusyAt/localBusyRd memoise the local seat's LOAD reading
	// (probeLocalBusy) for the same reason: the capacity wait of a subtask a deal kept
	// off a busy seat re-reads it every tick, and twelve waiters must not each ask the
	// engine.
	localBusyMu sync.Mutex
	localBusyAt time.Time
	localBusyRd busyReading
	// corpus*/ledger* count telemetry rows ATTEMPTED and LOST across the run
	// (atomic: record() runs on every subtask goroutine). The once-per-run
	// warnings above say THAT telemetry failed; these say how much, which is
	// the difference between a transient and a full disk.
	corpusTried atomic.Int64
	corpusLost  atomic.Int64
	ledgerTried atomic.Int64
	ledgerLost  atomic.Int64
	// spreadViews/Bases/ProbeErrs are the ONE fleet snapshot a route=spread run
	// deals its subtasks across (fetched in Run, read-only afterwards).
	spreadViews     []NodeView
	spreadBases     []string
	spreadProbeErrs []string
	// spreadDeal is the whole run's placement, computed by dealSpread in Run
	// before any subtask goroutine starts and READ-ONLY from then on.
	spreadDeal []spreadSlot
	// spreadLease is the machine-wide GPU lease as read ONCE with the fleet
	// snapshot: a TEXT holder reserves the local seat (Reserved) and it leaves
	// the deal — the whole reason 0.113.14 exists. A media lease is not read by
	// the spread deal (it never was; ADR 0026 arbitrates renders elsewhere).
	spreadLease gpulease.Info
	// spreadLocalBusy is the local seat's in-flight reading taken ONCE with the
	// fleet snapshot (0.113.20): a seat that already holds a request leaves the
	// rotation the way a leased one does (placeSpread). localBusyProbe is the
	// seam tests drive it through; nil = the production probe (seatload).
	spreadLocalBusy busyReading
	localBusyProbe  func(ctx context.Context) busyReading
	// seatGuardCheck is probeLocalBusy's seam onto the seat guard (checkSeatGuard);
	// nil = the process-wide seatguard.Shared.
	seatGuardCheck func(ctx context.Context, model string) seatguard.Verdict
	// autoDealBusy is the reading route=auto's one deal was made from (RunWith), kept
	// so a remote placement can say the local seat was occupied, not merely busy.
	autoDealBusy busyReading
	// autoDealReadBusy records that route=auto's deal (or a per-subtask placement) read the
	// local seat busy and placed around it (dealReadLocalBusy). Atomic: subtask goroutines
	// set it while the capacity wait of a sibling reads it.
	autoDealReadBusy atomic.Bool
	// autoLocalBusyOnce/autoLocalBusy cache route=auto's ONE read of the local
	// seat's load (W-01, register S-01): every subtask of this Run must see
	// the SAME reading — the same one-probe-per-Run invariant spreadLocalBusy
	// already holds for route=spread, extended to auto so a busy local seat
	// (in-flight at or past the fleet's own concurrency cap, or mid-load) is
	// no longer indistinguishable from an idle one just because no GPU lease
	// happens to be held.
	autoLocalBusyOnce sync.Once
	autoLocalBusy     busyReading
	// autoViews/autoBases/autoProbeErrs/autoDeal are route=auto/remote's OWN
	// one-fetch, one-deal snapshot (W-06, register S-11/S-13) — the same
	// invariant spreadDeal holds for route=spread, extended here so
	// runConcurrency sibling subtasks stop probing the fleet and calling
	// Place independently (S-13: "those subtasks probe within milliseconds of
	// each other — so several of them can read the same free slot and then
	// compete for it"). Computed once in RunWith before any goroutine starts,
	// read-only afterwards. Empty/nil on route=local, route=spread and route=
	// queue, which never touch them.
	autoViews     []NodeView
	autoBases     []string
	autoProbeErrs []string
	autoDeal      []spreadSlot
	// dealRoom is the local seat's run-cap room (localRunCapRoom) as the run's deal read it, ONCE, before any
	// goroutine started (ADR 0076): what the auto deal may commit to an idle seat, what the spread deal gives its
	// local slots, and the local share of the call's width (dealParallelism). Written by the deal, read-only after.
	dealRoom int

	// rescue is RunOptions.Rescue; nil = no rescue.
	rescue RescueFunc
	// quarantine is the caller's Quarantine (RunOptions), nil = inert.
	quarantine  *Quarantine
	quarantined atomic.Int64
	// priority / tenant are RunOptions.Priority (clamped) and RunOptions.Tenant,
	// stamped on every dispatch this run makes (see dispatch).
	priority int
	tenant   string
	// beforeForced is a test seam: called with the chosen node's base between the
	// capacity wait choosing a node and dispatching to it, the one window in which
	// another Run of the process can take the node's last gate slot. nil in production.
	beforeForced func(base string)
	// decider is RunOptions.LocalDecider; snapshot is the ONE memoised reader
	// set the production decider reads through (built lazily on the first
	// composite decision, shared by every subtask goroutine — Snapshot is
	// mutex-guarded). Both inert on a non-composite box.
	decider  func(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision
	snapOnce sync.Once
	snapshot *placetable.Snapshot
	// call is this run's whole-call deadline (RunOptions.Deadline), nil = none.
	// Every method on it is nil-safe, so no site needs a guard.
	call *callDeadline
	// pairMu/pairShut fence the PAIR emitter at the end of a run (calldeadline.go:
	// emitPair, shutPair): a goroutine the call deadline abandoned may report after
	// RunWith has begun draining the emitter.
	pairMu   sync.RWMutex
	pairShut bool
	// pairOpen is the run's account of the cards it opened (notePair), which shutPair settles:
	// a card still open when the run ends belongs to a subtask the call deadline abandoned.
	pairOpenMu sync.Mutex
	pairOpen   map[string]*pairCard
	// pairLate makes the "a late PAIR frame was dropped" line once per run.
	pairLate sync.Once
	// lastJob is, per subtask index, the job id of the attempt most recently started
	// (attempt mints one per attempt). A subtask the call gives up on is published under
	// it (abandoned), so the caller can reconcile the late row its goroutine writes.
	lastJob sync.Map
}

// decide is the composite box's placement decision for a contract that is
// about to run LOCALLY: the zero Decision on a plain box (nothing decided,
// nothing published — the byte-identical constraint), the injected decider
// when a caller supplied one, else the production table over the run's
// memoised snapshot. The request is built exactly as the node builds it
// (placetable.RequestForContract with the box's agent_max_tokens), so the
// delegator and the node describe the contract to the table identically.
//
// A contract that NAMES a layer (register A-100) is decided FOR that layer,
// never by the free choice — the same rule remoteDecision applies to a
// remote's rows. On a plain box the name cannot be served at all, and that is
// said as a contract defer naming the layer: the one case a plain box
// publishes a placed block, because the caller asked for a layer and a silent
// run on the planner seat would be a lie about where the work went.
func (r *runner) decide(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision {
	if !r.cfg.Composite() {
		if contract.Layer != "" {
			return placetable.Decision{Placed: core.Placed{Layer: contract.Layer,
				Reason: fmt.Sprintf("layer %s requested; this box declares no layers", contract.Layer)}, Defer: true, DeferClass: core.DeferClassContract}
		}
		return placetable.Decision{}
	}
	if r.decider != nil {
		return r.decider(ctx, contract, st)
	}
	r.snapOnce.Do(func() { r.snapshot = placetable.NewSnapshot(r.cfg, placetable.DefaultSnapshotTTL) })
	req := placetable.RequestForContract(contract, st.EstTokens, r.cfg.AgentMaxTokens)
	if contract.Layer != "" {
		return placetable.DecideOnLayer(req, r.cfg.Layers, contract.Layer, r.snapshot.Live())
	}
	return placetable.Decide(req, r.cfg.Layers, r.snapshot.Live())
}

// placedOrNil is the block a decision publishes: nil for the zero Decision (a
// plain box), the decision's Placed otherwise. One helper so no site can
// publish an empty block with layer "" on a plain box.
func placedOrNil(dec placetable.Decision) *core.Placed {
	if dec.Layer == "" && dec.Reason == "" {
		return nil
	}
	p := dec.Placed
	return &p
}

// placement is a resolved "run it HERE" — the node, its dial base ("" for
// local) and the human-readable reason that rides the result. decided, when
// set on a forced LOCAL placement, is the placement-table decision the run
// must use instead of deciding again: the capacity wait re-ran the decider and
// is handing attempt() the seat it found free (or the expired wait's seat),
// and a second decision inside attempt could contradict it.
type placement struct {
	view    NodeView
	base    string
	reason  string
	decided *placetable.Decision
}

// reportTelemetryLoss emits the ONE end-of-run line naming how much telemetry
// this run lost, out of how much it tried. Silent when nothing was lost, so a
// healthy run's output is unchanged.
func (r *runner) reportTelemetryLoss() {
	if lost := r.corpusLost.Load(); lost > 0 {
		log.Printf("delegate: delegation-log corpus: %d of %d rows lost this run (results unaffected)", lost, r.corpusTried.Load())
	}
	if lost := r.ledgerLost.Load(); lost > 0 {
		log.Printf("delegate: ledger: %d of %d rows lost this run (savings accounting incomplete; results unaffected)", lost, r.ledgerTried.Load())
	}
}

// runOne runs one subtask: a first attempt placed per route, then — when the
// first attempt came back failed_verification or an honest abstention and a
// DIFFERENT node is available — exactly one retry there, publishing the better
// of the two. Measured motivation (2026-08-21): on the same four contracts the
// 27B seat and the 4B seat each missed a different one; acceptance caught both,
// and the retry is what turns "caught" into "recovered".
func (r *runner) runOne(ctx context.Context, i int, contract core.AgentContract) PlacedResult {
	start := time.Now()
	// timeout_sec is the EXECUTION budget, and every mechanism inside runOne
	// spends from this one copy of it: the re-placement loop and the
	// verification retry both measure what is left against this `start`, so
	// neither can silently extend the other's.
	//
	// It is NOT the end-to-end wall — see placeAndRun for what else the wall
	// contains and why none of it can be charged here.
	budget := executionBudgetSec(contract)
	// ONE ledger per subtask, shared by every placeAndRun below. See placements.
	pl := newPlacements()
	first := r.placeAndRun(ctx, i, contract, nil, start, budget, pl)
	if !retryable(first) {
		return first
	}
	// A retryable first attempt is one a node TOOK: it ran the job and its answer was the
	// problem. That seat is recorded here, once, whichever path placed it (the first
	// dispatch, a re-placement, a capacity wait), so the retry's capacity wait can never
	// hand the second chance back to the seat whose answer it exists to correct.
	pl.ran[first.ranBase] = true
	// A research page that failed ONLY its acceptance is not run a second time
	// (register C-74). Guarded here, not inside retryable(): that predicate sees
	// a placed result, never the contract that names the door.
	if skipsRetryAsResearchAcceptanceOnly(contract, first) {
		first.RetryNote = fmt.Sprintf("retry skipped: the first attempt on %s failed only its acceptance, and a research digest is graded against its page — a second seat repeats the verdict at the cost of a whole second run", nodeLabel(first))
		return first
	}
	// An EMPTY final (0.115.8: stop_reason reasoning_starved / empty) is not a
	// wrong answer another seat can correct — it is the seat's completion
	// budget or think block, and a second seat handed the wall's leftovers
	// repeats the shape (2026-09-10: the 4B's 603 s empty final was retried on
	// the 27B with 296 s, which generated 4,178 tokens of think and timed out).
	// The reasoning_starved shape is class budget and never reaches here (kept
	// in the match so the rule reads as one shape, not as a coincidence of the
	// class table); the `empty` abstention does, and is skipped by name.
	if stop := first.Result.StopReason; first.Result.Deferred && (stop == "reasoning_starved" || stop == "empty") {
		first.RetryNote = fmt.Sprintf("retry skipped: the first attempt on %s ended on an empty final (stop_reason %s) — a second seat given the wall's leftovers repeats the shape; the fix is the seat's completion budget or agent_thinking, not a retry", nodeLabel(first), stop)
		return first
	}
	// Credit back what the node's ADMISSION spent before the coherence defer
	// (reviewer finding, D-118). The retry budget is delegator wall clock since
	// `start`, and the defer's own trigger under the default "cold" policy is a
	// COLD LOAD — 125–250 s of a vLLM seat on this fleet, against a 300 s
	// default contract. Left uncredited, the promise this defer is retryable
	// for ("the wall never started, the budget is still on the table") would be
	// refused by the retry floor on the very path that produces it. It is the
	// same mechanism, and the same argument, as the capacity wait's credit:
	// time the subtask provably did not spend WORKING is not charged to
	// timeout_sec.
	pl.credit += admissionCredit(first)
	// The retry lives INSIDE the subtask's own timeout_sec: the caller was told
	// that number bounds the work per subtask, and a second full attempt would
	// have doubled it silently. What is left after the first attempt is the
	// retry's budget; under the floor there is no honest retry to run. The
	// floor is seat-aware through config (agent_retry_min_sec, D-46): a cold
	// load plus one turn at max_tokens on the retry seat, never the bare 10 s
	// that let a 296 s retry burn a thinking seat for nothing.
	// First pass, BEFORE a retry node is chosen: the configured floor and nothing else. This is
	// the cheap refusal; the retry SEAT's floor is applied below once it is known. It used to be
	// raised by the first attempt's own min_turn, but the retry goes to a DIFFERENT node, so that
	// number belongs to a seat the retry will never use (retryFloorFor already exempted the
	// seat-down and lost-root defers for exactly this reason): a seat whose cold load plus two
	// 8,192-token turns is measured at 1,112 s took 20 to 325 s to give the answer, and 7 of the
	// 14 retries skipped in one day were refused for it before any node was asked. A retry seat that publishes
	// no rate still falls back to the first attempt's number in retryFloorOn, said in the note.
	floor, floorSrc := r.firstPassRetryFloor()
	remaining := pl.remaining(start, budget)
	if remaining < floor {
		first.RetryNote = fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after the first attempt (floor %ds%s)", remaining, budget, floor, floorSrc)
		return first
	}
	// alternativeNode BLOCKS — it probes the fleet. Bound that probe by what
	// the contract still has, so a roster of blackholing nodes cannot spend a
	// budget the subtask no longer owns (fetchViews is sequential at
	// fetchNodeViewTimeout per remote and Run derives no deadline of its own).
	altCtx, cancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
	alt, held, ok := r.alternativeNode(altCtx, first, contract, pl)
	cancel()
	if !ok {
		// A fence is a REASON, not a silence: the caller has to be able to tell
		// "there was nowhere else to go" from "the only seat left is behind a
		// measurement that has the cards" (D-94). The call's deadline is a second
		// reason: it ends the fleet read that would have named a node, and an empty
		// note after it reads as "there was nowhere else to go". The clock decides,
		// not altCtx, which also ends on the budget the retry is bounded by. A text reservation
		// of the local seat (C-81) is told the same way: `held` carries either clause, and
		// retryHeldWhy words what follows it. Like a fence it is skipped and not queued for: a
		// retry that found no node has no placement to wait with (ADR 0063, decision 10).
		//
		// The order is the point. "No other node is eligible" and "no other node could take
		// the contract" are claims about nodes, which a read the deadline ended cannot support
		// (ADR 0065), so once the clock has run out the note leads with the deadline, and when
		// the local seat is fenced it keeps that fact behind it: it is read from the lease
		// record and stays true, but the claim about the fleet goes. Next comes the fence on
		// its own, and a seat-down defer's own note (ADR 0066: the second placement it was
		// promised found no node) comes last.
		switch {
		case r.call.reached():
			first.RetryNote = "retry skipped: " + callDeadlinePrefix + " before a retry node was chosen"
			if held != "" {
				first.RetryNote += "; " + held + ", and " + retryHeldWhy(held)
			}
		case held != "":
			first.RetryNote = "retry skipped: " + held + " and no other node is eligible; " + retryHeldWhy(held)
		case SeatDownDefer(first.Result), RootGoneDefer(first.Result), FinalToolCallDefer(first.Result):
			// The defer promises a second placement on another node: when there is no
			// other node the caller must be told it was considered and why it did not
			// happen, not left with a retryable defer and an empty note. The seat-down
			// defer (ADR 0066), the lost-root defer (register C-79) and the forced-final
			// tool-call defer are the ones whose promise is that sentence.
			why := r.noRetryNodeWhy()
			what := "the seat on " + nodeLabel(first) + " went down"
			if RootGoneDefer(first.Result) {
				what = "the run on " + nodeLabel(first) + " lost its workspace root"
			}
			if FinalToolCallDefer(first.Result) {
				what = "the seat on " + nodeLabel(first) + " wrote a tool call as its final answer"
			}
			first.RetryNote = fmt.Sprintf("retry skipped: %s and no other node could take the contract (%s)", what, why)
		case r.retryOffLocal():
			// A reasoned remote pin, or a remote call that asked for its retry to stay on a fleet node, retries on another
			// fleet node or not at all (alternativeNode): when there is no other node, a failed verification, an abstention
			// or an admission defer would otherwise be published with no note and read as a retry nobody considered. The
			// pin or the request is the reason there is no local retry, so the note names it.
			first.RetryNote = fmt.Sprintf("retry skipped: no other fleet node could take the contract after the first attempt on %s (%s); %s",
				nodeLabel(first), attemptOutcome(first), r.retryOffLocalWhy())
		default:
			// Every other skip says why too (the operator's rule: a job's trouble is logged, not silent). A wrong answer or an
			// abstention with nowhere to go used to be published with an empty note, which reads as a retry nobody
			// considered; a research page skipped this way left nothing to log and nothing to fix.
			first.RetryNote = fmt.Sprintf("retry skipped: no other node could take the contract after the first attempt on %s (%s); %s",
				nodeLabel(first), attemptOutcome(first), r.noRetryNodeWhy())
		}
		return first
	}
	// Never land the retry on a seat that is already generating for another
	// job (D-46): the two runs halve each other's tok/s and the retry, on the
	// leftover budget, is the one that dies (2026-09-10: the ledger-01 retry
	// joined the 27B mid-generation of ledger-00 and both crawled at 27 tok/s).
	//
	// A busy seat is a place in line, never a reason to refuse the second chance
	// (INV-4, register C-76): the retry WAITS for the seat to have room - the same
	// capacity wait a refused first placement gets, agent_placement_wait_sec, credited
	// like it - and runs on it the moment it does, so the two runs still never share
	// it. The retry is skipped only when the seat stayed busy for the whole wait (or
	// the wait is switched off), and the note says how long it waited.
	//
	// Not for a defer that produced nothing (producedNothingDefer: the seat-down defer,
	// ADR 0066, the lost-root defer, C-79, and the forced-final tool-call defer): this check
	// neither waits nor skips. D-46's case is a verification retry: the first attempt
	// PRODUCED an answer, the retry is an optional second opinion on the budget that was
	// left, and joining a generating seat costs it the budget it needs. A defer that
	// produced nothing — declining to re-place it, or waiting out the placement wait and
	// then declining, loses the job — and a seat-down retry carries the credited budget of
	// the wait the dead seat cost. Busy
	// is a place in line, never a refusal (INV-4): the node's own queue is the line, and
	// it answers 503 when it is full, which placeAndRun re-places at once (ADR 0063),
	// into the bounded capacity wait when nothing has room. During the 2026-09-29 outage
	// the fleet was saturated (44 runs in flight for ~13 slots), so a check here would
	// have held or refused nearly every re-placement.
	//
	// That line is a REMOTE alternative's. The local seat answers no 503: a run forced
	// onto it joins the run-cap line and waits there for the run's whole wall, a line the
	// subtask cannot leave. alternativeNode therefore sends such a retry to an
	// untried remote while that line has no free slot ahead of a newcomer, so the
	// exemption below never hands the retry to a full local line when there is another
	// place to go.
	if !producedNothingDefer(first.Result) {
		if busy, why := r.retrySeatBusy(ctx, alt); busy {
			w := r.awaitRetrySeat(ctx, alt, why)
			pl.credit += w.waited
			if w.canceled {
				// The wait was ended from outside while the retry stood in line: the seat was
				// not shown to stay busy, so the note must not claim it did. Who ended it is
				// part of the truth: the call's own deadline (ADR 0065) is not the caller
				// cancelling, and the retry's fate is worded with the deadline's stable
				// opening, as it is for a retry the deadline cut while it ran.
				ended := "the caller canceled"
				if r.call.reached() {
					ended = callDeadlinePrefix
				}
				first.RetryNote = fmt.Sprintf("retry skipped: %s after the retry waited %s in line for the retry seat on %s, which was running another job (%s)", ended, w.waited.Round(time.Second), alt.view.NodeID, w.why)
				return first
			}
			if w.stillBusy {
				after := ""
				if w.waited > 0 {
					after = fmt.Sprintf(" after waiting %s in line", w.waited.Round(time.Second))
				}
				first.RetryNote = fmt.Sprintf("retry skipped: the retry seat on %s is already running another job%s (%s); a shared seat would only slow both", alt.view.NodeID, after, w.why)
				return first
			}
		}
	}
	// The floor of the seat the retry will actually run on (D-46 follow-up,
	// 0.117.2): the 2026-09-10 retry cleared a 201 s floor sized from the 4B's
	// numbers and landed on the 27B, whose own min_turn was ≈ 484 s, with 626 s
	// — enough for its loop, not for loop + re-pack. A remote node publishes
	// its seat rate on health; the local seat's is read from this box's store.
	floor, floorSrc = r.retryFloorOn(first, alt, contract)
	// RE-MEASURE after the probe. `remaining` above was true when it was taken
	// and can be minutes stale by now; writing that stale number into the retry
	// contract is what would hand a seat time the subtask no longer has.
	remaining = pl.remaining(start, budget)
	if remaining < floor {
		first.RetryNote = fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after choosing a retry node (floor %ds%s)", remaining, budget, floor, floorSrc)
		return first
	}
	retryContract := contract
	retryContract.TimeoutSec = remaining
	retryContract.TimeoutAuto = false // a retry's wall is what is left — explicit, never auto (D-03)
	second := r.placeAndRun(ctx, i, retryContract, &alt, start, budget, pl)
	merged := mergeAttempts(first, second)
	if r.retryOffLocal() {
		// The retry ran on a fleet node because the pin, or the caller's request, allows no other place (alternativeNode), and
		// the note says so whichever attempt is published: the first attempt it annotates, or the retry that recovered it.
		merged.RetryNote += "; " + r.retryOffLocalWhy()
	}
	return merged
}

// retryWait is what awaitRetrySeat found: how long the retry waited in line (time the
// caller credits to the retry's budget, because the wait is queueing and never work),
// whether the seat was still busy when the wait stopped, whether the context ended it
// (the caller cancelled, or the call's own deadline passed, ADR 0065: neither says
// anything about the seat), and the seat's own last reading.
type retryWait struct {
	waited    time.Duration
	stillBusy bool
	canceled  bool
	why       string
}

// awaitRetrySeat holds a verification retry in line for a seat that is running
// another job: it re-reads the seat every capacity-wait tick (placementPollInterval,
// jittered) until it has room, and gives up after the placement wait
// (agent_placement_wait_sec; a negative value switches the wait off and the retry is
// skipped at once, as before).
func (r *runner) awaitRetrySeat(ctx context.Context, alt placement, why string) retryWait {
	// The retry keeps the TTL under a call deadline too (ADR 0073): it is an optional second opinion on
	// an answer the subtask already holds, and holding that answer for the rest of the call to improve
	// on it would cost the caller more than the retry can return.
	wait := r.placementTTL()
	if wait <= 0 {
		return retryWait{stillBusy: true, why: why}
	}
	begin := time.Now()
	deadline := begin.Add(wait)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
		case <-time.After(jitteredWithin(placementPollInterval, time.Until(deadline))):
		}
		if ctx.Err() != nil {
			break
		}
		busy, w := r.retrySeatBusy(ctx, alt)
		if !busy {
			return retryWait{waited: time.Since(begin), why: ""}
		}
		why = w
	}
	return retryWait{waited: time.Since(begin), stillBusy: true, canceled: ctx.Err() != nil, why: why}
}
