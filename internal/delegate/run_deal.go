// run_deal.go - Dealing a batch across the fleet: parallelism, auto-remote and spread placement.

package delegate

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// unlimitedHeadroom stands in for an unpublished max_concurrent_jobs (0 =
// unknown, never a limit) — far past any real dealt count, so it can never be
// mistaken for one.
const unlimitedHeadroom = 1 << 30

// headroom is a node's remaining execution capacity: max_concurrent_jobs −
// jobs_running, floored at 0. An unpublished ceiling (0) reads as UNLIMITED —
// the same convention MaxQueueDepth/MaxConcurrentJobs already follow
// everywhere in this package (0 = unknown, never a limit), so a pre-0.100.0
// node's headroom is never artificially exhausted by this key.
func headroom(v NodeView) int {
	if v.MaxConcurrentJobs <= 0 {
		return unlimitedHeadroom
	}
	if h := v.MaxConcurrentJobs - v.JobsRunning; h > 0 {
		return h
	}
	return 0
}

// dealParallelism is how many subtasks of the call run at once: the width of RunWith's semaphore, sized from the
// deal instead of a constant (ADR 0076, the diagnosis' F01). The old constant, 4, let the joint deal commit two
// subtasks to each of four nodes while only four goroutines ran, so each node saw about one and a 12-page research
// call used four of the fleet's slots.
//
// The width is the sum, over the deal's dealt slots, of what each place takes at once, never less than
// runConcurrency:
//
//   - a remote that PUBLISHES max_concurrent_jobs counts every subtask dealt to it: the deal already held that
//     count to the node's headroom (ADR 0050 decision 4), and the process gate holds the dispatches this process
//     has open to its admission ceiling (ADR 0063 decision 7);
//   - a remote that publishes none counts at most runConcurrency, per node: an unpublished ceiling is unknown and
//     never a limit (headroom), so the deal can send it every subtask, and the bound the constant gave a call is
//     all the evidence there is that it can take them. Two such nodes give eight, not four. The semaphore is one
//     pool, so once the call's other legs finish only the process gate keeps such a node at four: admissionCeiling
//     holds a node that publishes neither ceiling to runConcurrency open dispatches;
//   - the local seat counts min(dealt, run-cap room): past its room a subtask waits in the seat's own FIFO (register
//     C-60), and holding a run slot for that wait is what the constant was bounding. A seat with no run cap
//     (fleet_max_concurrent_jobs < 0) has unlimited room, so it counts at most runConcurrency, the bound the
//     constant gave it;
//   - a slot the deal gave no place (capacityWait: every node with room was already dealt to its headroom; reserved:
//     a lease holds the seat) counts nothing. It reaches the wait only when a slot frees, so the launch loop still
//     starts it behind the dealt ones and ADR 0073 decision 9 holds as amended: a wait holds its slot for the TTL while
//     a subtask of the call has not started and then gives it up (it dispatches only with a slot), and every wait,
//     not only the last to start, runs to the call's horizon.
//
// A call with no deal to read (route=local, which has none) has runConcurrency.
func (r *runner) dealParallelism() int {
	var deal []spreadSlot
	switch r.route {
	case "spread":
		deal = r.spreadDeal
	case "auto", "remote":
		deal = r.autoDeal
	}
	local := 0
	perNode := make(map[string]int, len(deal))
	published := make(map[string]bool, len(deal))
	for _, sl := range deal {
		switch {
		case sl.capacityWait || sl.reserved:
		case sl.view.Local:
			local++
		default:
			perNode[sl.base]++
			published[sl.base] = sl.view.MaxConcurrentJobs > 0
		}
	}
	room := r.dealRoom
	if room >= unlimitedHeadroom {
		room = runConcurrency
	}
	width := min(local, room)
	for base, n := range perNode {
		if !published[base] {
			n = min(n, runConcurrency)
		}
		width += n
	}
	return max(runConcurrency, width)
}

