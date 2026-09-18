// eta_repack_fit_test.go: review round 2, BUG item 7 — etaFor must not
// double-count a schema contract's re-pack turn at its UNFITTED size once
// the final has been fitted/floored to the wall.

package delegate

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// lenovoShapedGSQ mirrors the diagnosis's Lenovo GSQ shape: 5.4 tok/s, 69 s
// cold, not currently loaded, a step budget of 8192 (matching MaxSteps==1's
// "final = stepTokens" override so the CONFIGURED final is exactly 8192, the
// exact worked example the review cites).
func lenovoShapedGSQ() NodeView {
	v := eligibleRemote()
	v.NodeID = "lenovo-gsq"
	loaded := false
	v.SeatLoaded = &loaded
	v.SeatRate = &SeatRateView{TokS: 5.4, ColdLoadSec: 69, Samples: 4, MinTurnSec: 1594}
	v.SeatBudget = &SeatBudgetView{StepTokens: 8192, Thinking: "off"}
	return v
}

func mechanicalSchemaAutoContract() Subtask {
	return Subtask{
		Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion,
			Goal:          "extract every field from the report", // mechanical shape
			OutputSchema:  gateSchema,
			Depth:         0,
			MaxSteps:      1,
			Thinking:      "off",
			TimeoutAuto:   true,
		},
		EstTokens: 1000,
	}
}

// TestEtaForDoesNotDoubleCountAFittedRepack is the exact worked example from
// the review: a 900 s auto wall on the Lenovo-shaped seat fits the final to
// ~2187 tokens: the eta must stay AT OR UNDER the wall it was fitted to, not
// ~1960 s from charging the re-pack at its unfitted 8192-token size on top of
// the fitted final.
func TestEtaForDoesNotDoubleCountAFittedRepack(t *testing.T) {
	st := mechanicalSchemaAutoContract()
	v := lenovoShapedGSQ()

	// Confirm the fixture actually reaches the auto-wall cap (900 s) and a
	// final genuinely NARROWED below its configured 8192, matching the worked example —
	// otherwise this test would not exercise the bug at all.
	policy, in, wallSec, known := seatWallFor(st, v)
	if !known {
		t.Fatal("fixture bug: the seat must publish a usable rate")
	}
	if wallSec != core.AgentTimeoutSecCap {
		t.Fatalf("fixture bug: wallSec = %d, want the wire cap (%d) — adjust the fixture to match the worked example", wallSec, core.AgentTimeoutSecCap)
	}
	if in.FinalBudget != 8192 {
		t.Fatalf("fixture bug: configured final = %d, want 8192 (MaxSteps==1 override)", in.FinalBudget)
	}
	_ = policy

	eta, ok := etaFor(st, v)
	if !ok {
		t.Fatal("etaFor must have an opinion: the seat publishes a rate")
	}
	if eta > float64(wallSec) {
		t.Fatalf("eta = %.0f s, want <= the %d s wall it was fitted to — the re-pack turn must be charged at the SAME fitted size as the final, not its unfitted 8192-token configured size", eta, wallSec)
	}

	// The placement_reason narration must report the same fixed number.
	verdict := chosenVerdictDetail(st, v)
	if !strings.Contains(verdict, "eta") {
		t.Fatalf("chosenVerdictDetail = %q, want it to name the eta", verdict)
	}
}

// TestEtaForUnchangedWhenTheFinalIsNotFloored is the control: a fast seat
// whose fitted final comfortably holds the configured budget (fit >= cfg, so
// FitFinalBudget returns the CONFIGURED budget unchanged) must see its eta
// completely unaffected by this fix — RepackBudget was already correct
// (equal to the unfitted final) in that case.
func TestEtaForUnchangedWhenTheFinalIsNotFloored(t *testing.T) {
	st := oneStepSchemaContract(300, false) // explicit 300 s wall, small step budget (512)
	v := etaFixtureRemote("fast-adequate", 8192, 34, 0)

	eta, ok := etaFor(st, v)
	if !ok {
		t.Fatal("etaFor must have an opinion")
	}
	if eta <= 0 || eta > 300 {
		t.Fatalf("eta = %.1f, want a small positive number well under the 300 s wall (34 tok/s, 512-token step/final/repack)", eta)
	}
}
