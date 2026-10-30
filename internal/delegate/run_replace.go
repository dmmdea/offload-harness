// run_replace.go - Replacement-node search after a refusal and the exhausted-placement result.

package delegate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// isReplaceable: this attempt ended because a node DECLINED the job at
// dispatch, and the answer it declined with is one another node may answer
// differently - or because the LOCAL seat's run-cap line held the contract for
// its wall and deferred it as capacity with nothing run (capacityDeferRefusal).
//
// The second half is the one comment after comment called re-placeable while
// nothing consumed it (a local capacity defer was terminal). It applies only when
// there is somewhere else to go: route=local waits in place - the seat's own line
// IS the wait, and its defer is the answer - and a delegator with no remotes has
// no other node to offer the work to.
func (r *runner) isReplaceable(pr PlacedResult) bool {
	if pr.refused && replaceableRefusal(pr.refusalStatus) {
		return true
	}
	return capacityDeferRefusal(pr) && r.route != "local" && len(r.remotes) > 0
}

// capacityDeferRefusal reports whether an attempt is the LOCAL leg's own capacity
// defer: it ran on this box's seat, the seat's admission (the run-cap line, the
// cordon, a lease) refused it as capacity, and no step ever ran. Nothing owned
// the contract, so another node may take it without arranging a double run.
//
// LOCAL only, on purpose. A capacity defer a REMOTE node files after acking the
// job is an observed terminal answer of a job that node held; "never re-place
// after a 202" (placeAndRun) keeps it exactly as it came. The one thing re-placed
// after a 202 is a job the node itself says it never ran (a confirmed withdrawal, or
// its own reaped / withdrawn / not-started record: ADR 0064), and that arrives as a
// refused 503, not as a defer.
func capacityDeferRefusal(pr PlacedResult) bool {
	return pr.ranLocal && pr.Err == "" && pr.Result.Deferred &&
		pr.Result.DeferClass == core.DeferClassCapacity && pr.Result.Steps == 0
}

