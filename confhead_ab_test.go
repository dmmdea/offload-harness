package main

import (
	"testing"

	"github.com/dmmdea/local-offload/internal/eval"
)

// oc builds a one-task Outcome with the given accepted/correct/escalated/tokens.
func oc(task string, accepted, correct bool, escalations, tokensOut int, margin float64) eval.Outcome {
	return eval.Outcome{
		Case:        eval.Case{Task: task},
		Accepted:    accepted,
		Correct:     correct,
		Escalations: escalations,
		TokensOut:   tokensOut,
		Margin:      margin,
	}
}

// TestABOffOffDeterminismTies: when the ON arm is forced confhead-off, it is the
// SAME run as the OFF arm — every metric ties exactly. Proves the confhead toggle
// is the ONLY variable: identical outcomes => identical arm metrics => audc/sel_acc
// tie and the verdict is vacuous (escalation_delta==0).
func TestABOffOffDeterminismTies(t *testing.T) {
	// A representative spread: some correct, some wrong, none escalated.
	arm := []eval.Outcome{
		oc("triage", true, true, 0, 100, 0.6),
		oc("triage", true, false, 0, 100, 0.2),
		oc("triage", true, true, 0, 100, 0.9),
		oc("triage", false, false, 0, 0, 0), // deferred
	}
	entry := arm // entry-only baseline is the same when escalation is off

	off := armMetricsFor(arm, entry)
	on := armMetricsFor(arm, entry) // OFF/OFF: identical input

	if off.SelectiveAcc != on.SelectiveAcc {
		t.Fatalf("OFF/OFF selective_acc must tie: off=%v on=%v", off.SelectiveAcc, on.SelectiveAcc)
	}
	if off.AUDC != on.AUDC {
		t.Fatalf("OFF/OFF audc must tie: off=%v on=%v", off.AUDC, on.AUDC)
	}
	if off.AvgCost != on.AvgCost || off.Coverage != on.Coverage || off.Peak != on.Peak {
		t.Fatalf("OFF/OFF all metrics must tie: %+v vs %+v", off, on)
	}

	delta := escalationDelta(arm, arm)
	if delta != 0 {
		t.Fatalf("OFF/OFF escalation_delta must be 0, got %d", delta)
	}

	tm := computeABTask(off, on, off.SelectiveAcc, delta, 0.0, 1.0)
	if tm.SelectiveAccOn != tm.SelectiveAccOff {
		t.Fatalf("emitted selective_acc must tie: %+v", tm)
	}
	if tm.AUDCOn != tm.AUDCOff {
		t.Fatalf("emitted audc must tie: %+v", tm)
	}
	if !tm.Vacuous {
		t.Fatalf("OFF/OFF must be flagged vacuous, got %+v", tm)
	}
	if tm.FrontierWin {
		t.Fatalf("a vacuous (no-behavior-change) A/B must NOT be a frontier win, got %+v", tm)
	}
}

// TestEscalationDeltaCounts: the delta counts only cases where ON escalated but
// OFF did not. Case order alignment between the two arms is what makes this valid.
func TestEscalationDeltaCounts(t *testing.T) {
	off := []eval.Outcome{
		oc("triage", true, true, 0, 100, 0.6), // off: no escalation
		oc("triage", true, true, 0, 100, 0.6), // off: no escalation
		oc("triage", true, true, 1, 200, 0.6), // off: already escalated (not counted)
		oc("triage", true, true, 0, 100, 0.6),
	}
	on := []eval.Outcome{
		oc("triage", true, true, 1, 200, 0.6), // on escalated, off didn't -> +1
		oc("triage", true, true, 0, 100, 0.6), // neither -> 0
		oc("triage", true, true, 1, 200, 0.6), // both escalated -> not counted (off already had)
		oc("triage", true, true, 2, 300, 0.6), // on escalated, off didn't -> +1
	}
	if got := escalationDelta(off, on); got != 2 {
		t.Fatalf("escalation_delta want 2, got %d", got)
	}
}

// TestVerdictDominate: ON strictly improves selective_acc at no extra cost, with
// real escalations (non-vacuous) and beats the calibrated baseline => frontier_win.
func TestVerdictDominate(t *testing.T) {
	off := abArmMetrics{SelectiveAcc: 0.80, Coverage: 1.0, AvgCost: 100, AUDC: 0.80, Peak: 0.80}
	on := abArmMetrics{SelectiveAcc: 0.90, Coverage: 1.0, AvgCost: 100, AUDC: 0.85, Peak: 0.90}
	tm := computeABTask(off, on, 0.82 /*calib*/, 5 /*escDelta*/, 0.0, 1.0)
	if !tm.FrontierWin {
		t.Fatalf("ON dominates and beats calib at no extra cost => frontier_win, got %+v", tm)
	}
	if tm.Vacuous {
		t.Fatalf("escalation_delta=5 must not be vacuous, got %+v", tm)
	}
}

// TestVerdictAccuracyRegress: ON regresses selective_acc beyond eps => no win.
func TestVerdictAccuracyRegress(t *testing.T) {
	off := abArmMetrics{SelectiveAcc: 0.90, AvgCost: 100, AUDC: 0.90}
	on := abArmMetrics{SelectiveAcc: 0.80, AvgCost: 100, AUDC: 0.90}
	tm := computeABTask(off, on, 0.0, 5, 0.0, 1.0)
	if tm.FrontierWin {
		t.Fatalf("accuracy regression must NOT be a frontier win, got %+v", tm)
	}
}