// dealAutoRemote computes route=auto/remote's placement for EVERY subtask of
// the run in ONE ordered pass over ONE fleet snapshot — auto/remote's own
// version of dealSpread's invariant (W-06, register S-11/S-13). Before this,
// attempt() called fetchViews and Place PER SUBTASK, independently, on
// whichever goroutine reached it first; runConcurrency siblings then probed
// the fleet within milliseconds of each other and could all read the SAME
// free slot on the SAME node before any of them had dispatched — gate.go's
// saturated() DEMOTES rather than excludes a full node precisely because this
// snapshot is stale by construction, and re-placement (the refusal loop) is
// the net that used to catch it, one 503 at a time, after the fact.
//
// dealt tracks, per dial base, how many subtasks THIS deal has already
// assigned — placeAutoRemote checks it against headroom(v) before a node is
// even considered, so a run that fans 8 subtasks at a 4-worker node deals it
// AT MOST 4, and the rest fall to the next-best eligible node instead of
// queuing behind siblings that have not even dispatched yet.
//
// An IDLE local seat takes the work it can run only while its run-cap line has room (ADR 0076, the diagnosis'
// F02): the deal counts every subtask it gives the seat against localRunCapRoom, read once, exactly as it counts a
// remote's headroom (ADR 0063 decision 6 did the same for the spread deal). Dealing the seat all of them put
// the overflow into a FIFO the subtasks could not leave while eligible remotes idled. The overflow goes through
// the ranking below, the remoteEligible gate unchanged, to the remotes with headroom, and with none to the capacity
// wait; only while no remote could run the contract at all is the seat's own line the only queue there is.
func (r *runner) dealAutoRemote(contracts []core.AgentContract, localView NodeView, views []NodeView, bases []string, localBusy bool, failed map[string]string) []spreadSlot {
	room, note := 0, ""
	if !localBusy {
		room, note = r.localRunCapRoom()
	}
	return r.dealAutoRemoteRoom(contracts, localView, views, bases, localBusy, failed, room, note)
}

// dealAutoRemoteRoom is dealAutoRemote over a run-cap room the caller already read: RunWith reads it before the
// deal, to decide whether the roster is needed at all, and the deal must count against that same reading
// (note is what the reading was, for the reason a spent line gives).
func (r *runner) dealAutoRemoteRoom(contracts []core.AgentContract, localView NodeView, views []NodeView, bases []string, localBusy bool, failed map[string]string, room int, note string) []spreadSlot {
	r.dealRoom = room
	dealt := make(map[string]int, len(views)+1)
	out := make([]spreadSlot, len(contracts))
	for i, c := range contracts {
		st := Subtask{Contract: c, EstTokens: EstimateTokens(c)}
		// Review round 1, MEDIUM item 3: the job id is minted HERE, once, and
		// used as W-11's P2C draw seed — not a throwaway random string
		// unrelated to what the result eventually carries. attempt() reuses
		// this exact id (spreadSlot.jobID) instead of minting a second one,
		// so the seed the ranking drew on and the id the published
		// placement/ledger/corpus row names are the SAME value.
		jobID := mintJobID()
		// The seat is dealt as a busy one for this subtask once the deal has spent its line AND some remote could
		// run the contract: placeAutoRemote then ranks the remotes and never takes the idle shortcut.
		busy, spent := localBusy, false
		if !localBusy && dealt[""] >= room && r.localServesLayer(localView, c) && anyRemoteEligible(st, views) {
			busy, spent = true, true
		}
		slot := r.placeAutoRemote(jobID, st, localView, views, bases, busy, dealt, failed)
		if spent {
			slot.reason += fmt.Sprintf("; the idle local seat's run-cap line is spent by this deal (%d of %d free slot(s) dealt; %s)", dealt[""], room, note)
		}
		// A remote hint (ADR 0078) is placed on the remotes first and may fall back to the local seat where route=remote
		// would have deferred. Where no remote has room the subtask goes to the seat HERE, in the deal, when the seat
		// read free and its run-cap line has room, and not to the capacity wait: the wait would start it only after a
		// dealt subtask finished (the overflow counts nothing in the width), and with the wait switched off it would
		// defer while the seat idled. A seat that read busy, leased or occupied, or whose line this deal has spent,
		// leaves the subtask to the wait, as for an auto call.
		if slot.capacityWait && r.remoteHint() && r.hintSeatFree && dealt[""] < room && r.localServesLayer(localView, c) {
			dealt[""]++
			slot = spreadSlot{placement: placement{view: localView, reason: slot.reason + fmt.Sprintf("; the idle local seat takes it instead, a remote hint falling back where no remote has room (%d of %d free slot(s) dealt; %s)", dealt[""], room, note)}}
		}
		slot.jobID = jobID
		out[i] = slot
	}
	return out
}

// anyRemoteEligible reports whether some remote in views could run st at all: capability (remoteEligible), not
// capacity. It is the question that decides whether a seat whose line is spent has anywhere else to send the work.
func anyRemoteEligible(st Subtask, views []NodeView) bool {
	for _, v := range views {
		if remoteEligible(st, v) {
			return true
		}
	}
	return false
}

