package seatrate

import (
	"strings"
	"testing"
)

// TestFinalBudgetFitsTheWall is the D-95 regression, measured 2026-09-14 on the
// Lenovo 4B seat (qwen3.5-4b-vllm, ~15 tok/s): a list-heavy grounded extraction
// with an output_schema ran its final answer at the configured 8,192-token
// budget, and the harness's own wall_note predicted 1,166–1,310 s for
// final + re-pack against a 900 s wall. METHODOLOGY.md and SELF-CONTROL.md hit
// that wall instead of answering. The fit is the fix: the final budget is
// narrowed to what the REMAINING wall can decode at the seat's measured rate,
// split across the final answer and its re-pack when a schema is set.
func TestFinalBudgetFitsTheWall(t *testing.T) {
	slow := Input{
		Seat: "qwen3.5-4b-vllm", TokS: 15, RateSamples: 4, ColdLoadSec: 34,
		MaxSteps: 12, StepBudget: 2048, FinalBudget: 8192, RepackBudget: 8192,
		ThinkingAuto: true, TimeoutSec: 900,
	}
	est := Compute(slow)
	if est.OtherSec <= 0 || est.OtherSec >= est.TotalSec {
		t.Fatalf("Estimate.OtherSec = %d of a %d s total — the non-final terms must be published on their own so the fit can subtract them", est.OtherSec, est.TotalSec)
	}
	// The configured budget is what blew the wall: everything the run owes
	// besides the answer, plus two 8,192-token turns at 15 tok/s.
	if unfitted := float64(est.OtherSec) + 2*8192.0/15; unfitted <= 900 {
		t.Fatalf("the D-95 case must be OVER its 900 s wall at the configured final: %.0f s", unfitted)
	}

	fit := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 900, OtherSec: float64(est.OtherSec), TokS: 15, Schema: true})
	if fit.Budget >= 8192 || fit.Budget < FinalBudgetFloor {
		t.Fatalf("fitted final budget = %d, want a narrowed budget in [%d, 8192)", fit.Budget, FinalBudgetFloor)
	}
	if spent := float64(est.OtherSec) + 2*float64(fit.Budget)/15; spent > 900 {
		t.Fatalf("the fitted budget does not fit: %.0f s of a 900 s wall (final %d + its re-pack at 15 tok/s)", spent, fit.Budget)
	}
	if !strings.Contains(fit.Note, "final 8192 →") || !strings.Contains(fit.Note, "15.0 tok/s") || !strings.Contains(fit.Note, "900 s") {
		t.Fatalf("budget_note must name the arithmetic (old budget, new budget, wall, rate): %q", fit.Note)
	}
	if !strings.Contains(fit.Note, "re-pack") {
		t.Fatalf("a schema contract's note must say the fit is split with the re-pack: %q", fit.Note)
	}
}

// TestFastSeatKeepsTheConfiguredFinalBudget: the fit may only NARROW. A seat
// whose wall comfortably holds the configured final runs exactly as before —
// same budget, no note, nothing published that would make an operator think a
// healthy run was throttled.
func TestFastSeatKeepsTheConfiguredFinalBudget(t *testing.T) {
	est := Compute(Input{TokS: 60, ColdLoadSec: 34, MaxSteps: 12, StepBudget: 2048, FinalBudget: 8192, RepackBudget: 8192, ThinkingAuto: true, TimeoutSec: 900})
	fit := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 900, OtherSec: float64(est.OtherSec), TokS: 60, Schema: true})
	if fit.Budget != 8192 || fit.Note != "" || fit.Floored {
		t.Fatalf("a 60 tok/s seat must keep the configured 8192 final untouched, got %+v", fit)
	}
}

// TestFitNeverRaisesAndStopsAtTheFloor pins the three boundaries: no rate means
// no opinion (the wall stays the stop, exactly as before the fit existed), a
// wall too small for a usable answer stops at FinalBudgetFloor and SAYS the
// wall will be the stop, and neither the floor nor a huge wall may ever raise
// the budget above the configured cap.
func TestFitNeverRaisesAndStopsAtTheFloor(t *testing.T) {
	if got := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 900, TokS: 0}); got.Budget != 8192 || got.Note != "" {
		t.Fatalf("an unknown seat rate must leave the budget alone: %+v", got)
	}
	if got := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 0, TokS: 15}); got.Budget != 8192 || got.Note != "" {
		t.Fatalf("an unknown remaining wall must leave the budget alone: %+v", got)
	}

	floored := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 60, TokS: 5, Schema: true})
	if floored.Budget != FinalBudgetFloor || !floored.Floored {
		t.Fatalf("want the %d floor with Floored set, got %+v", FinalBudgetFloor, floored)
	}
	if !strings.Contains(floored.Note, "the wall will be the stop") {
		t.Fatalf("the floor note must say the wall is the stop: %q", floored.Note)
	}

	if got := FitFinalBudget(FinalFit{ConfiguredFinal: 512, RemainingSec: 60, TokS: 5}); got.Budget != 512 {
		t.Fatalf("the floor must never RAISE a smaller configured budget: %d", got.Budget)
	}
	if got := FitFinalBudget(FinalFit{ConfiguredFinal: 4096, RemainingSec: 100000, TokS: 60}); got.Budget != 4096 {
		t.Fatalf("the fit is a ceiling, never a raise: %d", got.Budget)
	}
}

// TestFitDropsTheSpentStepsOnceTheFinalTurnIsReached: the loop recomputes the
// fit at the forced final step, where the tool steps are already spent — the
// same wall then buys a bigger answer than it did at run start. Without this
// the mid-run recompute would keep charging for work that already happened.
func TestFitDropsTheSpentStepsOnceTheFinalTurnIsReached(t *testing.T) {
	atStart := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 900, OtherSec: 331, TokS: 15, Schema: true})
	atFinal := FitFinalBudget(FinalFit{ConfiguredFinal: 8192, RemainingSec: 900, OtherSec: 0, TokS: 15, Schema: true})
	if atFinal.Budget <= atStart.Budget {
		t.Fatalf("with the steps spent the same wall must buy MORE answer: start %d, final %d", atStart.Budget, atFinal.Budget)
	}
}