// replaceableRefusal decides, from the status a node answered a DISPATCH with,
// whether offering the job to a DIFFERENT node is worth the wall clock.
//
// The line is WHO THE ANSWER IS ABOUT.
//
// About THIS NODE, RIGHT NOW → re-place. Another node answers these
// differently, and the whole point of a fleet is that it can:
//
//	0   the delegator never reached it at all (dial refused, dropped
//	    connection, per-request deadline) — a statement about one address.
//	404 nothing at this address serves /fleet/dispatch.
//	408 it ran out of time reading THIS request.
//	409 "job previously failed on this node" — the node's own message says
//	    on this node; it is a fact about that node's job store, and no other
//	    node holds that record.
//	429 too many requests, to it.
//	5xx it, or something in front of it, is failing: `503 queue full`,
//	    `503 node draining`, `503 vram snapshot stale`, a 500 from a proxy.
//	    An unknown 5xx is still an "it is broken" answer, so the default for
//	    that range is to re-place.
//
// About THE REQUEST → terminal. Every other 4xx: 400 (a malformed envelope, a
// body over the node's 1 MiB cap, an unsupported task_type, a contract the
// node's own Validate rejects), 401 (the fleet_auth_token — the delegator
// sends the SAME token to every node, so a fleet-wide credential mismatch is
// an operator fix, not a routing one), 403 (the agent lane requires a token
// this delegator does not have), 405, 413, 415, 422, and any future 4xx. The
// next node is handed byte-identical bytes and the same bearer, so it returns
// the same answer; re-placing only collects it N times and spends the
// contract's budget doing it. An unknown 4xx defaults to terminal for exactly
// that reason — the 4xx range means "your request", by definition.
//
// The rule keys on the STATUS, never on the text of an error string: a node's
// prose is not a protocol, and matching on it is how a reworded message
// silently changes routing.
//
// KNOWN BOUND, stated rather than hidden: a fleet whose nodes carry DIFFERENT
// bearer tokens gets no re-placement out of a 401/403. That is deliberate —
// docs/FLEET-NODE.md specifies one shared token for the whole fleet, and
// spraying a rejected credential across a roster is not something to build in
// on the chance the deployment disobeys it.
func replaceableRefusal(status int) bool {
	if status >= 400 && status < 500 {
		switch status {
		case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return false
	}
	// status 0 (never reached), any 5xx, and the pathological non-202 2xx/3xx
	// (something at that address is not a fleet node) are all about the node.
	return true
}

// replacementNode picks where a refused subtask goes next: the best eligible
// remote not yet tried, and — reserved as the last resort — the local seat.
//
// The bound and the exclusion set both come from the SUBTASK's ledger, not
// from this call: pl.used is every re-placement the subtask has spent (across
// the first attempt AND the verification retry) and pl.tried is every dial base
// it has already been placed on. Local is deliberately not charged against the
// bound — it is the one seat always able to take the contract, so a wide roster
// must not be able to spend the bound before reaching it — but it IS in
// pl.tried, which is what keeps it to one use per subtask.
//
// refused is this chain's refusal count, used only for the human-readable
// reason; the bound is pl.used and nothing else.
//
// THE FLEET IS READ AGAIN, for every route (ADR 0063). route=spread used to reuse
// the snapshot taken when the run started, so a node the run's own siblings had
// filled since was still "the node with room". The read goes through the probe
// memo, but only a snapshot taken AFTER this refusal counts (fetchViewsSince): one
// that predates it is exactly the picture the refusal just proved wrong. In practice
// each re-placement reads the fleet itself - a sibling's probe is rarely newer than
// this caller's own refusal - which is the one extra health read per re-placement
// ADR 0063 names in its Consequences.
//
// A candidate must pass the room check (hasRoomWithin: not saturated, and able
// to START the job inside the caller's patience), must not be in cooldown after
// a refusal of its own, and must not be a node whose process gate is already
// full (processgate.go). A node that fails those is not "refused for good": with
// no candidate left the subtask goes to the capacity wait, which re-reads every
// one of them each tick.
//
// The local seat is the reserved last resort only when its run-cap line has a
// free slot ahead of a newcomer (localSlotAhead). A full seat used to take the
// subtask at once and hold it in a line it could not leave - 26 of the day's 52
// local re-placements died there.
//
// ok=false returns the sentence the exhausted message ends with. It is built
// from BOTH facts (the bound, and whether local was available) because a reader
// who is told only one of them will look in the wrong place.
func (r *runner) replacementNode(ctx context.Context, contract core.AgentContract, pl *placements, refused int) (placement, string, bool) {
	refusedAt := time.Now()
	boundHit := pl.used >= maxRemoteReplacements
	// busy names every eligible node the filters below held out only because it is
	// busy right now (withRoom); nothing was dispatched to any of them.
	var busy []string
	if r.route != "local" && !boundHit {
		st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
		// Re-probe: the roster this subtask was placed against is now known to
		// be at least partly wrong (a node just refused), and health is the
		// only thing that can say which of the others has room.
		views, bases, _ := r.fetchViewsSince(ctx, refusedAt)
		freshViews, freshBases := untried(views, bases, pl.tried)
		var candViews []NodeView
		var candBases []string
		var lifted map[string]string
		candViews, candBases, busy, lifted = r.withRoomAfter(st, freshViews, freshBases, refusedAt)
		if chosen := Place(mintP2CSeed(), st, r.localView(), candViews, true); !chosen.Local {
			base := baseFor(chosen, candViews, candBases)
			return placement{
				view:   chosen,
				base:   base,
				reason: fmt.Sprintf("re-placed on %s after %d refusal(s)", chosen.NodeID, refused) + liftedClause(lifted[base]),
			}, "", true
		}
	}
	// Local is the reserved last resort. route=remote never takes it: an
	// explicit remote route must not silently fall local, which is the same
	// posture the "no eligible remote" defer already holds.
	head := "no further eligible remote was available"
	if r.call.reached() {
		// The read above may be the very thing the call's deadline ended, and what a cut
		// read held is not evidence about the nodes it never reached (ADR 0065).
		head = "the call's deadline had passed, so the fleet read may not have named every eligible remote"
	}
	if len(busy) > 0 {
		// A node the filters held out is BUSY, not broken: INV-4 sends the subtask to
		// the capacity wait, which re-reads every one of them each tick - whatever
		// kind of refusal started this chain. Only a 503 or 429 used to say so, and a
		// 500 followed by a full node failed a subtask that a node about to free could
		// have taken.
		pl.capacityRefusal = true
		head = "no further remote could take it right now (" + strings.Join(busy, "; ") + ")"
	}
	if boundHit {
		head = fmt.Sprintf("the re-placement bound of %d further node(s) was reached", maxRemoteReplacements)
	}
	if r.route == "remote" {
		return placement{}, head + ", and route=remote never falls back to local", false
	}
	if pl.tried[""] {
		return placement{}, head + ", and the local seat had already been tried", false
	}
	// A contract that names a layer this box does not declare (register A-108) has no local seat to
	// fall back on: it could only defer there by name. Returning here sends a refusal that was a place
	// in line (pl.capacityRefusal) to the capacity wait, for the node that declares the layer.
	if !r.localServesLayer(r.localView(), contract) {
		return placement{}, head + ", and " + localRefusal(r.localView(), contract) + ", so the local seat cannot take it", false
	}
	// A text lease reserves the local seat for re-placement exactly as it does
	// for first placement (0.113.18; before this a remote's 503 fell straight
	// onto the reserved cards — the 2026-09-05 incident through a side door).
	// The capacity wait, not this fall-back, is what watches for the release.
	if info := r.localLease(contract); Reserved(info) {
		return placement{}, head + ", and the local seat is reserved (" + HolderLine(info) + ")", false
	}
	// A seat a lease FENCES is no last resort either (GPU routing P7): the run would be turned
	// away at the seat's own pre-check. The fence is a place in line, and the wait watches it.
	if info, why, fenced := r.fencedLocal(contract); fenced {
		pl.capacityRefusal = true
		return placement{}, head + ", and the local seat is fenced (" + why + " — " + HolderLine(info) + ")", false
	}
	// An occupied seat is no last resort either (0.165.2): loading it would unload another
	// loaded vLLM seat (probeLocalBusy's occupiedBy; the reference box's opencode seat). A remote
	// was eligible and refused, so this is a place in line: the capacity wait re-reads every node
	// each tick and takes the local seat only once the occupant has left.
	if r.route != "local" {
		if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" {
			pl.capacityRefusal = true
			return placement{}, head + ", and " + rd.why(), false
		}
	}
	if free, note := r.localSlotAhead(); !free {
		// A full run-cap line is a place in line, not a refusal: the wait watches it and
		// takes the seat when a slot frees (INV-4), whatever kind of refusal started
		// this chain.
		pl.capacityRefusal = true
		return placement{}, head + ", and the local seat's run-cap line has no free slot (" + note + ")", false
	}
	return placement{
		view:   r.localView(),
		reason: fmt.Sprintf("re-placed on the local seat after %d refusal(s) — queued-local beats work nobody ran", refused),
	}, "", true
}