// placeAutoRemote deals ONE subtask within dealAutoRemote's joint pass: an
// idle local seat wins the work it can run (Place's own rule); busy, the
// best REMOTE that passes remoteEligible AND still has headroom over what
// this deal has already committed to it. A node at 0 headroom gets NOTHING —
// no floor, no "at least one" — and the next-best candidate is tried.
//
// A contract that names a layer this box does not declare is not work the
// idle seat can run (register A-108): it is dealt exactly as a busy seat's
// subtask is, to the best remote that declares the layer, and the idle-local
// shortcut is for the rest.
//
//   - Some node had headroom: dealt, resolved, headroom decremented for the
//     next subtask in this same deal.
//   - No node had headroom, but at least one was otherwise eligible: the
//     capacityWait sentinel — attempt() hands it to the existing capacity
//     wait (awaitCapacity), which watches for room to free the same way it
//     already watches a 503 refusal or a held lease.
//   - Nothing was eligible at all (capability, not capacity): the noRemote
//     sentinel — attempt() re-derives the exact pre-W-06 sentence
//     (r.noEligibleRemote) over this same snapshot; unrelated to headroom.
func (r *runner) placeAutoRemote(seed string, st Subtask, localView NodeView, views []NodeView, bases []string, localBusy bool, dealt map[string]int, failed map[string]string) spreadSlot {
	if !localBusy && r.localServesLayer(localView, st.Contract) {
		dealt[""]++ // the local seat's run-cap line is counted like a remote's headroom (ADR 0076)
		return spreadSlot{placement: placement{view: localView, reason: "local idle"}}
	}
	var best, bestRanked NodeView
	var bestBase string
	found, anyEligible := false, false
	prior := fleetTokSPrior(views)
	for j, v := range views {
		if !remoteEligible(st, v) {
			continue
		}
		anyEligible = true
		// The backlog gate (ADR 0063): a node that cannot START this job inside
		// the caller's patience is held out of the deal - a placement feasibility
		// refusal that prints its arithmetic (the verdict line below), never a
		// preference for a faster seat. The capacity wait re-reads it every tick.
		patience, _ := r.patience(st.Contract, v)
		if ok, _ := startsWithinPatience(v, patience); !ok {
			continue
		}
		if headroom(v) <= dealt[bases[j]] {
			continue // W-06: no floor — a node at 0 headroom gets nothing this deal
		}
		// GPU routing P1: a node's cards are spent as the deal gives them out. The
		// ranking copy carries how many subtasks this deal already put on the
		// node's cards, so a node with two free cards is preferred for its first
		// two subtasks and not its third. The stored view is never the copy:
		// baseFor matches the chosen view against the roster by value.
		ranked := v.withCardsDealt(dealt[bases[j]])
		if !found || betterRemote(seed, &st, prior, ranked, bestRanked) {
			best, bestRanked, bestBase, found = v, ranked, bases[j], true
		}
	}
	// D-105 (review round 1, BLOCKER item 2): the verdict line is built ONCE,
	// from `dealt` as it stood WHILE scanning (every candidate's headroom read
	// against the state at decision time, matching what the loop above
	// actually saw), and printed on EVERY exit — chosen, capacity-waiting, or
	// nothing eligible at all — not only the happy path. `failed` carries the
	// dead/unreachable bases from this SAME snapshot, so a node this deal
	// never even heard from is named too, not silently dropped.
	verdicts := placementVerdictLine(st, views, bases, bestBase, dealt, failed, r.patience)
	if found {
		dealt[bestBase]++
		reason := fmt.Sprintf("route=%s → %s (headroom)", r.route, best.NodeID)
		if room := cardRoomFor(best, st.Contract.Layer); room.known && cardTier(bestRanked, st.Contract.Layer) == tierSome {
			// The basis, in the node's own numbers: what it published BEFORE this
			// subtask was dealt to it.
			reason += fmt.Sprintf(" [free cards %d of %d]", room.free, room.total)
		}
		if verdicts != "" {
			reason += "; " + verdicts
		}
		if r.autoDealBusy.occupiedBy != "" {
			reason += "; " + r.autoDealBusy.why()
		}
		return spreadSlot{placement: placement{view: best, base: bestBase, reason: reason}}
	}
	if anyEligible {
		reason := fmt.Sprintf("route=%s: every eligible remote is at headroom or cannot start the job inside the caller's patience", r.route)
		if r.autoDealBusy.occupiedBy != "" {
			reason += "; " + r.autoDealBusy.why()
		}
		if verdicts != "" {
			reason += "; " + verdicts
		}
		return spreadSlot{
			placement:    placement{view: localView, reason: reason},
			capacityWait: true,
		}
	}
	return spreadSlot{placement: placement{view: localView, reason: verdicts}, noRemote: true}
}

