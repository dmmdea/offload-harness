// run_placements.go - Per-contract placement bookkeeping: attempt counts, refusals and node cooldowns.

package delegate

import (
	"fmt"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// placements is ONE SUBTASK's placement ledger. runOne owns it and hands the
// same pointer to every placeAndRun call it makes, which is the whole point:
// the exclusion set and the re-placement bound are properties of the SUBTASK,
// not of one placeAndRun invocation.
//
// Round-3 review found what per-call state actually cost, and neither symptom
// was visible from inside a single call:
//
//   - The bound doubled. runOne calls placeAndRun twice (first attempt, then
//     the verification retry) and each built its own counter, so a subtask
//     could make eight placements while the constant, the CHANGELOG and the
//     docs all said four.
//   - The retry could run on the seat the first attempt already used. Traced:
//     route=auto with the local GPU busy, two remotes refuse, the first attempt
//     falls to LOCAL and fails acceptance; the retry is forced remote, that
//     remote refuses, the bound trips, and the retry's own fresh map has no
//     record of local — so it lands local a second time and publishes
//     "this result is the retry" over two runs of the same seat. That defeats
//     the measured premise of the retry (the 27B and the 4B each missed a
//     DIFFERENT contract); a retry on the same seat is not a second opinion.
type placements struct {
	// tried is every dial base this subtask has been placed on ("" = the local
	// seat). EVERY attempt records itself here, including one that SUCCEEDED —
	// the retry's whole premise is a different seat, so the seat that just
	// answered has to be excluded from it.
	tried map[string]bool
	// used counts remote re-placements spent across the whole subtask.
	used int
	// credit is time this subtask spent WAITING FOR CAPACITY (awaitCapacity):
	// idle polling, never an attempt. It is not charged to timeout_sec — the
	// same rule as time provably spent queued on a node — so remaining() adds
	// it back. The idle polling is bounded by agent_placement_wait_sec. Two spans of
	// queueing also land here when their attempt is refused (noteRefusal): the time a
	// job provably sat in the backlog of a node that then took it back (ADR 0064;
	// bounded by the queue budget) and the admission wait a local capacity defer
	// already spent in the seat's own line.
	credit time.Duration
	// capacityRefusal records that the subtask's trouble is CAPACITY (a node
	// refused it with a 503/429: queue full, leased, draining, shed; or
	// replacementNode found an eligible node, or the local run-cap line, merely
	// busy: no room, a cooldown, a backlog past the caller's patience, a full
	// process gate) rather than a request or an address that was wrong. Only then
	// is a capacity wait worth running: a roster that 404s, is unreachable or can
	// never run the contract does not free up.
	capacityRefusal bool
	// excluded is every dial base that refused for a NON-capacity reason
	// (404/408/409, or unreachable): nothing about such a node frees up, so
	// the capacity wait never asks it again, however much room its health
	// claims. A capacity refusal (503/429) gets a cooldown instead.
	excluded map[string]bool
	// waitNote is why the subtask went to the capacity wait when nothing REFUSED it:
	// the process gate turned its dispatch away, or a deal handed it over because
	// every node with room was already dealt to its headroom. It is narration only -
	// carried on the landing's placement reason and the defer's reason - and never
	// counted as a refusal or a replacement.
	waitNote string
	// ran is every dial base ("" = the local seat) that TOOK a job of this subtask:
	// it ran the job or answered it, and did not decline it at dispatch. A node that
	// REFUSED for capacity is in tried but not here, because the capacity wait's whole
	// job is to ask it again; one that took the first attempt is excluded from the
	// wait's candidates, because the verification retry's premise is a DIFFERENT seat
	// from the one whose answer it corrects.
	ran map[string]bool
	// attempts counts REAL placements (a dispatch or a local run — anything
	// that went through attempt()'s finish and so wrote its telemetry row).
	// A subtask that ends with none of them (reserved seat, nothing freed) is
	// recorded by settle(); one that made at least one is already in the
	// corpus and the ledger through that attempt, the way exhausted() relies on.
	attempts int
	// lastRowRefusal: the row of the latest attempt was written as a refusal (PlacedResult.refusalRow). A subtask
	// that ends without a node taking it has no row of its own then, and settle writes one.
	lastRowRefusal bool
}

// isRefusalRow: the row of this attempt is a node's capacity refusal AT DISPATCH, a place in line. A job the node
// acked and then took back (refuseAsWithdrawn, refuseAsNeverRan: a queue deadline it confirmed, a record that says
// it never ran) is filed with the same 503 but is not that: the node held it under an id the delegator's intent
// ledger and the node's own record know, and when nothing else takes the subtask that row, under that id, is the
// subtask's record.
func isRefusalRow(pr PlacedResult) bool {
	return pr.refused && pr.Err != "" && capacityRefusal(pr.refusalStatus) && !pr.withdrawn && pr.nodeNeverRan == ""
}

// countAttempt files one attempt that went through attempt's finish and so wrote its row.
func (pl *placements) countAttempt(pr PlacedResult) {
	pl.attempts++
	pl.lastRowRefusal = pr.refusalRow
}

// settle is the telemetry close-out for an outcome NO attempt produced —
// a capacity defer, a shed, or the holder-naming deferral reached without a
// single dispatch. attempt()'s finish records every real placement; these
// results are built outside it, and before this helper a subtask whose whole
// life was "reserved seat, nothing freed" left zero corpus rows and zero
// ledger rows (review finding, 2026-09-06). Recorded once, only when the
// subtask made no real attempt at all — otherwise the last attempt's row
// already stands, exactly as exhausted() leaves it.
func (r *runner) settle(contract core.AgentContract, pr PlacedResult, pl *placements, since time.Time) PlacedResult {
	// A capacity wait (or a lease wait) that ended because the CALL did is the call
	// deadline, not "no node had room" (ADR 0065). It quotes nothing of the wait's own
	// text: the placement narration carries that history.
	cutBefore := pr.deadlineCut
	pr = r.cutOutcome(pr, false)
	cutHere := pr.deadlineCut && !cutBefore
	// A refused attempt's row is a refusal, not an outcome (delegationLogLine.Refusal), so when the last attempt's
	// row is one, no row says how the subtask ended: this one does, as a cut's does.
	closing := cutHere || pl.lastRowRefusal
	if pl.attempts > 0 && !closing {
		return pr
	}
	// A wait the deadline ended after refused attempts (ADR 0065 decision 2) is recorded
	// here like any other cut: no attempt produced this outcome, so no attempt's row can
	// say it. The row gets a job id of its own — capacityDefer copies the id of the last
	// REFUSED dispatch, whose row already says "dispatch ... 503" for a job no node held,
	// and a second row under it would double-count one id — and the caller is given that
	// id, so the result, the ledger row and the corpus row are one thing.
	if pr.JobID == "" || (closing && pl.attempts > 0) {
		pr.JobID = mintJobID()
	}
	pr.refusalRow = false
	pr.wallMs = time.Since(since).Milliseconds()
	r.record(contract, pr)
	return pr
}

func newPlacements() *placements {
	return &placements{tried: map[string]bool{}, excluded: map[string]bool{}, ran: map[string]bool{}}
}

// noteRefusal files a refused attempt in the ledger: a capacity refusal (a 503 or
// 429) makes the subtask wait-worthy; any other refusal excludes that node from the
// wait. The local seat's own capacity defer files nothing here on purpose: it is a
// refusal to LIST (refusalLine) but not, by itself, a reason to wait - replacementNode
// marks the subtask wait-worthy when some ELIGIBLE node is merely busy, and a defer
// beside a remote that can never run the contract would otherwise hold the subtask for
// the whole placement wait to publish the same defer.
func (pl *placements) noteRefusal(pr PlacedResult) {
	// Time the refused job provably spent QUEUED on a node that then took it back
	// (a confirmed withdrawal at the queue deadline) is not execution: it is
	// credited like every other span the subtask spent waiting rather than
	// working, so the re-placement still owns the budget the contract asked for.
	pl.credit += pr.queuedWait
	if capacityRefusal(pr.refusalStatus) {
		pl.capacityRefusal = true
		return
	}
	if pr.ranBase != "" {
		pl.excluded[pr.ranBase] = true
	}
}

// noteRefusal is the runner's filing of a refused attempt: the subtask's own
// ledger (placements.noteRefusal), the run's per-node cooldown for a capacity
// refusal, and the time a LOCAL capacity defer already spent waiting in the seat's
// own line - which is queueing, so it is credited back exactly as the capacity
// wait's idle time is and never charged to the contract's execution budget.
func (r *runner) noteRefusal(pl *placements, pr PlacedResult) {
	pl.noteRefusal(pr)
	r.noteCooldown(pr)
	if capacityDeferRefusal(pr) && pr.Result.AdmissionWaitSec > 0 {
		pl.credit += time.Duration(pr.Result.AdmissionWaitSec * float64(time.Second))
	}
}

// cooldowns is a run's per-node "do not ask before" instants, keyed by dial base.
// The zero value is ready to use.
type cooldowns struct {
	mu    sync.Mutex
	until map[string]time.Time
	// at is when the latest refusal that held a node was noted: a health read taken before it is the
	// picture that refusal just proved wrong, so it can say nothing about the hold (lift).
	at map[string]time.Time
	// lifted marks a node whose hold a post-refusal health read lifted once this run, and firm a node
	// that refused again after that: its counters did not predict its refusals, so its Retry-After
	// stands for the rest of the run. One lift is the bound on how often a node whose health reports
	// free workers while it keeps answering 503 (a stale VRAM snapshot, say) can be re-asked early.
	lifted map[string]bool
	firm   map[string]bool
	// liftNote is, per base, the words for a lift that has not yet been followed by anything else: the
	// note and the instant the hold it ended would have run out on its own. A new hold (the node refused
	// after the lift) forgets it, and so does liftNarration once that instant has passed.
	liftNote map[string]liftedCooldown
}

// liftedCooldown is a Retry-After hold that a health read ended early (cooldowns.lift): the words that
// narrate it, and when the hold would have ended on its own.
type liftedCooldown struct {
	note  string
	until time.Time
}

// hold records that base is off limits until `until` (never shortening a hold
// already in force).
func (c *cooldowns) hold(base string, until time.Time) {
	if base == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[string]time.Time{}
		c.at = map[string]time.Time{}
		c.lifted = map[string]bool{}
		c.firm = map[string]bool{}
		c.liftNote = map[string]liftedCooldown{}
	}
	if cur, ok := c.until[base]; !ok || until.After(cur) {
		c.until[base] = until
	}
	c.at[base] = time.Now()
	delete(c.liftNote, base) // refused after a lift: a dispatch after THIS hold ends was not allowed by that lift
	if c.lifted[base] {
		c.firm[base] = true // refused again after its hold was lifted
	}
}

