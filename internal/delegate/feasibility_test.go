// feasibility_test.go: W-05 (PR-5 item 4, register S-03/S-05) — a seat that
// cannot produce one tool step and a minimal (64-token) answer within the
// contract's own effective wall is refused, naming the arithmetic. This is
// deliberately NOT the seat's published min_turn_sec (its max-final worst
// case), and since 0.128.1 not a fit of the configured final against
// seatrate.FinalBudgetFloor either: the INV-5 rider permits a refusal only
// "below a minimum viable final"; everything above that is etaFor's ranking.

package delegate

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// lenovoShapedSlow is the Lenovo GSQ 27B shape from the diagnosis: 5.4 tok/s,
// 69 s cold, not currently loaded — a seat that holds one step and a minimal
// answer in a 300 s wall (a ranking matter) but not in a 20 s one (refused).
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

func TestFeasibleFinalExcludesAWallTooShortForOneStepAndAMinimalAnswer(t *testing.T) {
	v := lenovoShapedSlow()
	// 5.4 tok/s: one tool step (128 tok + 6 s prefill) and a 64-token final need
	// ~42 s. A 20 s wall cannot hold ANY answer — that, and only that, is the
	// INV-5 rider's refusal. (The cold load is NOT charged: admission pays it
	// outside the wall.)
	st := oneStepSchemaContract(20, false)
	ok, reason := feasibleFinal(st, v)
	if ok {
		t.Fatalf("a 5.4 tok/s seat cannot produce one step and a minimal answer in 20 s; reason=%q", reason)
	}
	for _, want := range []string{"one step", "5.4", "the wall is 20 s"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to contain %q", reason, want)
		}
	}
	// remoteEligible must refuse on the same arithmetic.
	if remoteEligible(st, v) {
		t.Fatal("remoteEligible must refuse a seat feasibleFinal excludes")
	}
}

// The 0.128.0 regression, pinned: fleet-smoke's contract (an explicit 60 s wall,
// the default 12 steps, a schema, thinking auto) reached a COLD Aorus and was
// refused "fitted final 0 < floor 1024" although the seat completes it in ~25 s;
// the cold load was subtracted from a wall that never pays it, and a floor sized
// for a full answer was applied to a one-token reply. Feasibility asks only
// whether one step and a minimal answer fit; the fitted final is the eta's.
func TestFeasibleFinalAdmitsAShortExplicitWallOnAColdSeat(t *testing.T) {
	v := eligibleRemote()
	loaded := false
	v.SeatRate = &SeatRateView{TokS: 38.9, ColdLoadSec: 28.4, Samples: 242, MinTurnSec: 134}
	v.SeatBudget = &SeatBudgetView{StepTokens: 1024, Thinking: "auto"}
	v.SeatLoaded = &loaded
	st := oneStepSchemaContract(60, false)
	st.Contract.MaxSteps = 12
	st.Contract.Thinking = ""
	ok, reason := feasibleFinal(st, v)
	if !ok {
		t.Fatalf("a cold 38.9 tok/s seat must be eligible for a 60 s contract it completes in ~25 s; reason=%q", reason)
	}
	if !remoteEligible(st, v) {
		t.Fatal("remoteEligible must admit the cold fast seat on a short explicit wall")
	}
	// And the slow seat on a 300 s wall is a RANKING matter (eta), not a refusal:
	// it answered this class of contract in the smoke at 26 s after admission.
	slow := lenovoShapedSlow()
	if ok, reason := feasibleFinal(oneStepSchemaContract(300, false), slow); !ok {
		t.Fatalf("a 5.4 tok/s seat holds one step and a minimal answer in 300 s; reason=%q", reason)
	}
}

// The eta never exceeds cold + wall: the wall is the stop, so a floored fit on a
// slow seat reads as "cold, then the whole wall", never as the configured
// budgets' arithmetic (0.128.0 printed "eta 4682 s" for a 60 s wall).
func TestEtaForNeverExceedsColdPlusWall(t *testing.T) {
	v := lenovoShapedSlow()
	st := oneStepSchemaContract(60, false)
	eta, ok := etaFor(st, v)
	if !ok {
		t.Fatal("a published rate must yield an eta")
	}
	if eta > 69+60+0.5 {
		t.Fatalf("eta = %.0f s, want <= cold 69 + wall 60", eta)
	}
	if eta < 69 {
		t.Fatalf("eta = %.0f s, want >= the cold load it must pay", eta)
	}
}

func TestFeasibleFinalEligibleUnderTimeoutAuto(t *testing.T) {
	v := lenovoShapedSlow()
	// The SAME node under a timeout_auto contract: the wall is sized by the
	// node's own slow rate (clamped to the wire cap, up to 900 s), and the
	// feasibility floor reads THAT wall — one step and a minimal answer fit
	// with room to spare.
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