// dealSpread computes the spread placement for EVERY subtask of the run in ONE
// ordered pass, before any dispatch goroutine starts. It is a deal, not N
// independent picks, and that is forced rather than stylistic:
//
// The invariant spread exists to hold is that within each deal CYCLE — each
// aligned window of len(nodes) slots — every eligible seat receives at most one
// subtask, so `runConcurrency` sibling subtasks never queue behind each other on
// one seat while another seat idles. A fit score that re-picks freely breaks it
// immediately: the best-scoring seat wins EVERY mechanical slot and the roomiest
// wins every reasoning slot, which is the stacking spread was built to remove.
//
// That invariant cannot be recovered by any per-subtask pure function of
// (index, own shape, roster). Proof, because it decided the shape of this code:
// let f(slot, shape) be such a function. Distinctness within a cycle forces
// f(·, shape) to be a bijection over the seats for each shape. Distinctness
// across a MIXED-shape cycle additionally forces f(p, mechanical) != f(q,
// reasoning) for all p != q — and since both are bijections, that holds only if
// f(p, mechanical) == f(p, reasoning) for every p, i.e. only if the shape does
// not influence the placement at all. Fit scoring and the invariant coexist only
// when the deal can see its siblings, so the deal is computed jointly, once,
// with every subtask's shape in hand.
//
// Computing it up front also keeps placement DETERMINISTIC and free of shared
// mutable state: the cycle bookkeeping lives in this single-threaded pass, the
// result is read-only by the time the goroutines start (same posture as the
// spreadViews snapshot), and the same contracts always produce the same deal.
func (r *runner) dealSpread(contracts []core.AgentContract, localView NodeView) []spreadSlot {
	room, note := r.localRunCapRoom()
	r.dealRoom = room
	book := &dealBook{
		// dealt holds the DIAL BASES already given a subtask in the CURRENT cycle.
		//
		// The base, not the node id, and that is the fix for a real collapse: every
		// other exclusion in this file (pl.tried, pl.excluded, the refusal cooldown,
		// the quarantine) keys on the base, and a node id is neither unique nor
		// guaranteed to be published. Two remotes that both advertise "" — or the
		// same id, which C-19 shows does drift here — shared one entry, so the
		// second remote of a cycle found its key already taken, the cycle was
		// reshuffled, and the fit score handed the SAME seat both subtasks while
		// the other one idled. The base is what the dispatcher actually dials, so
		// it is the only key that can mean "this seat already has one".
		dealt: make(map[string]bool, len(r.spreadViews)),
		// counts is the WHOLE RUN's tally per dial base (dealt is one cycle's): how many
		// subtasks this deal has already given each node, held against the node's
		// headroom so no node is dealt more than it can start (placeSpreadWith). The
		// local seat is the base "".
		counts:    make(map[string]int, len(r.spreadViews)+1),
		localRoom: room,
		localNote: note,
	}
	out := make([]spreadSlot, len(contracts))
	for i, c := range contracts {
		st := Subtask{Contract: c, EstTokens: EstimateTokens(c)}
		out[i] = r.placeSpread(i, st, localView, book)
	}
	return out
}

// dealBook is the running tally of ONE spread deal, owned by dealSpread's
// single-threaded pass.
type dealBook struct {
	dealt  map[string]bool // dial bases already given a subtask in the current cycle
	counts map[string]int  // subtasks dealt per dial base over the whole deal; "" = the local seat
	// localRoom is how many MORE runs the local seat's run-cap line takes when the
	// deal starts (localRunCapRoom, read once so every subtask deals against ONE
	// snapshot), localNote what that reading was. The local seat is counted against it
	// exactly as a remote is counted against its headroom.
	localRoom int
	localNote string
}

// localRoomLeft is what the local seat's run cap still has for this deal.
func (b *dealBook) localRoomLeft() int { return b.localRoom - b.counts[""] }

// spreadSlot is one subtask's resolved spread placement plus the deadFleet flag
// route=auto also raises — see PlacedResult.remotesUnreachable.
type spreadSlot struct {
	placement
	deadFleet bool
	// reserved marks a local slot dealt while a TEXT lease reserves the seat
	// and no remote was eligible: attempt() must wait on the lease (or defer)
	// before running it, never run it outright.
	reserved bool
	// capacityWait (W-06, register S-11/S-13): at least one remote passed
	// remoteEligible but every one of them was already dealt to its own
	// headroom by the time this subtask's turn came — attempt() must hand it
	// to the capacity wait (awaitCapacity), which watches for room to free,
	// rather than dispatch to an already-full node or defer a contract the
	// fleet can plainly run once something clears.
	capacityWait bool
	// noRemote marks a subtask for which NO node in the snapshot passed
	// remoteEligible at all — the pre-W-06 "no eligible remote" outcome,
	// unrelated to headroom. attempt() re-derives the exact sentence with
	// r.noEligibleRemote over the SAME snapshot the deal used.
	noRemote bool
	// jobID (review round 1, MEDIUM item 3): the wire job id dealAutoRemote
	// pre-mints for this subtask and uses as W-11's P2C draw seed — attempt()
	// REUSES it rather than minting a second, unrelated one, so the seed
	// recorded in the ranking and the id the published result/ledger/corpus
	// row carries are the SAME value, matching ADR 0050's "seeded from the
	// job id" claim literally rather than only in spirit. "" on route=spread
	// (dealSpread mints no id at deal time) and on the pre-W-06 fallback path
	// (a caller that reached attempt() without RunWith's precompute).
	jobID string
}