// withRoom filters a fleet snapshot down to the nodes that can take a NEW
// dispatch of st right now: eligible, room by their own advertisement, a start
// inside the caller's patience, no cooldown from a refusal of their own, and a
// process gate that is not already full. Views and bases stay index-parallel.
//
// busy has one line for every ELIGIBLE node that was held out only because it is
// busy right now - the arithmetic or the cooldown that says so - and nothing for a
// node that cannot run the contract at all (remoteEligible): the first kind frees
// up and is worth waiting for, the second never does.
func (r *runner) withRoom(st Subtask, views []NodeView, bases []string) (outV []NodeView, outB []string, busy []string) {
	outV, outB, busy, _ = r.withRoomAfter(st, views, bases, time.Time{})
	return outV, outB, busy
}

// withRoomAfter is withRoom for views read no earlier than readAfter (the zero time: a read of unknown
// age, which lifts nothing). Given one, a node on a Retry-After cooldown whose health in these views
// proves a free worker has its cooldown lifted (cooldowns.lift) and is judged like any other; lifted
// names, per base, why.
func (r *runner) withRoomAfter(st Subtask, views []NodeView, bases []string, readAfter time.Time) (outV []NodeView, outB []string, busy []string, lifted map[string]string) {
	outV = make([]NodeView, 0, len(views))
	outB = make([]string, 0, len(bases))
	now := time.Now()
	sheddable := r.priority < core.BandNormal
	for j, v := range views {
		if !remoteEligible(st, v) {
			continue
		}
		if until, cooling := r.cool.heldUntil(bases[j], now); cooling {
			note, liftedNow := r.cool.lift(bases[j], v, readAfter, now)
			if !liftedNow {
				busy = append(busy, fmt.Sprintf("%s: cooling down after its own refusal (%s left)", laneID(v), until.Sub(now).Round(time.Second)))
				continue
			}
			if lifted == nil {
				lifted = map[string]string{}
			}
			lifted[bases[j]] = note
		}
		if patience, clamp := r.patience(st.Contract, v); !hasRoomWithin(v, sheddable, patience) {
			busy = append(busy, laneID(v)+": "+whyNoRoom(v, sheddable, patience, clamp))
			continue
		}
		if !processGate.available(bases[j], admissionCeiling(v)) {
			busy = append(busy, fmt.Sprintf("%s: process gate (this process already holds %d open, at its admission ceiling of %d)", laneID(v), processGate.load(bases[j]), admissionCeiling(v)))
			continue
		}
		outV = append(outV, v)
		outB = append(outB, bases[j])
	}
	return outV, outB, busy, lifted
}