// newestHold is the latest refusal that holds any node at now, or the zero time when none does:
// a health read for the capacity wait is taken after it, so a hold can be judged against it.
func (c *cooldowns) newestHold(now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var newest time.Time
	for base, until := range c.until {
		if now.Before(until) && c.at[base].After(newest) {
			newest = c.at[base]
		}
	}
	return newest
}

// lift ends base's hold when a health read taken AFTER its latest refusal proves a free worker
// (freeWorkerProven): the hold is the node's Retry-After, its own estimate at refusal time of when a
// worker frees, and a later read that shows one free has outdated it. readAfter is the instant the
// read was guaranteed to postdate (fetchViewsDetailedSince's notBefore); a hold set after it was not
// judged by that read. Unknown and saturated readings keep the hold, as does a node that already
// refused after a lift (firm).
func (c *cooldowns) lift(base string, v NodeView, readAfter, now time.Time) (note string, lifted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, held := c.until[base]
	if !held || !now.Before(until) || c.firm[base] || readAfter.IsZero() || c.at[base].After(readAfter) || !freeWorkerProven(v) {
		return "", false
	}
	delete(c.until, base)
	c.lifted[base] = true
	note = fmt.Sprintf("its Retry-After cooldown (%s left) was lifted: its health, read after the refusal, shows %d of %d worker(s) running and none queued",
		until.Sub(now).Round(time.Second), v.JobsRunning, v.MaxConcurrentJobs)
	if c.liftNote == nil {
		c.liftNote = map[string]liftedCooldown{}
	}
	c.liftNote[base] = liftedCooldown{note: note, until: until}
	return note, true
}

