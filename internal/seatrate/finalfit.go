package seatrate

import (
	"fmt"
	"math"
)

// FinalBudgetFloor is the smallest final-answer budget the wall fit may impose
// (register D-95). Below it the fit stops narrowing and SAYS so: a 900-token
// final is not an answer to a grounded extraction, so the honest outcome there
// is the one the harness already had — the wall, not the token budget, ends
// the run, and the note names that.
const FinalBudgetFloor = 1024

const (
	// finalFitSafetyFrac / finalFitSafetyMinSec: the slack FitFinalBudget keeps
	// back from the remaining wall before converting it into tokens. The seat's
	// rate is an EMA over past runs, not a promise about THIS completion, and
	// the turn also pays one transcript prefill (stepOverheadSec) that the rate
	// does not describe. A tenth of the wall, never less than that prefill.
	finalFitSafetyFrac   = 0.10
	finalFitSafetyMinSec = stepOverheadSec
)

// FinalFit is what FitFinalBudget sizes a final answer from.
type FinalFit struct {
	// ConfiguredFinal is the budget the ONE rule asks for (FinalBudgetFor) and
	// the CAP of the result: the fit may only narrow it, never raise it. A
	// contract whose wall is generous runs exactly as it did before the fit.
	ConfiguredFinal int
	// RemainingSec is the wall left when the final answer is about to be asked
	// for: the whole timeout_sec at run start, time.Until(deadline) at the
	// forced final step. 0 or negative = unknown = no opinion.
	RemainingSec float64
	// OtherSec is everything the run still owes that is NOT the final answer
	// and its re-pack — Estimate.OtherSec at run start (cold load + think block
	// + tool steps); 0 at the forced final step, where those are already spent.
	OtherSec float64
	// TokS is the seat's measured decode rate. 0 = no sample yet = no fit.
	TokS float64
	// Schema: the contract carries an output_schema, so the same wall must hold
	// the final answer AND the structured re-pack of it (register D-46). The
	// fit is then split in two — the exact term the 2026-09-14 A/B ran out of.
	Schema bool
}

// FinalFitResult is the sizing FitFinalBudget publishes.
type FinalFitResult struct {
	// Budget is the final-answer budget to run at: min(ConfiguredFinal, fit),
	// floored at FinalBudgetFloor (itself never above ConfiguredFinal).
	Budget int
	// Fit is the raw arithmetic before the min and the floor — the number of
	// tokens the remaining wall actually buys for ONE final turn. 0 when there
	// was no rate or no wall to compute from.
	Fit int
	// Floored: the wall buys less than FinalBudgetFloor, so the floor stands
	// and the wall will be what stops the run, exactly as it does today.
	Floored bool
	// Note is the one-line arithmetic, empty when the configured budget was
	// left untouched — an operator must never read a throttling note about a
	// run that was not throttled.
	Note string
}

// FitFinalBudget computes the largest final-answer budget that FITS the wall
// that is left, at the seat's measured rate (register D-95).
//
//	fit = (remaining_wall − other − safety) × tok_s / turns
//	final = min(configured, fit), floored at FinalBudgetFloor
//
// where `turns` is 2 when an output_schema is set (the final answer and its
// structured re-pack are both decoded on this seat, inside this wall) and 1
// otherwise. Measured 2026-09-14 on the Lenovo 4B seat at ~15 tok/s: a 900 s
// contract with a schema and the configured 8,192-token final owed ≈ 1,420 s,
// so METHODOLOGY.md and SELF-CONTROL.md hit the wall instead of answering.
//
// The fit is a CEILING. It never raises a budget, never exceeds the configured
// cap, and returns the configured budget unchanged — with no note — whenever
// there is no rate, no wall, or enough room.
func FitFinalBudget(in FinalFit) FinalFitResult {
	cfg := in.ConfiguredFinal
	if cfg <= 0 {
		cfg = FinalBudgetFor(0)
	}
	res := FinalFitResult{Budget: cfg}
	if in.TokS <= 0 || in.RemainingSec <= 0 {
		// No sample and no clock are the same answer: say nothing, change
		// nothing. The wall remains the stop, exactly as before D-95.
		return res
	}
	safety := in.RemainingSec * finalFitSafetyFrac
	if safety < finalFitSafetyMinSec {
		safety = finalFitSafetyMinSec
	}
	turns := 1.0
	split := ""
	if in.Schema {
		turns, split = 2, " (split with the output_schema re-pack)"
	}
	avail := in.RemainingSec - in.OtherSec - safety
	fit := 0
	if avail > 0 {
		fit = int(math.Floor(avail * in.TokS / turns))
	}
	res.Fit = fit
	if fit >= cfg {
		return res // the wall already holds the configured budget
	}
	floor := FinalBudgetFloor
	if floor > cfg {
		floor = cfg // the fit is a ceiling: the floor must never RAISE a budget
	}
	if fit < floor {
		res.Budget, res.Floored = floor, true
		res.Note = fmt.Sprintf("final %d → floor %d: %.0f s of wall at %.1f tok/s buys only %d tok%s — the wall will be the stop",
			cfg, floor, in.RemainingSec, in.TokS, fit, split)
		return res
	}
	res.Budget = fit
	res.Note = fmt.Sprintf("final %d → %d to fit %.0f s at %.1f tok/s%s", cfg, fit, in.RemainingSec, in.TokS, split)
	return res
}