// liftedClause is the bracket a placement reason carries when the node it names was asked before
// its Retry-After cooldown ended, because its health proved a free worker.
func liftedClause(note string) string {
	if note == "" {
		return ""
	}
	return " [" + note + "]"
}

// fleetReadForWait is the capacity wait's per-tick read of the fleet. A node on a cooldown is judged
// only by a read taken after its latest refusal (cooldowns.lift), so the read must postdate the newest
// hold in force: the 2 s probe memo may serve a snapshot another waiter's probe took just before a
// refusal, and a picture older than the refusal is the one thing that proves nothing about it. With
// no hold in force it is the ordinary memoised read (the zero time). readAfter is the instant the read
// is guaranteed to postdate.
func (r *runner) fleetReadForWait(ctx context.Context) (views []NodeView, bases []string, probeErrs []string, failed map[string]string, readAfter time.Time) {
	readAfter = r.cool.newestHold(time.Now())
	views, bases, probeErrs, failed = r.fetchViewsDetailedSince(ctx, readAfter)
	return views, bases, probeErrs, failed, readAfter
}

// whyNoRoom says why hasRoomWithin refused v: no room by its own advertisement, or
// room but a backlog past the caller's patience (with the arithmetic). clamp is the
// note runner.patience returns when the call's remaining time, not the contract's own
// poll budget, set that patience; it rides only the backlog verdict, since a node with
// no room is refused whatever the patience.
func whyNoRoom(v NodeView, sheddable bool, patience time.Duration, clamp string) string {
	if !hasRoom(v, sheddable) {
		switch {
		case v.SaturationKnown && v.SaturationHigh:
			return "no room (the node reports itself saturated)"
		case v.MaxQueueDepth > 0 && v.QueueDepth >= v.MaxQueueDepth:
			return fmt.Sprintf("no room (queue_depth %d of max_queue_depth %d)", v.QueueDepth, v.MaxQueueDepth)
		case sheddable:
			return "no room (no idle execution slot, and sheddable work takes idle capacity only)"
		}
		return "no room (not accepting new work)"
	}
	_, why := startsWithinPatience(v, patience)
	return "backlog (" + why + clamp + ")"
}

// untried filters a fleet snapshot down to the nodes this subtask has not
// already been refused by, keeping views and bases index-parallel.
//
// It excludes on the DIAL BASE, not the node id: a node that answered health
// without a node_id would otherwise share one empty key with every other such
// node, and the dial target is the thing actually being re-tried.
func untried(views []NodeView, bases []string, tried map[string]bool) ([]NodeView, []string) {
	outV := make([]NodeView, 0, len(views))
	outB := make([]string, 0, len(bases))
	for j := range views {
		if tried[bases[j]] {
			continue
		}
		outV = append(outV, views[j])
		outB = append(outB, bases[j])
	}
	return outV, outB
}

// refusalLine renders ONE node's refusal for the operator-facing list. The node
// is named from what it advertised; a node that published no node_id gets a
// shape that reads as MISSING rather than a fabricated name.
func refusalLine(pr PlacedResult) string {
	name := pr.Node
	if name == "" {
		name = "(a remote that reported no node_id)"
	}
	if capacityDeferRefusal(pr) {
		// A defer, not an error: the local seat answered, and what it said is
		// its own reason.
		return name + " (local seat): deferred (capacity): " + pr.Result.Reason
	}
	return name + ": " + pr.Err
}

// replacementNote is the annotation carried on every re-placed result — the one
// that finally ran as much as the one nobody took. landed separates the two,
// because one wording cannot describe both without lying about one of them:
// "re-placed after 2 refusals" on a subtask nobody ran claims a placement that
// never happened.
func replacementNote(refusals []string, landed bool) string {
	if landed {
		return fmt.Sprintf("re-placed after %d refusal(s) — %s", len(refusals), strings.Join(refusals, "; "))
	}
	return fmt.Sprintf("%d refusal(s) and no node took it — %s", len(refusals), strings.Join(refusals, "; "))
}

