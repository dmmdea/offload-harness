package seatrate

// MinimalFinalTokens is the smallest answer a run can be said to have: the
// viability floor asks only whether the wall holds one tool step and this many
// tokens of final at the seat's rate.
const MinimalFinalTokens = 64

// MinViableSec is the least wall in which a run on this seat can produce ANY
// answer at the seat's measured rate (register D-102; the INV-5 rider's
// clause (i), ADR 0050: a wall-time term may refuse "only below a minimum
// viable final, naming the arithmetic"). The question is the smallest one that
// still means something: one tool step (the read of the context) plus a
// MinimalFinalTokens-token final — no think block, no structured re-pack, and
// NO cold load, because admission pays the cold load OUTSIDE the wall (D-64),
// so a wall shorter than the load is not infeasible.
//
// It is never the estimate's MinTurnSec: that is a cold load plus the seat's
// CONFIGURED final (up to 8,192 tokens), a worst case the rider forbids
// refusing on — a measured-working tier (a 900 s wall on a 27B at 7 tok/s that
// completes its contracts) sits under it. Everything above this floor is
// PUBLISHED (wall_estimate_sec / min_turn_sec / wall_note), never refused.
//
// 0 when the input carries no decode rate: no rate is no opinion, and a caller
// treats 0 as "do not refuse", exactly as every other sizing decision does.
//
// internal/delegate's feasibleFinal (eta.go) applies this same floor to a
// contract placed on a remote node; its parity test
// (TestMinViableSecMatchesTheDelegatorsFeasibilityFloor) fails when the two
// drift, so the two doors can never disagree about the same seat.
func MinViableSec(in Input) int {
	if in.TokS <= 0 {
		return 0
	}
	min := in
	min.ColdLoadSec = 0
	min.MaxSteps = 2 // one tool step, then the final
	min.FinalBudget = MinimalFinalTokens
	min.RepackBudget = 0
	min.ThinkingAuto, min.ThinkingOn = false, false
	return Compute(min).TotalSec
}