// TestVerdictCostBlowup: ON keeps/improves accuracy but blows the cost budget => no win.
func TestVerdictCostBlowup(t *testing.T) {
	off := abArmMetrics{SelectiveAcc: 0.90, AvgCost: 100, AUDC: 0.90}
	on := abArmMetrics{SelectiveAcc: 0.95, AvgCost: 250, AUDC: 0.95} // 2.5x cost
	tm := computeABTask(off, on, 0.0, 5, 0.0, 1.0)                   // budget 1.0 = no increase
	if tm.FrontierWin {
		t.Fatalf("cost blow-up beyond budget must NOT be a frontier win, got %+v", tm)
	}
}

// TestVerdictLosesToCalibratedBaseline: ON keeps accuracy and cost but does not
// beat the calibrated-margin baseline => no win (the head adds nothing over a
// calibrated threshold).
func TestVerdictLosesToCalibratedBaseline(t *testing.T) {
	off := abArmMetrics{SelectiveAcc: 0.85, AvgCost: 100, AUDC: 0.85}
	on := abArmMetrics{SelectiveAcc: 0.88, AvgCost: 100, AUDC: 0.88}
	tm := computeABTask(off, on, 0.92 /*calib beats ON*/, 5, 0.0, 1.0)
	if tm.FrontierWin {
		t.Fatalf("ON below the calibrated baseline must NOT be a frontier win, got %+v", tm)
	}
}

// TestVerdictVacuousFlagged: escalation_delta==0 forces vacuous + no win even if
// every numeric criterion would otherwise pass.
func TestVerdictVacuousFlagged(t *testing.T) {
	off := abArmMetrics{SelectiveAcc: 0.80, AvgCost: 100, AUDC: 0.80}
	on := abArmMetrics{SelectiveAcc: 0.95, AvgCost: 90, AUDC: 0.95} // would-be win
	tm := computeABTask(off, on, 0.0, 0 /*escDelta=0*/, 0.0, 1.0)
	if !tm.Vacuous {
		t.Fatalf("escalation_delta=0 must be vacuous, got %+v", tm)
	}
	if tm.FrontierWin {
		t.Fatalf("a vacuous A/B must never be a frontier win, got %+v", tm)
	}
}

// TestRecommendationFold: ENABLE requires gate-1 ADOPT AND all tasks frontier_win.
func TestRecommendationFold(t *testing.T) {
	winTasks := map[string]abTaskMetrics{
		"classify": {FrontierWin: true},
		"triage":   {FrontierWin: true},
	}
	mixedTasks := map[string]abTaskMetrics{
		"classify": {FrontierWin: true},
		"triage":   {FrontierWin: false},
	}

	if rec, en := abRecommendation(true, winTasks); !en || rec != "ENABLE" {
		t.Fatalf("gate-1 ADOPT + all win => ENABLE, got %q en=%v", rec, en)
	}
	if rec, en := abRecommendation(false, winTasks); en || rec != "NOT-ENABLE" {
		t.Fatalf("gate-1 REJECT => NOT-ENABLE even if all tasks win, got %q en=%v", rec, en)
	}
	if rec, en := abRecommendation(true, mixedTasks); en || rec != "NOT-ENABLE" {
		t.Fatalf("a non-winning task => NOT-ENABLE, got %q en=%v", rec, en)
	}
	if rec, en := abRecommendation(true, map[string]abTaskMetrics{}); en || rec != "NOT-ENABLE" {
		t.Fatalf("no tasks => NOT-ENABLE, got %q en=%v", rec, en)
	}
}

// TestCalibratedMarginSelAcc: with a clean margin/correct separation, the
// calibrated threshold accepts the high-margin (correct) rows and the selective
// accuracy of the accepted set is high. Sanity check the comparator is wired to
// confhead.SelectThreshold and not degenerate.
func TestCalibratedMarginSelAcc(t *testing.T) {
	var outs []eval.Outcome
	// 10 high-margin correct, 10 low-margin wrong.
	for i := 0; i < 10; i++ {
		outs = append(outs, oc("triage", true, true, 0, 100, 0.9))
	}
	for i := 0; i < 10; i++ {
		outs = append(outs, oc("triage", true, false, 0, 100, 0.1))
	}
	sel, ok := calibratedMarginSelAcc(outs, 0.15)
	if !ok {
		t.Fatalf("expected a usable calibrated baseline")
	}
	// The accepted set (margin >= tau) should be majority-correct; with a clean
	// split tau lands above the wrong rows so accepted accuracy is high.
	if sel < 0.9 {
		t.Fatalf("clean split should give high calibrated selective acc, got %v", sel)
	}
}

// TestCalibratedMarginSelAcc_TooFew: fewer than 2 usable pairs => not ok.
func TestCalibratedMarginSelAcc_TooFew(t *testing.T) {
	outs := []eval.Outcome{oc("triage", true, true, 0, 100, 0.9)}
	if _, ok := calibratedMarginSelAcc(outs, 0.15); ok {
		t.Fatalf("a single pair must not yield a usable baseline")
	}
}