// exhausted turns "nobody took it" into the one honest outcome: a FAILURE
// naming every node that refused and what it said, plus why the delegator
// stopped asking. last is the final refused attempt, whose telemetry row was
// already written by finish(); only what is PUBLISHED is rewritten here (the
// same posture mergeAttempts takes with its retry annotations).
//
// Node and Seat are CLEARED. `last` carries the id of the node that refused
// LAST, and publishing it as the result's node attributes the subtask to a box
// that explicitly declined it — the one attribution nobody in this path earned.
// No node ran this subtask, so it names none; every node that was asked, and
// exactly what each said, is in Err verbatim.
//
// The note is omitted when nothing was actually RE-placed (a single refusal
// with nowhere to go). Replacements is 0 there and correctly so, and shipping a
// `replacement_note` beside `replacements: 0` — with summary.replaced not
// counting it either — read as a contradiction on the wire. Err already carries
// the same refusal list, so nothing is lost by the omission.
func exhausted(last PlacedResult, refusals []string, why string) PlacedResult {
	last.refusalRow = false // what is published is the chain's outcome, not the refused attempt's row
	if capacityDeferRefusal(last) {
		// The last thing refused was the local seat's own capacity DEFER, and
		// nothing else had room either. That is still a defer - the fleet is
		// healthy, it was not this contract's turn - and publishing it as a
		// "placement refused" FAILURE would tell the caller no node was willing
		// when every node was merely busy. Node and Seat stay: the local seat did
		// answer, and its own reason is the outcome.
		last.Replacements = len(refusals) - 1
		last.ReplacementNote = ""
		if last.Replacements > 0 {
			last.ReplacementNote = replacementNote(refusals, false)
		}
		last.Unplaced = true // nothing ran it: never counted as a recovered re-placement
		last.Result.Reason += "; no other node could take it (" + why + ")"
		return last
	}
	last.Node, last.Seat = "", ""
	last.Replacements = len(refusals) - 1
	last.ReplacementNote = ""
	if last.Replacements > 0 {
		last.ReplacementNote = replacementNote(refusals, false)
	}
	last.Err = fmt.Sprintf("%s: %d node(s) refused this subtask and none of them ran it (%s); %s",
		replacementExhaustedPrefix, len(refusals), strings.Join(refusals, "; "), why)
	return last
}

// exhaustedSettled is exhausted() with the call deadline's answer applied (ADR 0065 decision
// 2). Every outcome is cut at the moment it is PRODUCED, and a refusal chain is produced here,
// outside finish and settle. One closed after the call's deadline has passed is not evidence
// about the nodes: the fleet read that would have found one may be the very thing the deadline
// ended, so "no further eligible remote was available" would accuse nodes that were never
// asked, and a failure flags a one-subtask call as an error when it only ran out of time.
//
// Until the deadline has passed it is exactly exhausted(): the last refused attempt's row
// stands as the record. After it the outcome is the call-deadline defer with the chain quoted
// behind the marker, recorded here under a job id of its own for the reason settle gives one:
// the id of the last refused attempt belongs to its row, which says "dispatch ... 404" for a
// job no node held, and a second row under it would double-count one id.
//
// Nothing ran it, so it is cut as an outcome that was never placed. What the refused attempt
// left on the result says where THAT attempt was (the local seat's capacity defer, a job a
// node acked and then took back, a lease's sentinel), not where this subtask is, and must not
// steer the cut's wording ("was still running on the local seat") or its node and seat; the
// attempt's own intent was closed by its finish. since is when the span the closing row
// measures began.
func (r *runner) exhaustedSettled(contract core.AgentContract, last PlacedResult, pl *placements, refusals []string, why string, since time.Time) PlacedResult {
	// The row of the latest attempt that wrote one, which is what settle() reads too: the seed is the last attempt only when
	// it went through finish, and a pseudo-attempt (a replacement the process gate turned away, a deal's overflow) returns
	// before it, with no row and no mark, in front of a refused attempt whose row is the only record there is.
	refusedLast := last.refusalRow || (pl != nil && pl.lastRowRefusal)
	pr := exhausted(last, refusals, why)
	if !r.call.reached() {
		if refusedLast {
			// The row of the last refused attempt is a refusal (delegationLogLine.Refusal), so the failure the
			// caller is handed has no row yet: it gets one under a job id of its own, like a cut chain's.
			pr.JobID = mintJobID()
			pr.wallMs = time.Since(since).Milliseconds()
			r.record(contract, pr)
		}
		return pr
	}
	pr.ranLocal, pr.intentRecorded, pr.orphanable, pr.waitCapacity = false, false, false, false
	pr.withdrawn, pr.nodeNeverRan, pr.nodeTerminal = false, "", false
	pr.refusalRow = false
	pr = r.cutOutcome(pr, true)
	pr.JobID = mintJobID()
	pr.wallMs = time.Since(since).Milliseconds()
	r.record(contract, pr)
	return pr
}
