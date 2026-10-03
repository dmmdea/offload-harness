package seatrate

import "testing"

// TestMinViableSecIsOneStepAndAMinimalAnswer pins the floor's arithmetic and what
// it leaves out (register D-102): one tool step (128 tok + a 6 s prefill) and a
// MinimalFinalTokens-token final at the seat's rate, with NO cold load, NO think
// block and NO re-pack, however the contract is configured — those are the
// estimate's terms, never a refusal's.
func TestMinViableSecIsOneStepAndAMinimalAnswer(t *testing.T) {
	// 5 tok/s: step 128/5 + 6 = 31.6, final 64/5 = 12.8 -> 44.4 -> 45.
	in := Input{Seat: "s", TokS: 5, ColdLoadSec: 600, MaxSteps: 12, StepBudget: 4096, FinalBudget: 8192, RepackBudget: 8192, ThinkingAuto: true}
	if got := MinViableSec(in); got != 45 {
		t.Fatalf("MinViableSec = %d, want 45 (one step + 64 tokens at 5 tok/s, nothing else)", got)
	}
	// The full estimate for the same input is far above it: the floor is not the estimate.
	if est := Compute(in); est.TotalSec <= 45 || est.MinTurnSec <= 45 {
		t.Fatalf("test premise: estimate %d / min_turn %d must exceed the floor", est.TotalSec, est.MinTurnSec)
	}
}

// No rate is no opinion: the floor is 0 and a caller never refuses on it.
func TestMinViableSecWithNoRateIsZero(t *testing.T) {
	if got := MinViableSec(Input{Seat: "s", MaxSteps: 12, FinalBudget: 8192}); got != 0 {
		t.Fatalf("MinViableSec with no rate = %d, want 0 (no opinion)", got)
	}
}