// liftNarration is the lift note a dispatch to base may carry, or "". A lift explains a dispatch only while
// it is what let the node be asked: the hold it ended would still be in force at now, and the node has not
// refused since (hold forgets the note). A node lifted on one tick and dispatched on a later one, after the
// hint had run out anyway, was asked because the hint ended; saying its cooldown "was lifted" would claim a
// speed-up that never happened.
func (c *cooldowns) liftNarration(base string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.liftNote[base]
	if !ok || !now.Before(l.until) {
		return ""
	}
	return l.note
}

// heldUntil reports the instant base becomes a candidate again, when that is
// still in the future.
func (c *cooldowns) heldUntil(base string, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[base]
	if !ok || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// noteCooldown turns a capacity refusal into a cooldown on the node that sent it.
//
// The instant is JITTERED once, here: K dispatchers that were refused by the same
// node must not all ask it again on the same beat (jitterFrac). The length is the
// node's own Retry-After (in the wall's unit, so a test that compresses the wall
// compresses the hint) or, when the refusal carried none, refusalCooldown - the
// node's health may still advertise the room it just denied, because the snapshot
// is stale by construction (see saturated).
//
// A hint at or above maxQueuedWait - the node clamps its own hint at 300 s and
// says ">=300 s" - is a backlog the delegator would not wait out anyway: the
// comparison is the header's number against the delegator's OWN constant, never
// the node's prose, and the cooldown is capped at that ceiling. The refusal has
// already re-placed the subtask; nothing sleeps.
func (r *runner) noteCooldown(pr PlacedResult) {
	if !pr.refused || !capacityRefusal(pr.refusalStatus) || pr.ranBase == "" {
		return
	}
	d := refusalCooldown
	if pr.retryAfterSec > 0 {
		d = time.Duration(pr.retryAfterSec) * pollSecond
	}
	// The ceiling holds AFTER the jitter: capped first and jittered up by 20 %, a node's
	// 300 s hint held it out for 360 s, while the docs, the ADR and this comment all say
	// 300. Clamping the jittered value would fix that and pile every dispatcher onto the
	// same instant, so the excess is folded back under the ceiling instead: a hint at or
	// above it holds the node out for 240-300 s, spread, never past 300.
	ceiling := time.Duration(int(maxQueuedWait/time.Second)) * pollSecond
	hold := jittered(min(d, ceiling))
	if hold > ceiling {
		hold = 2*ceiling - hold
	}
	r.cool.hold(pr.ranBase, time.Now().Add(hold))
}

// remainingSec is what is LEFT of a subtask's timeout_sec budget. Elapsed
// rounds UP: crediting a 1.2 s attempt as 1 s overstates what is left, and no
// later placement may be promised time the subtask does not have. It can go
// negative, which every caller reads as "nothing left" through a floor check.
func remainingSec(start time.Time, budget int) int {
	return budget - int((time.Since(start).Milliseconds()+999)/1000)
}

// remaining is remainingSec with the subtask's capacity-wait credit added
// back: the ledger's `start` is shifted forward by exactly the idle time the
// wait spent, so a contract that waited 90 s for a node still owns its whole
// execution budget when one finally takes it.
func (pl *placements) remaining(start time.Time, budget int) int {
	return remainingSec(start.Add(pl.credit), budget)
}
