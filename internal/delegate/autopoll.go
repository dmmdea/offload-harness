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

// autoPollBound sizes the delegator's poll bound for contract c placed on
// view, and returns the one-line note naming where the number came from. It
// answers (0, "") for a contract that named its own timeout_sec — those are
// polled at timeout_sec + grace exactly as before.
//
// With no usable rate on the view (no seat_rate, no samples, no tok/s) it
// returns the CAP and says so: today's behaviour, unchanged, because a node
// that advertises nothing has told the delegator nothing to bound by.
func autoPollBound(view NodeView, c core.AgentContract) (time.Duration, string) {
	if !c.TimeoutAuto {
		return 0, ""
	}
	who := view.NodeID
	if who == "" {
		who = "the node"
	}
	capBound := time.Duration(core.AgentTimeoutSecCap) * pollSecond
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
