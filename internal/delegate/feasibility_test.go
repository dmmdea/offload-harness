// feasibility_test.go: W-05 (PR-5 item 4, register S-03/S-05) — a seat whose
// FITTED final cannot clear seatrate.FinalBudgetFloor within its own
// effective wall is refused, naming the arithmetic. This is deliberately NOT
// the seat's published min_turn_sec (its max-final worst case): the INV-5
// rider forbids gating on that, only on a floor derived from the contract's
// own fitted final at the seat's measured rate.

package delegate

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// lenovoShapedSlow is the Lenovo GSQ 27B shape from the diagnosis: 5.4 tok/s,
// 69 s cold, not currently loaded — the seat that a 300 s explicit wall
// cannot hold even the floor of a final answer on.
func lenovoShapedSlow() NodeView {
	v := eligibleRemote()
	loaded := false
	v.SeatRate = &SeatRateView{TokS: 5.4, ColdLoadSec: 69, Samples: 4, MinTurnSec: 1594}
	// StepTokens=8192 + MaxSteps=1 (set on the contract) makes InputFor's
	// FinalBudgets override final = stepTokens directly (the one-step rule),
	// so the configured final is exactly 8192 without depending on the 4x/cap
	// arithmetic — the number the item's own worked example names.
	v.SeatBudget = &SeatBudgetView{StepTokens: 8192, Thinking: "off"}
	v.SeatLoaded = &loaded
	return v
}

func oneStepSchemaContract(timeoutSec int, auto bool) Subtask {
	return Subtask{
		Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion,
			Goal:          "summarize the docs",
			OutputSchema:  gateSchema,
			Depth:         0,
			MaxSteps:      1,
			Thinking:      "off",
			TimeoutSec:    timeoutSec,
			TimeoutAuto:   auto,
		},
		EstTokens: 1000,
	}
}

func TestFeasibleFinalExcludesASeatThatCannotHoldTheFloor(t *testing.T) {
	v := lenovoShapedSlow()
	st := oneStepSchemaContract(300, false)
	ok, reason := feasibleFinal(st, v)
	if ok {
		t.Fatalf("a 5.4 tok/s seat with a 300 s explicit wall must not hold the floor; reason=%q", reason)
	}
	for _, want := range []string{"fitted final", "5.4"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to contain %q", reason, want)
		}
	}
	// remoteEligible must refuse on the same arithmetic.
	if remoteEligible(st, v) {
		t.Fatal("remoteEligible must refuse a seat feasibleFinal excludes")
	}
}

func TestFeasibleFinalEligibleUnderTimeoutAuto(t *testing.T) {
	v := lenovoShapedSlow()
	// The SAME node: only the wall changes, from a tight explicit 300 s to an
	// auto wall the node's own slow rate sizes generously (clamped to the
	// wire cap, up to 900 s) — enough room for the fitted final to clear the
	// floor.
	st := oneStepSchemaContract(0, true)
	ok, reason := feasibleFinal(st, v)
	if !ok {
		t.Fatalf("a timeout_auto contract on the same slow seat must size its own wall and become eligible; reason=%q", reason)
	}
	if !remoteEligible(st, v) {
		t.Fatal("remoteEligible must admit a seat feasibleFinal clears")
	}
}

func TestFeasibleFinalEligibleOnAFastSeat(t *testing.T) {
	v := eligibleRemote()
	loaded := false
	v.SeatRate = &SeatRateView{TokS: 33.7, ColdLoadSec: 15, Samples: 6, MinTurnSec: 137}
	v.SeatBudget = &SeatBudgetView{StepTokens: 8192, Thinking: "off"}
	v.SeatLoaded = &loaded
	st := oneStepSchemaContract(300, false)
	ok, reason := feasibleFinal(st, v)
	if !ok {
		t.Fatalf("a 33.7 tok/s seat with a 300 s wall must hold the floor; reason=%q", reason)
	}
	if !remoteEligible(st, v) {
		t.Fatal("remoteEligible must admit the fast seat")
	}
}

func TestFeasibleFinalNoOpinionOnUnknownRate(t *testing.T) {
	v := eligibleRemote() // no SeatRate published
	st := oneStepSchemaContract(300, false)
	ok, reason := feasibleFinal(st, v)
	if !ok || reason != "" {
		t.Fatalf("an unpublished rate must be NO OPINION (eligible, no reason); got ok=%v reason=%q", ok, reason)
	}
}

func TestFitColdSecReadsTheTriStateCorrectly(t *testing.T) {
	policy := seatrate.SeatPolicy{ColdLoadSec: 69}
	loadedTrue, loadedFalse, starting := true, false, true
	cases := []struct {
		name string
		v    NodeView
		want float64
	}{
		{"unknown", NodeView{}, 0},
		{"known loaded", NodeView{SeatLoaded: &loadedTrue}, 0},
		{"known not loaded", NodeView{SeatLoaded: &loadedFalse}, 69},
		{"starting (half, regardless of loaded)", NodeView{SeatLoaded: &loadedTrue, SeatStarting: &starting}, 34.5},
	}
	for _, c := range cases {
		if got := fitColdSec(policy, c.v); got != c.want {
			t.Errorf("%s: fitColdSec = %v, want %v", c.name, got, c.want)
		}
	}
}