// placeSpread deals ONE subtask across the run's fleet snapshot: slot 0 is the
// local seat, then every remote that passes the hard gate FOR THIS SUBTASK in
// roster order; i mod len picks the rotation slot. The eligible set is per
// subtask on purpose — a contract too big for the 8k seat must not be dealt to
// it just because its sibling fit. With nothing eligible the subtask runs local
// and the reason says why; an infrastructure-class reason is flagged deadFleet
// exactly as route=auto flags it.
//
// A remote slot goes to the best-FIT-SCORED seat (fit.go) among the seats not
// yet dealt in this cycle — fit decides WHICH seat inside the cycle, never a
// free re-pick, so the one-subtask-per-seat-per-cycle invariant survives and a
// same-shaped fan-out still reaches every seat. `dealt` is the cycle's
// bookkeeping and is mutated here; dealSpread owns it. Ties fall back to the
// rotation order, so an all-equal roster deals exactly as it did before fit
// scoring existed.
//
// The LOCAL rotation slot is never contested by SHAPE, and that is a deliberate
// bound on this heuristic, not an oversight:
//
//   - With the local seat idle, subtask 0 lands local whatever its shape — the
//     documented guarantee that a spread's first subtask (and therefore a
//     SINGLE-subtask spread, the riskiest case for any shape heuristic) stays
//     on-box. One regex match must not be able to send an entire run off-box.
//   - The same holds for every later local slot (i mod len == 0), because the
//     fit score ranks seats by their ADVERTISED context ceiling and the local
//     seat advertises none in a delegator run. Scoring it would mean inventing
//     a number for it; leaving it in the rotation keeps its share of the fan-out
//     exactly as before, which is also what stops an all-reasoning fan-out from
//     collapsing back onto one seat.
//
// It IS contested by LOAD (0.113.20, operator decision 2026-09-06): when the
// local seat already holds a request at deal time (spreadLocalBusy, read once
// with the fleet snapshot), every local slot goes to the best-fit eligible
// remote with room instead — K delegating sessions used to stack K × 3 of every
// 8 subtasks on one local seat while the remotes idled (first-local subtask
// 155–189 s under K=3 vs 91–105 s for its siblings). An idle seat keeps every
// slot it had; `agent_spread_local_slot: "always"` restores the unconditional
// slot.
//
// And it is contested by the CONTRACT's layer (register A-108): a subtask that
// names a layer this box does not declare is never dealt the local slot - the
// seat could only defer it by name - so it deals among the remotes that declare
// the layer, and the guarantee above (subtask 0 stays local) is for the
// contracts the box can run.
func (r *runner) placeSpread(i int, st Subtask, localView NodeView, book *dealBook) spreadSlot {
	return r.placeSpreadWith(i, st, localView, book, r.skipsBusyLocal())
}

