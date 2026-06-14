package confhead

import "testing"

// floatEq compares two float64 with a small tolerance.
func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestSelectThresholdTargetAboveBaseError: when the overall accepted error
// already meets the target (target >= base error), no escalation is needed, so
// tau = 0 (accept everything).
func TestSelectThresholdTargetAboveBaseError(t *testing.T) {
	// 8 correct, 2 wrong => base error 0.2.
	scores := []float64{0.9, 0.85, 0.8, 0.75, 0.7, 0.65, 0.6, 0.55, 0.5, 0.45}
	correct := []bool{true, true, true, true, true, true, true, true, false, false}

	// target 0.30 >= base error 0.20 => tau must be 0.
	got := SelectThreshold(scores, correct, 0.30)
	if !floatEq(got, 0.0) {
		t.Fatalf("target>=base: want tau=0, got %v", got)
	}

	// target exactly equal to base error (0.20) also => tau 0 (accepting all meets it).
	got = SelectThreshold(scores, correct, 0.20)
	if !floatEq(got, 0.0) {
		t.Fatalf("target==base: want tau=0, got %v", got)
	}
}

// TestSelectThresholdPerfectlyRankedTargetZero: with all correct strictly above
// all wrong and target 0, tau must land BETWEEN the two groups so the accepted
// set (score >= tau) is pure-correct, and the escalated set is exactly the wrong
// rows.
func TestSelectThresholdPerfectlyRankedTargetZero(t *testing.T) {
	// correct: 0.9,0.8,0.7  ; wrong: 0.4,0.3 . Clean separation at 0.7 vs 0.4.
	scores := []float64{0.9, 0.8, 0.7, 0.4, 0.3}
	correct := []bool{true, true, true, false, false}

	tau := SelectThreshold(scores, correct, 0.0)

	// tau must accept all 3 correct and reject both wrong.
	if !(tau <= 0.7 && tau > 0.4) {
		t.Fatalf("perfectly-ranked target 0: want tau in (0.4, 0.7], got %v", tau)
	}
	// Verify the accepted set is pure correct.
	var accN, accWrong int
	for i, s := range scores {
		if s >= tau {
			accN++
			if !correct[i] {
				accWrong++
			}
		}
	}
	if accWrong != 0 {
		t.Fatalf("accepted set not pure-correct: %d wrong of %d accepted (tau=%v)", accWrong, accN, tau)
	}
	if accN != 3 {
		t.Fatalf("want 3 accepted (the correct rows), got %d (tau=%v)", accN, tau)
	}
}

// TestSelectThresholdConstantScoresUnseparable: constant scores cannot separate
// correct from wrong. With a target below the base error, no accepted subset can
// meet the bound, so tau falls back to the max score (escalate almost
// everything), capped at 1.0.
func TestSelectThresholdConstantScoresUnseparable(t *testing.T) {
	scores := []float64{0.5, 0.5, 0.5, 0.5, 0.5}
	correct := []bool{true, true, true, false, false} // base error 0.4

	tau := SelectThreshold(scores, correct, 0.10) // target well below 0.4
	if !floatEq(tau, 0.5) {
		t.Fatalf("constant scores, target<base: want tau=max=0.5, got %v", tau)
	}
	if tau > 1.0 {
		t.Fatalf("tau must be capped at 1.0, got %v", tau)
	}
}

// TestSelectThresholdMonotoneLowersAcceptedError: in a monotone case, raising tau
// drops the lowest-scoring (wrong) rows first, monotonically lowering accepted
// error. SelectThreshold must pick the SMALLEST tau meeting the target (maximal
// acceptance / minimal escalation) — not an unnecessarily high one.
func TestSelectThresholdMonotoneLowersAcceptedError(t *testing.T) {
	// Scores descending; wrongs concentrated at the bottom.
	// 10 rows: top 6 correct, bottom 4 wrong.
	scores := []float64{0.95, 0.9, 0.85, 0.8, 0.75, 0.7, 0.65, 0.6, 0.55, 0.5}
	correct := []bool{true, true, true, true, true, true, false, false, false, false}
	// base error 0.40.

	// Accepted error as a function of a cutoff among the score grid:
	//   tau=0.65 -> accept 7 (6 correct,1 wrong) raw err 1/7=0.143
	//   tau=0.70 -> accept 6 (all correct) raw err 0
	// With conformal slack (nErr+1)/(nAcc+1):
	//   tau=0.70: (0+1)/(6+1)=0.143 ; tau=0.75: (0+1)/(5+1)=0.167 ; ...
	// Smallest tau whose adjusted error <= 0.15 should be 0.70 (adjusted 0.1428).
	tau := SelectThreshold(scores, correct, 0.15)
	if !floatEq(tau, 0.70) {
		t.Fatalf("monotone: want smallest qualifying tau=0.70, got %v", tau)
	}

	// And raising the target a touch (so 0.65 qualifies) must lower tau:
	// tau=0.65: adjusted (1+1)/(7+1)=0.25 -> too high; tau=0.70 adjusted 0.1428.
	// A target of 0.25 should let tau drop to 0.65.
	tau2 := SelectThreshold(scores, correct, 0.25)
	if !(tau2 <= tau) {
		t.Fatalf("raising target must not raise tau: tau(0.15)=%v tau(0.25)=%v", tau, tau2)
	}
	if !floatEq(tau2, 0.65) {
		t.Fatalf("monotone, target 0.25: want tau=0.65, got %v", tau2)
	}
}

// TestSelectThresholdEmptyInput: defensive — empty inputs yield tau 0.
func TestSelectThresholdEmptyInput(t *testing.T) {
	if got := SelectThreshold(nil, nil, 0.15); !floatEq(got, 0.0) {
		t.Fatalf("empty input: want tau=0, got %v", got)
	}
}
