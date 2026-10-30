// run_remote.go - Remote execution over the fleet wire: runRemote with its dispatch, poll loop and result assembly.

package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// runRemote drives the fleet wire: dispatch (202 ack, retried once on
// transport doubt under the SAME job id), then poll to a terminal state or
// the poll deadline.
//
// view is the health view of the node being dispatched to. It is the input to
// the auto poll bound (register D-116): what that node advertises about its
// seat is what the delegator's clock is sized from. A zero view simply bounds
// nothing — the cap, as before. runSeat is the seat the decision puts this run
// on ("" when none was decided); it guards the bound against being sized from
// a seat the run will not use.
func (r *runner) runRemote(ctx context.Context, base, jobID string, contract core.AgentContract, view NodeView, runSeat string) PlacedResult {
	var pr PlacedResult
	payload, err := json.Marshal(contract)
	if err != nil {
		pr.Err = "marshaling contract: " + err.Error()
		return pr
	}
	// Queued: the node is about to be asked. The seat named here is the one
	// the decision intends (the layer's seat on a composite node, else the
	// advertised agent seat) and it is what the card is keyed on from now on.
	intendedSeat := runSeat
	if intendedSeat == "" {
		intendedSeat = view.AgentSeat
	}
	r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), []string{view.NodeID}, intendedSeat, "queued", false)
	disp := r.dispatchDetailed(ctx, base, jobID, payload)
	// A 503 is a refusal that returns AT ONCE (ADR 0063). It used to sleep the
	// node's Retry-After here (up to the 300 s the node clamps it to), retry the
	// same node once and charge the sleep to the contract's budget: on 2026-09-29
	// that was 232 refusals and 16 caller-hours asleep on nodes that had just said
	// no while others had room. The hint is carried on the result instead
	// (retryAfterSec); noteCooldown turns it into the node's cooldown, and the
	// capacity wait - which credits what it idles and re-reads health every few
	// seconds - is the only thing that ever waits on it.
	if disp.err != nil {
		pr.Err = disp.err.Error()
		// Carry the class so runOne can decide whether ANOTHER node is worth
		// asking. Set here and nowhere else: this is the one moment at which
		// the node has answered and no seat can possibly hold the contract.
		pr.refused, pr.refusalStatus, pr.retryAfterSec = disp.refused, disp.status, disp.retryAfterSec
		return pr
	}
	// The node ACKED: from here the job can be orphaned by delegator death,
	// so persist the intent before any polling (Option A, intent.go).
	r.intent.dispatched(jobID, base, contract.Goal)
	pr.intentRecorded = true
	// And say so in the ledger now, not when the job ends: a hang, or a ghost that
	// outlives this process, is then visible while it matters (PR-14, ADR 0064).
	r.recordStarted(contract, jobID, jobID, view.NodeID, intendedSeat, "")
	// The card stays queued past the ack: it turns running when a poll shows
	// the node's seat working on the job (seatWorking), not at the ack.
	pairRunning := false
	var pairSince time.Time

	// pollBudget is the budget for WORK. Before the node gained a real queue
	// (0.100.0) that distinction did not exist: the node's Accept was its
	// start, so dispatch → running was microseconds and the whole budget went
	// to execution by construction. Now a job can legitimately sit in the
	// node's backlog in state `accepted`, and charging that wait to the
	// contract's own timeout would convert a node's loud, immediate
	// "503 queue full" into a slow timeout that burns the entire budget and
	// then hands back a manufactured "node accepted the job but did not reach
	// a terminal state" defer — the same lost work with less signal and more
	// wall clock. So QUEUED TIME IS CREDITED BACK: the deadline moves out by
	// the time the job provably spent waiting, and the contract still gets the
	// execution budget it asked for.
	//
	// pollBudgetFor sizes it: the contract's wall + grace, or — for a contract
	// the caller left unsized (register D-116) — the bound sized from what the
	// node ADVERTISES, plus the admission allowance `slack` carried on top of
	// it until a started wall is observed (wallAnchored). pollNote names the
	// source and rides the result (poll_note).
	pollBudget, slack, pollNote := pollBudgetFor(view, contract, runSeat)
	pr.PollNote = pollNote
	start := time.Now()
	// anchor is where the CLOCK is measured from. It is the dispatch instant
	// until the node publishes a started wall, and that wall's start after —
	// so the budget bounds the node's WALL, not the wall plus however long the
	// node spent admitting the job.
	anchor := start
	wallAnchored := false
	// queuedWaitBudget bounds the credit — an unbounded wait is its own
	// failure mode. A job may wait for a slot at most as long as it was
	// allowed to run (pollBudget), and the wait it is GIVEN is derived from the
	// node's own ETA (queueBudgetFor, ADR 0063): 1.5 x the wait a new job faces
	// + 30 s, floored at 60 s. It used to be a fixed five minutes whatever the
	// node said, which abandoned jobs a node was about to start — and the node
	// ran them anyway. Total wall clock is therefore bounded by
	// pollBudget + queuedWaitBudget.
	//
	// queueView is the node's picture the budget is derived from: the placement
	// snapshot at first (a route=spread run's is as old as the run), replaced by a
	// fresh read the moment the job is seen sitting in the node's backlog - see the
	// `accepted` arm below.
	queueView := view
	queuedWaitBudget := queueBudgetFor(queueView, pollBudget)
	// queuedCredit is time PROVABLY spent in the backlog: an interval is banked
	// only when it is bracketed by two CONSECUTIVE `accepted` observations with
	// nothing else in between — see the reset at the top of the poll loop. The
	// partial spans either side (dispatch → first queued poll, last queued poll
	// → first running poll) and any interval interrupted by a transport error,
	// a 404 or a 5xx are deliberately NOT credited. Each omission is at most
	// one pollEvery of real backlog, and under-crediting is the safe direction:
	// over-crediting silently hands a job more execution budget than the
	// contract granted, and makes the give-up message assert the job "waited in
	// the node's backlog" across time the node was doing something else.
	var queuedCredit time.Duration
	var lastQueuedAt time.Time
	// keptInLine counts the times the job was left in the node's line at its budget (stayInLine) and
	// keptWhy is the last reason, for the deferral if it is finally given up.
	keptInLine, keptWhy := 0, ""
	// queuedPolls counts ONLY the polls that actually answered `accepted`.
	// Deliberately not the total poll count: this run may also have seen 404s
	// and 5xxs, and a message saying "N polls, every one queued" over a total
	// that included them would be the delegator authoring a claim the node
	// never made — the exact failure the shapes in unownedDetail exist to
	// prevent.
	queuedPolls := 0
	// The queue-budget refresh (below) is attempted on the first queued polls until one read
	// succeeds, at most maxQueueRefreshTries times: refreshed says one did, refreshErr is
	// the newest failure while none has - the deadline message names it.
	refreshTries, refreshed := 0, false
	var refreshErr error
	// setDeadline is the ONE place the deadline is computed, so no arm can
	// rebuild it from a stale base. Once the clock is anchored on the node's
	// own wall start, the backlog credit is deliberately NOT added again: the
	// anchor already subsumes every second spent before the wall began — the
	// backlog wait included — and adding it twice would hand the run more
	// budget than either clock granted. queuedCredit itself keeps accumulating
	// for the operator-facing message, which reports an observed fact.
	var deadline time.Time
	// Liveness (0.131.0): progressUntil is how far the node's reported progress
	// holds the poll open past `deadline`; the loop stops only when BOTH have
	// passed. lastProgress/lastAllowance feed the deadline message.
	var progressUntil, lastProgress time.Time
	var lastAllowance time.Duration
	extendedOnProgress := 0
	setDeadline := func() {
		credit := queuedCredit
		if wallAnchored {
			credit = 0
		}
		deadline = anchor.Add(pollBudget + credit)
	}
	setDeadline()
	redispatches := 0
	// A poll failure is NOT nothing. Before these, pollOnce's error was bound
	// and dropped, so a node that died after acking (refused dial, per-poll
	// deadline, 500/502/503, a 200 whose body is not JSON) ran out the clock and
	// was handed a MANUFACTURED defer that runOne then stamped with that node's
	// id and seat — the delegator inventing a sentence the node never said, and
	// exiting 0.
	//
	// lastPollErr keeps the newest UNRETIRED failure for the operator: a healthy
	// answer CLEARS it, because one early 503 followed by fifty clean `running`
	// answers is a node that recovered, not a broken one.
	//
	// Two flags, not one, and only the second decides defer-vs-failure:
	//   - sawNodeAnswer: something at that address answered at all. Message text
	//     only — it says "reachable", never "running your job".
	//   - sawJobOwned: a 200 whose state says THIS NODE HOLDS THE JOB
	//     (accepted/running/done/error). That, and only that, earns a defer at
	//     the deadline, because a defer asserts the node accepted the work.
	// A poll 404 is a POSITIVE DENIAL — the node stating it never held the job.
	// It used to set the single answered flag, so a deadline landing inside the
	// re-dispatch window published {deferred, class:budget, "node accepted the
	// job but did not reach a terminal state"} for a job that node never ran.
	var lastPollErr error
	sawNodeAnswer := false
	sawJobOwned := false
	// saw404 records that a 404 ACTUALLY happened. The failure message used to
	// print the 404-denial sentence for every answering node, so a node that
	// returned only 503s published "a poll 404 DENIES it ever held it" — a denial
	// nobody made, authored by the delegator, which is the exact fabrication the
	// failure path exists to prevent.
	saw404 := false
	// lastState is the last state the node reported for the job ("" = it never
	// answered): a give-up asks the node to withdraw a job only while it can still
	// be unstarted. ask is the one withdraw this loop's queue-deadline arm may ask
	// for, and what the node answered when it was not a confirmation: the arm asks
	// once, and a give-up that follows a `running` answer — a cancel or a deadline
	// that lands before the next poll has answered, or a node that then says
	// `accepted` again — reads it instead of asking again, and can still say what
	// the node was told and what it said.
	lastState := ""
	var ask withdrawAsk
	// pollFails bounds the failure logging (both arms below fired once PER
	// POLL) and summarizes on the way out, whichever exit is taken.
	pollFails := newPollFailLog(jobID, base)
	defer pollFails.summarize()
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			// The node may still start it — recovery's case — unless it confirms it
			// took the job back (ADR 0064). A withdraw that was asked and not confirmed
			// says why on the row.
			if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
				pr.Err += "; " + note
			}
			return pr
		}
		if now := time.Now(); now.After(deadline) && now.After(progressUntil) {
			if !sawJobOwned {
				// Never a defer: nothing on that node ever reported OWNING this
				// job, so there is no defer to report. A FAILURE (Summary.Failed,
				// non-zero CLI exit) is the honest outcome — a broken node, or one
				// denying the job, must read broken.
				pr.Err = fmt.Sprintf("poll deadline after %s%s: %s%s", pollBudget, queuedNote(queuedCredit), unownedDetail(sawNodeAnswer, saw404, redispatches, lastPollErr), boundNote(pollNote, slack))
				// The node ACKED this job, so it may hold it however little it has said
				// since, and nothing observed it end: the intent must not claim so. Ask the
				// node to take the job back like every other give-up — a confirmation
				// closes the intent, anything else leaves it for the recovery pass (ADR 0064).
				if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
					pr.Err += "; " + note
				}
				return pr
			}
			// Roast delta 14: mark deferred, reason PREFIXED "poll deadline"
			// (stable key), and STOP. The node may still finish server-side; the
			// job id in the telemetry line lets an operator reconcile by hand.
			// The wording says only what is known: it acked and it never reached
			// a terminal state — not that it "could not complete the contract".
			reason := fmt.Sprintf("poll deadline after %s%s: node accepted the job but did not reach a terminal state%s", pollBudget, queuedNote(queuedCredit), boundNote(pollNote, slack))
			if !lastProgress.IsZero() {
				// The node reported liveness and it went STALE: say when it
				// last moved and what it was allowed, so the reader can tell a
				// node that stopped reporting from one that never did.
				reason += fmt.Sprintf("; the node's last progress was %.0fs ago against a %.0fs allowance", time.Since(lastProgress).Seconds(), lastAllowance.Seconds())
				if extendedOnProgress > 0 {
					reason += fmt.Sprintf(" (the poll was extended on progress %d time(s) past its %s budget)", extendedOnProgress, pollBudget)
				}
			}
			// A node that answered every poll normally simply ran out of clock:
			// a BUDGET defer. One whose last answer we could not use (a 5xx, an
			// unknown state) is a broken box, and classing the two alike is how
			// a broken node keeps reading as a slow one.
			class := core.DeferClassBudget
			if redispatches > 0 {
				// It also LOST the job at least once. unownedDetail renders the
				// re-dispatch count on the FAILURE path and this arm discarded it,
				// so a node that dropped the work twice and then answered normally
				// to the deadline published the same quiet budget defer a
				// healthy-but-slow node earns — with nothing anywhere saying it had
				// lost the job. A node that forgets jobs is broken, not slow.
				reason += fmt.Sprintf(" (the node LOST the job %d time(s) first; each was re-dispatched under the same id)", redispatches)
				class = core.DeferClassInfrastructure
			}
			if lastPollErr != nil {
				reason += " (last poll error: " + lastPollErr.Error() + ")"
				class = core.DeferClassInfrastructure
			}
			// Acked, owned, no terminal state seen — recovery's case. Unless the node
			// still holds the job as `accepted` and confirms it takes it back: a job
			// that never started must not be run later for nobody (ADR 0064), and the
			// reason then says what became of it.
			note := r.giveUp(ctx, base, jobID, lastState, ask, &pr)
			if pr.withdrawn {
				reason += "; the job never started and was withdrawn from the node"
			} else if note != "" {
				reason += "; " + note
			}
			pr.Result = core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, DeferClass: class, Reason: reason}
			return pr
		}
		poll, perr := r.pollOnce(ctx, base, jobID)
		state, data, jobErr, status := poll.State, poll.Data, poll.JobErr, poll.Status
		// Liveness (0.131.0, ADR 0055): a node that reports PROGRESS is polled
		// past any clock sized in advance — the job stays alive while its last
		// progress event is inside the node's own stall allowance (+ grace),
		// bounded by the node's ceiling. A node that reports none is polled
		// exactly as before; a node whose report goes stale is not extended.
		if status == http.StatusOK && poll.Progress != nil && poll.Progress.LastProgressMs > 0 {
			lastProgress = time.UnixMilli(poll.Progress.LastProgressMs)
			allow := time.Duration(poll.Progress.AllowanceMs) * time.Millisecond
			if allow <= 0 {
				allow = 60 * pollSecond
			}
			lastAllowance = allow
			until := lastProgress.Add(allow + pollGrace)
			if poll.Progress.CeilingSec > 0 {
				if cap := anchor.Add(time.Duration(poll.Progress.CeilingSec)*pollSecond + pollGrace); until.After(cap) {
					until = cap
				}
			}
			if until.After(progressUntil) {
				if until.After(deadline) && !progressUntil.After(deadline) {
					extendedOnProgress++
				}
				progressUntil = until
			}
		}
		if !pairRunning && perr == nil && status == http.StatusOK && state == "running" {
			if pairSince.IsZero() {
				pairSince = time.Now()
			}
			if seatWorking(poll.Progress, pairSince, time.Now()) {
				pairRunning = true
				r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), []string{view.NodeID}, intendedSeat, "running", false)
			}
		}
		// The node's OWN wall, published while the job runs (jobWire
		// `wall_sec`, register D-116), beats every estimate: it is the number
		// the run is actually executing under. It can only RAISE the bound —
		// the delegator must never abandon a job the node is still running
		// inside its own wall — and a node too old to publish it changes
		// nothing. Only for an auto contract: an explicit timeout_sec is the
		// caller's own number and the node runs exactly it.
		if contract.TimeoutAuto && status == http.StatusOK && poll.WallSec > 0 {
			switch {
			case !wallAnchored:
				// FIRST started wall (review finding 1). The node stamps
				// wall_sec at the line that opens its wall context, so this is
				// the delegator's only observation of where the node's clock
				// actually began — after the cordon, the pre-flight, the cold
				// load and the coherence probe. Re-anchor here and drop the
				// admission allowance: from this instant the honest budget is
				// the node's own wall plus transport slack, and nothing else.
				wallAnchored = true
				anchor = time.Now()
				slack = 0
				pollBudget, pollNote = anchoredPollBound(poll.WallSec)
				pr.PollNote = pollNote
				setDeadline()
			default:
				// Already anchored: only a LARGER wall moves anything, and it
				// can only ever raise (raisedPollBound).
				if raised, note, ok := raisedPollBound(pollBudget, poll.WallSec); ok {
					pollBudget = raised
					pollNote = note
					pr.PollNote = note
					setDeadline()
					queuedWaitBudget = max(queuedWaitBudget, queueBudgetFor(queueView, pollBudget))
				}
			}
		}
		// EVERY observation closes the queued span; only the `accepted` arm
		// below re-opens it. Reset-then-re-arm is deliberate structure, not
		// style: lastQueuedAt used to be cleared only in the `running` arm, so
		// the transport-error, 404 and 5xx arms all left it set — and an
		// interval bracketed by two `accepted` answers was banked IN FULL even
		// when the node spent the middle of it returning 503s, or denying with
		// a 404 that it had ever held the job. The endpoints being `accepted`
		// does not make the interior backlog wait. Written this way a future
		// poll arm cannot silently inherit an open span.
		prevQueuedAt := lastQueuedAt
		lastQueuedAt = time.Time{}
		switch {
		case perr != nil:
			// Transient transport noise: keep polling until the deadline —
			// the deadline, not one dropped packet, decides abandonment. But
			// RECORD it: this is the only trace a dead node leaves.
			lastPollErr = perr
			pollFails.note(perr)
		case status == http.StatusNotFound:
			// The node answered — with "I have no such job". That is the
			// lost-ack shape AND a positive denial of ownership, so it sets
			// sawNodeAnswer (reachable) and never sawJobOwned: at the deadline
			// this must read as a failure, not as a node that "accepted the job".
			// Re-dispatch the SAME id — the store's duplicate path re-acks
			// 202 without a second run — bounded so a state-losing node
			// cannot be made to re-run the contract forever.
			sawNodeAnswer, saw404 = true, true
			// LOGGED (bounded by the shape cap) because this arm recorded
			// nothing at all: a node that keeps losing jobs left no trace
			// anywhere except a dispatch counter nobody prints.
			pollFails.note(fmt.Errorf("poll: 404 — the node denies ever holding job %s", jobID))
			if redispatches >= maxRedispatches {
				pr.Err = fmt.Sprintf("node lost job %s %d times (poll 404 after re-dispatch)", jobID, redispatches+1)
				pr.nodeTerminal = true
				return pr
			}
			redispatches++
			// A refusal HERE is deliberately NOT marked re-placeable. This node
			// already acked this job id once; its 404 denies holding it NOW,
			// which is not a promise it never ran it and never will. Moving the
			// contract to a second node from inside the polling window is the
			// one shape that could arrange two concurrent runs, and no amount of
			// saved work is worth buying that.
			if _, _, err := r.dispatch(ctx, base, jobID, payload); err != nil {
				pr.Err = err.Error()
				return pr
			}
		case status == http.StatusUnauthorized:
			pr.Err = "poll: 401 unauthorized (fleet_auth_token mismatch)"
			// A refusal of THIS process's credentials is a fact about the caller, not
			// about the job (ADR 0064, decision 6): the node may still hold or finish
			// it, so the intent stays open for a process holding the right token. No
			// withdraw is sent — it would carry the token that was just refused.
			pr.orphanable = true
			return pr
		case status == http.StatusOK && state == "done":
			sawNodeAnswer, sawJobOwned = true, true
			var wire core.AgentWireResult
			if uerr := json.Unmarshal(data, &wire); uerr != nil {
				pr.Err = "job done but data is not an AgentWireResult: " + uerr.Error()
				pr.nodeTerminal = true
				return pr
			}
			pr.Result = wire
			pr.Node = wire.NodeID
			pr.Seat = wire.Seat
			if !wire.Deferred {
				pr.gradeAcceptance(contract, wire)
				r.noteFingerprint(base, contract, wire, pr.AcceptanceFailures)
			}
			return pr
		case status == http.StatusOK && state == "error":
			pr.Err = "remote job error: " + jobErr
			pr.nodeTerminal = true
			if neverRan(jobErr) {
				// The node took this job out of its backlog without running it: reaped
				// because nobody polled it within the poll lease (this process was away
				// for longer — a suspended host, a partition), or withdrawn. Nothing ran,
				// so offering the subtask to another node cannot arrange a double run: it
				// is filed as the capacity refusal a confirmed withdrawal is, and the
				// re-placement machinery takes it from here (ADR 0064).
				pr.refuseAsNeverRan(jobErr, queuedCredit)
			}
			return pr
		case status == http.StatusOK && (state == "accepted" || state == "running"):
			// The node answered AND says it owns the job: the only shape that
			// earns a defer at the deadline.
			sawNodeAnswer, sawJobOwned = true, true
			lastState = state
			// `accepted` and `running` are no longer the same fact. Since
			// 0.100.0 `accepted` means ADMITTED BUT NOT STARTED — the job is in
			// the node's backlog waiting for one of its concurrency slots — and
			// `running` means a seat is actually working on it. A node too old
			// to have a queue simply never lingers in `accepted`, so it accrues
			// no credit and behaves exactly as it did before.
			if state == "accepted" {
				queuedPolls++
				if !refreshed && refreshTries < maxQueueRefreshTries {
					// The job is sitting in the node's backlog, so the ETA the wait was
					// derived from is what matters now - and it may be a snapshot minutes old
					// (route=spread deals once, at the start of the run). Read the node again
					// and EXTEND the wait when its ETA is longer than the snapshot said; a
					// shorter one changes nothing (the job will simply start sooner). One GET
					// per attempt, only for a job that is actually queued, and it fails open to
					// the snapshot - but not silently: a failed read is counted and logged with
					// the poll failures, tried again on the next queued poll (up to
					// maxQueueRefreshTries), and named in the queue-deadline message if it
					// never succeeded, because the budget the job is then abandoned at is one
					// nobody re-checked.
					refreshTries++
					rctx, cancelRead := context.WithTimeout(ctx, pollRequestTimeout)
					fresh, ferr := FetchNodeView(rctx, base, r.cfg.FleetAuthToken)
					cancelRead()
					if ferr == nil {
						refreshed, refreshErr = true, nil
						queueView = fresh
						queuedWaitBudget = max(queuedWaitBudget, queueBudgetFor(queueView, pollBudget))
					} else {
						refreshErr = ferr
						pollFails.note(fmt.Errorf("queue-budget refresh: %w", ferr))
					}
				}
				now := time.Now()
				if !prevQueuedAt.IsZero() {
					queuedCredit += now.Sub(prevQueuedAt)
					if queuedCredit > queuedWaitBudget {
						queuedCredit = queuedWaitBudget
					}
					// Push the execution deadline out by the wait. Recomputed
					// from the anchor rather than incremented, so a capped
					// credit cannot drift the deadline past the intended bound.
					setDeadline()
				}
				lastQueuedAt = now
				if queuedCredit >= queuedWaitBudget && queuedWaitBudget < pollBudget {
					// The budget is where the delegator asks whether there is a better place (stayInLine,
					// ADR 0063 amendment 2026-10-10): a job waiting for a busy node's worker with nowhere
					// better to go stays in line, since withdrawing it would put it at the back of the
					// same line. It is asked about again a step later, up to the contract's patience.
					if why, stay := r.stayInLine(ctx, base, contract); stay {
						queuedWaitBudget = min(queuedWaitBudget+queueStayStep(), pollBudget)
						keptInLine, keptWhy = keptInLine+1, why
					}
				}
				if queuedCredit >= queuedWaitBudget {
					// Bounded give-up while STILL QUEUED. Deliberately an
					// ERROR, not a defer, for three reasons — and note that
					// "a defer carries the node id and seat" is NOT among them:
					// runOne stamps those onto the failure path too.
					//
					//  1. A defer manufactures an AgentWireResult{Deferred:
					//     true} — a payload shaped like something the SEAT
					//     produced. No seat ever saw this contract.
					//  2. The only honest class would be `budget`, which
					//     teaches every consumer of that class (the sizing
					//     path especially) that the seat needed more time.
					//     The seat needed nothing; it was never asked.
					//  3. Decisive: what this replaces was ALREADY A FAILURE.
					//     Before the node had a queue, a saturated node
					//     answered 503 at dispatch, which becomes pr.Err and
					//     counts in summary.failed. Filing this as a defer
					//     would quietly downgrade the severity of a refusal
					//     while changing nothing about the work not getting
					//     done.
					//
					// The message says only what was observed: the node acked
					// it, and N polls answered `accepted`. The "queue deadline"
					// prefix is stable and distinct from the "poll deadline"
					// one a job that actually ran produces.
					//
					// Before giving up, ask the node to take the job back (ADR 0064):
					// a job left in its backlog runs later on a seat nobody is waiting
					// for. One try, and one only — a `running` answer is final for
					// this job.
					verdict := withdrawUnconfirmed
					if !ask.tried {
						ask.tried = true
						verdict, ask.why = r.withdraw(ctx, base, jobID)
					}
					if verdict != withdrawStarted {
						// The deadline is the ETA-derived budget above (ADR 0063), and its refresh note
						// says when that budget was never re-checked. It comes BEFORE the withdraw clause:
						// how the wait was sized, then what was done at its end, so a row that says the
						// node did not take the job back still ENDS with that clause (ADR 0064).
						pr.Err = fmt.Sprintf("queue deadline after %s: the node accepted the job but never started it — it waited in the node's backlog and never reached running (%d poll(s) answered `accepted`)",
							queuedWaitBudget, queuedPolls) + queueRefreshNote(refreshErr, refreshTries) + keptInLineNote(keptInLine, keptWhy)
						if verdict == withdrawConfirmed {
							// The node took it back: it will never run there, so the
							// subtask may be offered to another node, and the intent
							// has nothing left for recovery to collect.
							pr.Err += "; the job was withdrawn from the node, which will never run it"
							pr.refuseAsWithdrawn(queuedCredit)
						} else {
							// Still queued on the node — it may run later; recovery's case. The
							// row says what the node answered instead of taking it back: an old
							// node with no route and an upgraded node that refused would
							// otherwise read alike.
							if note := notConfirmed(ask.why); note != "" {
								pr.Err += "; " + note
							}
							pr.orphanable = true
						}
						return pr
					}
					// `running`: the job left the backlog between the last poll and the
					// withdraw — a slot took it. It is not abandoned; keep polling and
					// let liveness govern from here. The node's answer stays in ask: a
					// give-up that comes before the next poll reads it, and never asks
					// again.
				}
			}
			// `running`: the span stays closed by the reset above. Anything
			// already banked stays banked — the job earned that credit.
			// A healthy answer RETIRES the previous failure. lastPollErr was
			// assigned and never cleared, so ONE early 503 followed by fifty
			// clean `running` answers still ended classed infrastructure, exited
			// non-zero, and quoted an error fifty polls stale — contradicting
			// the deadline branch's own comment about a node that "answered
			// every poll normally". The pollFails counter keeps the history.
			lastPollErr = nil
		default:
			// Anything else IS an answer we cannot act on — a 500/502/503 from
			// the node or something in front of it, or a 200 carrying a state
			// this delegator does not know. Something answered, so this is not
			// the never-answered shape; keep polling (the state may still
			// resolve) but record it, because silently discarding it is what let
			// a broken node look like a busy one. It does NOT prove ownership:
			// a 503 from a proxy says nothing about what the node holds.
			sawNodeAnswer = true
			lastPollErr = fmt.Errorf("poll: unusable answer (status %d, state %q)", status, state)
			pollFails.note(lastPollErr)
		}
		select {
		case <-ctx.Done():
			pr.Err = "canceled: " + ctx.Err().Error()
			// A give-up like the one at the top of the loop, and it used to skip the
			// orphanable mark: a cancel that landed while the delegator slept closed
			// its intent as "terminal observed" for a job the node may still run.
			if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
				pr.Err += "; " + note
			}
			return pr
		case <-time.After(jitteredWithin(pollEvery, time.Until(deadline))):
		}
	}
}