// placeSpreadWith is placeSpread with the busy rule as an explicit argument, so
// the no-remote-with-room fallback can re-deal WITHOUT it and the two paths
// share one body.
func (r *runner) placeSpreadWith(i int, st Subtask, localView NodeView, book *dealBook, skipBusy bool) spreadSlot {
	// A TEXT reservation takes the local seat out of the rotation — a spread
	// used to ignore the lease entirely, which is how three foreign contracts
	// loaded a reserved seat mid-measurement (2026-09-05 08:04–08:09). A media
	// lease was deliberately NOT consulted here (review 2026-09-06): its render
	// was arbitrated at the affinity gate (ADR 0026), which waited for the render
	// and admitted the load. That gate now turns a run away at once (S-26), so a
	// lease that fences every local seat of the contract also leaves the rotation
	// (below, GPU routing P7); a lease on cards the contract does not use does not.
	//
	// A contract that names a layer this box does not declare leaves the rotation the same
	// way (register A-108): the seat would only defer it by name while a node that declares
	// the layer idles. It deals among the remotes alone, and what they cannot take yet waits
	// in line for them below.
	localServes := r.localServesLayer(localView, st.Contract)
	spreadLease := r.spreadLeaseFor(st.Contract)
	// A lease that FENCES every local seat of the contract's chain takes the seat out of the
	// rotation too (GPU routing P7): a local run dealt onto it is turned away at the seat's own
	// pre-check, so the slot is spent on a leg that cannot take the work. A fence of a card the
	// contract does not need leaves the seat in, exactly as before.
	_, _, spreadFenced := r.fencedLocalSpread(st.Contract)
	localIn := !Reserved(spreadLease) && !spreadFenced && localServes
	// A BUSY local seat (0.113.20) leaves the rotation exactly like a leased
	// one, but only while a remote with room exists to take its slots: the
	// remotes are filtered by hasRoom (a sheddable run needs an idle slot), and
	// with none left the slot falls back to the ordinary deal below, reason
	// attached — the busy rule is an optimisation, never a way to lose work.
	skip := skipBusy && localIn
	var remotes []NodeView
	var remoteBases []string
	eligible := 0
	var heldBack, atCap []string
	// SF-40: public nodes this private contract is kept off, named on the slot's reason (the rest of the
	// ineligible nodes are not narrated by the spread deal, but this one is a policy, not a capability).
	var classHeld []string
	for j, v := range r.spreadViews {
		if !remoteEligible(st, v) {
			if v.AgentEnabled && v.PublicOnly {
				if private, _ := privateWork(st.Contract); private {
					_, detail := publicVerdict(st.Contract)
					classHeld = append(classHeld, laneID(v)+": private ("+detail+")")
				}
			}
			continue
		}
		eligible++
		// The backlog gate (ADR 0063): eligible, but it cannot START this job
		// inside the caller's patience, so it is out of THIS deal - named in the
		// reason with its arithmetic, and re-read by the capacity wait.
		patience, clamp := r.patience(st.Contract, v)
		if ok, why := startsWithinPatience(v, patience); !ok {
			heldBack = append(heldBack, laneID(v)+": backlog ("+why+clamp+")")
			continue
		}
		if skip && !hasRoom(v, r.priority < core.BandNormal) {
			continue
		}
		// Capacity (ADR 0063, W-06's rule for the spread deal): a node is never
		// dealt more subtasks in one run than it has free execution slots
		// (headroom, no floor; an unpublished ceiling is unlimited). Capacity
		// decides FEASIBILITY only - it drops a full node from this subtask's
		// rotation and never re-ranks the rest, so the fit order, the cycle and
		// the one-subtask-per-seat-per-cycle invariant are exactly what they
		// were. The overflow is handed to the capacity wait below.
		if headroom(v) <= book.counts[r.spreadBases[j]] {
			atCap = append(atCap, fmt.Sprintf("%s: cap (%d/%d running, headroom %d, dealt %d)", laneID(v), v.JobsRunning, v.MaxConcurrentJobs, headroom(v), book.counts[r.spreadBases[j]]))
			continue
		}
		remotes = append(remotes, v)
		remoteBases = append(remoteBases, r.spreadBases[j])
	}
	// The LOCAL seat is counted the same way (ADR 0063, decision 6): its run-cap line
	// takes fleet_max_concurrent_jobs runs, minus those already registered on it, and a
	// deal that has spent that room takes the seat out of the rotation exactly as a
	// remote at its headroom is taken out - the overflow waits in line for the first
	// node that frees (the seat too) instead of piling into a line it cannot leave.
	// Only while some remote COULD run the contract (eligible > 0): with none, the
	// seat's own line is the only queue there is, as it always was.
	inRotation := localIn && !skip
	localSpent := false
	if inRotation && eligible > 0 && book.localRoomLeft() <= 0 {
		inRotation, localSpent = false, true
	}
	var nodes []NodeView
	var bases []string
	if inRotation {
		nodes, bases = []NodeView{localView}, []string{""}
	}
	nodes, bases = append(nodes, remotes...), append(bases, remoteBases...)
	held := append(append([]string(nil), heldBack...), atCap...)
	if skip && len(nodes) == 0 {
		if len(atCap) > 0 {
			// Remotes with room exist, but this run's own deal has already spent
			// every free slot they have, and the local seat is busy: the overflow
			// waits in line (the capacity wait, INV-4) instead of stacking on the
			// busy seat or on a node that would refuse it.
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote with room is already dealt to its headroom (%s); %s", strings.Join(atCap, "; "), r.spreadLocalBusy.why())}, capacityWait: true}
		}
		// An occupied seat is never the fallback (operator 2026-10-06: "wait in line"): the
		// subtask waits for the occupant to leave or a remote to free, whichever comes first.
		if r.spreadLocalBusy.occupiedBy != "" {
			what := "no remote with room"
			if eligible == 0 {
				what = "no eligible remote"
			}
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: %s; %s — waiting in line", r.spreadLocalBusy.why(), what)}, capacityWait: true}
		}
		// The reason must not send an operator chasing capacity when no remote
		// could take this contract at all.
		sl := r.placeSpreadWith(i, st, localView, book, false)
		if eligible == 0 {
			sl.reason += fmt.Sprintf(" (%s; no eligible remote)", r.spreadLocalBusy.why())
		} else {
			sl.reason += fmt.Sprintf(" (%s; no remote with room)", r.spreadLocalBusy.why())
		}
		return sl
	}
	if localSpent && len(nodes) == 0 {
		// Every remote that could run it is at its headroom (or holds a backlog the
		// caller will not wait out) AND this deal has spent the local seat's run cap:
		// the overflow waits in line rather than stack on either.
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote with room is already dealt to its headroom or holds a backlog past the caller's patience (%s), and the local seat's run cap is spent (%d of %d free slot(s) dealt; %s)", strings.Join(held, "; "), book.counts[""], book.localRoom, book.localNote)}, capacityWait: true}
	}
	if !localServes && len(nodes) == 0 && len(held) > 0 {
		// Every remote that could take this contract is dealt to its headroom or holds a backlog
		// the caller will not wait out, and the local seat declares no such layer: the overflow
		// waits in line for the first of them to free (INV-4), as it does when the seat's own run
		// cap is spent - never for the seat, which cannot take it.
		if seatless(localView) {
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote that can take it is already dealt to its headroom or holds a backlog past the caller's patience (%s); %s", strings.Join(held, "; "), noLocalSeat)}, capacityWait: true}
		}
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote that can take layer %s is already dealt to its headroom or holds a backlog past the caller's patience (%s); this box declares no such layer", st.Contract.Layer, strings.Join(held, "; "))}, capacityWait: true}
	}
	if len(nodes) == 0 || (len(nodes) == 1 && nodes[0].Local) {
		why, class := r.noEligibleRemote(st, r.spreadViews, r.spreadProbeErrs)
		what := "no eligible remote"
		if len(held) > 0 {
			// Not "no eligible remote": every remote that could run it is at its
			// headroom or holds a backlog the caller will not wait out, and saying
			// otherwise would send an operator to add a node.
			what = "no remote with room"
			why = "every eligible remote is at headroom or holds a backlog past the caller's patience — " + strings.Join(held, "; ")
			class = core.DeferClassCapacity
			if len(r.spreadProbeErrs) > 0 {
				// A remote that failed its health probe is a BROKEN node, not a busy
				// one (noEligibleRemote's DEFAULT-TO-LOUD rule): name it, and keep the
				// loud class so the run reads as the broken fleet it is.
				why += fmt.Sprintf("; and %d other remote(s) failed the health probe: %s", len(r.spreadProbeErrs), strings.Join(r.spreadProbeErrs, "; "))
				class = core.DeferClassInfrastructure
			}
		}
		dead := class == core.DeferClassInfrastructure
		if !localServes {
			// No node on the fleet can take it and this box declares no layer by that name: nothing
			// runs here. The slot is dealt local only so it has a view - the seat's own decision
			// defers the contract naming the layer (runner.decide), with the fleet's verdict beside
			// it. It takes no run-cap slot and waits on no lease.
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: %s, and %s so the seat cannot take it (%s)", what, localRefusal(localView, st.Contract), why)}, deadFleet: dead}
		}
		if spreadLease := r.spreadLeaseFor(st.Contract); Reserved(spreadLease) {
			// The one placement the lease exists to forbid. Dealt local so the
			// slot has a view, but flagged: attempt() waits or defers.
			return spreadSlot{placement: placement{view: localView, reason: "route=spread: local seat reserved (" + HolderLine(spreadLease) + "); " + what + " — " + why}, deadFleet: dead, reserved: true}
		}
		if fence, fenceWhy, fenced := r.fencedLocalSpread(st.Contract); fenced {
			// Likewise a fenced seat: flagged so attempt() waits in line for the lease to end.
			return spreadSlot{placement: placement{view: localView, reason: "route=spread: local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); " + what + " — " + why}, deadFleet: dead, reserved: true}
		}
		book.counts[""]++
		return spreadSlot{placement: placement{view: localView, reason: "route=spread: " + what + " — local (" + why + ")"}, deadFleet: dead}
	}
	slot := i % len(nodes)
	if nodes[slot].Local {
		// A local slot opens a new cycle: the deck of remotes is reshuffled, so
		// the next len(nodes)-1 subtasks deal one to each seat again.
		clear(book.dealt)
		book.counts[""]++
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread → local (slot %d of %d)", slot+1, len(nodes))}}
	}
	prior := fleetTokSPrior(r.spreadViews)
	k := fitPickWith(st, nodes, bases, slot, book.dealt, book.counts, prior)
	if k < 0 {
		// Every eligible remote has already taken a subtask this cycle — which a
		// ragged eligible set can reach without passing through a local slot.
		// Reshuffle rather than stack: after the clear a pick always exists,
		// because len(nodes) > 1 guarantees at least one remote.
		clear(book.dealt)
		k = fitPickWith(st, nodes, bases, slot, book.dealt, book.counts, prior)
	}
	book.dealt[bases[k]] = true
	book.counts[bases[k]]++
	kind, rule := shapeOf(st)
	reason := fmt.Sprintf("route=spread → %s (slot %d of %d, fit=%s/%s)", nodes[k].NodeID, slot+1, len(nodes), kind, rule)
	if skip {
		reason += "; " + r.spreadLocalBusy.why()
	}
	if len(held) > 0 {
		// Nodes this subtask could have been dealt to and was not: named on the slot
		// they were passed over for, with their arithmetic, so an operator can see why
		// a node got nothing.
		reason += "; held back: " + strings.Join(held, "; ")
	}
	if len(classHeld) > 0 {
		reason += "; excluded by data class: " + strings.Join(classHeld, "; ")
	}
	return spreadSlot{placement: placement{view: nodes[k], base: bases[k], reason: reason}}
}

