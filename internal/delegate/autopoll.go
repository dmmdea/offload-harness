package delegate

import (
	"fmt"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// THE TWO CLOCKS (register D-116).
//
// Since D-03 a contract whose caller named no timeout_sec rides the wire as
// `timeout_auto`, and the EXECUTING node sizes the wall from its own seat's
// measured rate — anywhere inside [AgentTimeoutSecDefault, AgentTimeoutSecCap]
// — because only that node knows what its cards do. The delegator held its own
// clock at the CAP for every such contract (executionBudgetSec), so a node that
// acked a job and then died silently was polled for 900 s + grace although its
// own wall had been 300 s. The 0.126.0 review wrote that down as an accepted
// cost and named the fix: size the bound from what the node ADVERTISES.
//
// This file is that fix. It is the delegator's half of one arithmetic — the
// node's half is pipeline.AutoWallFor — and both run seatrate.AutoWallFor over
// a seatrate.SeatPolicy, so the two cannot drift: only where the numbers come
// from differs (the node's machine-local seat-rates store there, the node's
// published `seat_rate`/`seat_budget` here).
//
// The bound is an ESTIMATE of the node's wall, never a verdict over it. The
// node's own wall is authoritative, and the poll loop RAISES this bound the
// moment a running job publishes one (jobWire `wall_sec`): abandoning a job a
// node is still running inside its own wall would be the delegator overruling
// the only party that measured anything.
//
// TWO CLOCKS MEANS TWO STARTS, TOO (review finding 1). The node's wall starts
// AFTER its admission budget — the cordon, the llama-swap pre-flight, the
// cold-load warm-up (125–250 s for a vLLM seat) and the coherence probe, up to
// core.AgentAdmissionSecDefault — and all of that is spent in job state
// `running`, which earns no queued credit. So the delegator's clock, which
// starts at DISPATCH, covers admission + wall in two ways: the pre-adoption
// bound carries an admission allowance (admissionSlack), and the first poll
// that publishes a started wall RE-ANCHORS the clock to that moment and drops
// the allowance (anchoredPollBound). Without them an auto contract landing on
// a cold seat was abandoned at its poll deadline while the node was still
// inside its own wall — exactly the regression this register was written not
// to buy.
//
// And ONE arithmetic is only one clock while both halves are fed the SAME seat
// (review finding 2): the delegator sizes from the agent seat the node
// advertises on health, while a COMPOSITE node runs the contract on the
// dispatched layer's seat and sizes its wall from THAT seat's rate, which
// health publishes no rate for. autoPollBound takes the seat the run will
// actually use and falls back to the cap when it is not the advertised one.

// autoPollBound sizes the delegator's poll bound for contract c placed on
// view, and returns the one-line note naming where the number came from. It
// answers (0, "") for a contract that named its own timeout_sec — those are
// polled at timeout_sec + grace exactly as before.
//
// runSeat is the seat the dispatched contract will ACTUALLY run on when the
// delegator decided one (a composite node's layer seat), and "" when it did
// not name a seat. It is not decoration: the node re-decides the layer and
// sizes its wall from that seat's rate, and health advertises a rate for the
// agent seat alone.
//
// With no usable rate on the view (no seat_rate, no samples, no tok/s) it
// returns the CAP and says so: today's behaviour, unchanged, because a node
// that advertises nothing has told the delegator nothing to bound by.
func autoPollBound(view NodeView, c core.AgentContract, runSeat string) (time.Duration, string) {
	if !c.TimeoutAuto {
		return 0, ""
	}
	who := view.NodeID
	if who == "" {
		who = "the node"
	}
	capBound := time.Duration(core.AgentTimeoutSecCap) * pollSecond
	// THE SEAT MUST MATCH (review finding 2). On a composite node the
	// delegator dispatches a LAYER, the node re-decides it against its own
	// live guards and switches to that layer's seat, and sizes its wall from
	// that seat's measured rate — which health does not publish (`layers`
	// carries seats, never rates). Sizing the delegator's clock from the
	// advertised AGENT seat there is the same arithmetic over different
	// numbers, and a long layer seat is slower by design: the node's wall then
	// outlives the delegator's bound by up to the whole default..cap span.
	// Nothing rescues it either — the `wall_sec` adoption below needs a node
	// new enough to publish one and polls that actually answer 200. So: the
	// cap, and a note that says which seat has no advertised rate.
	if runSeat != "" && view.AgentSeat != "" && runSeat != view.AgentSeat {
		return capBound, fmt.Sprintf("cap: the contract runs on %s's seat %s and only %s's rate is advertised (%d s)",
			who, runSeat, view.AgentSeat, core.AgentTimeoutSecCap)
	}
	sr := view.SeatRate
	switch {
	case sr == nil:
		return capBound, fmt.Sprintf("cap: no seat rate advertised by %s (%d s)", who, core.AgentTimeoutSecCap)
	case sr.TokS <= 0, sr.Samples <= 0:
		return capBound, fmt.Sprintf("cap: no seat rate advertised by %s — seat_rate carries %.1f tok/s over %d samples (%d s)", who, sr.TokS, sr.Samples, core.AgentTimeoutSecCap)
	}
	p := seatrate.SeatPolicy{
		Seat:        view.AgentSeat,
		TokS:        sr.TokS,
		RateSamples: sr.Samples,
		RateSource:  "health seat_rate of " + who,
		ColdLoadSec: sr.ColdLoadSec,
	}
	budgetNote := ""
	if b := view.SeatBudget; b != nil {
		p.StepTokens, p.Thinking = b.StepTokens, b.Thinking
	} else {
		// A node publishes seat_rate and seat_budget together (one gate on the
		// health handler), so this is the future node that publishes the rate
		// alone: size at the house defaults and SAY so, rather than pretend
		// the budgets were read.
		budgetNote = " (no seat_budget published: house defaults)"
	}
	wall, est := seatrate.AutoWallFor(p, c)
	if wall <= 0 {
		return capBound, fmt.Sprintf("cap: no seat rate advertised by %s (%s)", who, est.Note)
	}
	return time.Duration(wall) * pollSecond,
		fmt.Sprintf("sized from %s's seat_rate %.1f tok/s (%d samples): %d s%s", who, sr.TokS, sr.Samples, wall, budgetNote)
}

// admissionSlack is the window the delegator allows for the node's ADMISSION
// before its wall starts (review finding 1): the cordon wait, the llama-swap
// pre-flight, the seat's cold load and the coherence probe, bounded node-side
// by pipeline.admissionBudget (core.AgentAdmissionSecDefault by default).
//
// It rides ON TOP of the sized bound exactly as pollGrace does, and for the
// same reason: neither is part of the node's wall arithmetic, so neither may
// contaminate it (the anti-drift test compares the bound to the node's wall).
// A node that has published a started wall no longer needs it — the clock is
// anchored where the wall began — so the poll loop drops it there.
//
// It reads pollSecond rather than being a constant duration so a test that
// compresses the wall unit compresses this with it.
func admissionSlack() time.Duration {
	return time.Duration(core.AgentAdmissionSecDefault) * pollSecond
}

// anchoredPollBound is the bound once a running job has published a STARTED
// wall (register D-116, review finding 1). The node's wall is authoritative
// AND so is its start: `wall_sec` is stamped at the line that opens the wall
// context, after admission, so the moment it is first observed is the moment
// the delegator can stop guessing. The budget is then the node's own wall plus
// transport slack, measured from here — no admission allowance, because
// admission is over.
func anchoredPollBound(nodeWallSec int) (time.Duration, string) {
	if nodeWallSec > core.AgentTimeoutSecCap {
		nodeWallSec = core.AgentTimeoutSecCap
	}
	return time.Duration(nodeWallSec)*pollSecond + pollGrace,
		fmt.Sprintf("the node's own wall %d s, from where the node started it", nodeWallSec)
}

// raisedPollBound is the bound a RUNNING job's published wall implies
// (register D-116): the node's own wall plus the same grace, clamped to the
// wire cap so a node publishing nonsense cannot hold a delegation open past
// what the wire allows. ok is false when there is nothing to raise to — no
// wall published, or one that does not exceed the bound already in force.
func raisedPollBound(current time.Duration, nodeWallSec int) (time.Duration, string, bool) {
	if nodeWallSec <= 0 {
		return current, "", false
	}
	if nodeWallSec > core.AgentTimeoutSecCap {
		nodeWallSec = core.AgentTimeoutSecCap
	}
	raised := time.Duration(nodeWallSec)*pollSecond + pollGrace
	if raised <= current {
		return current, "", false
	}
	return raised, fmt.Sprintf("the node's own wall %d s", nodeWallSec), true
}