// fitPick returns the index of the best seat in nodes that is neither local
// nor already dealt this cycle, or -1 when the cycle has no seat left. The scan
// starts at the rotation slot and wraps, and only a STRICTLY better key
// displaces the incumbent — so equal seats are dealt in rotation order and an
// all-equal roster behaves exactly as blind round-robin did.
// bases is parallel to nodes and is what `dealt` is keyed on — see dealSpread
// for why the node id is not a usable key here.
//
// The key is the free-card tier (GPU routing P1) first, then the fit score:
// among the seats still undealt this cycle, one whose cards can take the
// contract goes before one whose cards are all busy, and fit orders seats
// within a tier. The cycle invariant is untouched, because the tier only
// chooses WHICH undealt seat takes the slot; every seat still takes exactly one
// per cycle. counts is the deal's whole-run tally per dial base (nil = none
// dealt yet), which spends a node's free cards as subtasks land on them. Nodes
// that publish no per-card truth are one tier, so a fleet of them deals exactly
// as it did before.
func fitPick(st Subtask, nodes []NodeView, bases []string, slot int, dealt map[string]bool, counts map[string]int) int {
	return fitPickWith(st, nodes, bases, slot, dealt, counts, fleetTokSPrior(nodes))
}

// fitPickWith is fitPick with the fleet's assumed rate chosen by the caller: the spread deal passes
// fleetTokSPrior over the WHOLE roster it probed, once for every subtask of the deal, so a seat's
// score does not move as nodes at their headroom leave the rotation (Place and the joint auto deal
// take the median over the whole roster too).
//
// Adequacy comes before all of it: a seat that cannot hold the contract (fitKey) ranks below every seat
// that can, whatever its cards, leases or queue read. Such a seat is never excluded, so a cycle whose
// other seats are all dealt still hands it its slot, and the deal does not depend on its caller having
// filtered the roster through remoteEligible first.
//
// Two demotion keys come next, the two betterRemote leads with (gate.go): a node a lease
// demotes (a long text lease, then an overdue one), then a node whose own advertisement says the next
// dispatch is refused (saturated: its admission ceiling is met, or it reports saturation.high). Both
// DEMOTE and never exclude, as there (see saturated): the
// delegator's copy of those numbers is stale by construction, a demoted node still takes its slot of
// the cycle when nothing better is left in it, and re-placement is the net that catches a wrong
// guess. Before this the spread deal read neither, so with an idle local seat a draining or
// queue-capped node kept its cycle slot while a healthy node idled: a refused dispatch, a cooldown on
// that node, one of the subtask's maxRemoteReplacements spent, and the cycle's order broken.
func fitPickWith(st Subtask, nodes []NodeView, bases []string, slot int, dealt map[string]bool, counts map[string]int, prior float64) int {
	k, bestTier := -1, 0
	var best fitKey
	bestLease, bestSat, bestFault := 0, false, false
	now := faultNow()
	for c := 0; c < len(nodes); c++ {
		j := (slot + c) % len(nodes)
		if nodes[j].Local || dealt[bases[j]] {
			continue
		}
		lease, sat := leaseDemotionRank(nodes[j], &st), saturated(nodes[j])
		fault := faultDemoted(nodes[j], &st, now)
		tier := cardTier(nodes[j].withCardsDealt(counts[bases[j]]), st.Contract.Layer)
		s := scoreFitWith(st, nodes[j], prior)
		better := false
		switch {
		case k < 0:
			better = true
		case s.adequate != best.adequate:
			better = s.adequate
		case lease != bestLease:
			better = lease < bestLease
		case sat != bestSat:
			better = !sat
		case fault != bestFault:
			better = !fault
		case tier != bestTier:
			better = tier > bestTier
		default:
			better = s.beats(best)
		}
		if better {
			k, best, bestTier, bestLease, bestSat, bestFault = j, s, tier, lease, sat, fault
		}
	}
	return k
}
