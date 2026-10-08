// eta_repack_fit_test.go: review round 2, BUG item 7 — etaFor must not
// double-count a schema contract's re-pack turn. That review was about a re-pack
// charged at its UNFITTED size once the final had been fitted to the wall. Since
// ADR 0079 the eta fits nothing to the wall: the final AND the re-pack are both
// priced at the reference size (seatrate.FinalBudgetFloor), once each.

package delegate

import (
	"math"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// nodeCShapedGSQ mirrors the diagnosis's <node-c> GSQ shape: 5.4 tok/s, 69 s
// cold, not currently loaded, a step budget of 8192 (matching MaxSteps==1's
// "final = stepTokens" override so the CONFIGURED final is exactly 8192, the
// exact worked example the review cites).
func nodeCShapedGSQ() NodeView {
	v := eligibleRemote()
	v.NodeID = "node-c-gsq"
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

// TestEtaForChargesTheRepackOnceAtTheReferenceSize is the review's worked example under the rule that
// replaced the wall fit (ADR 0079): the <node-c>-shaped seat (5.4 tok/s, configured final 8192) on a 900 s
// auto wall. The old assertion was "eta at or under the wall the final was fitted to"; that bound WAS the
// cap, and it is gone. What the review protected is kept: the re-pack is charged ONCE, and never at the
// unfitted 8192-token size (~3,100 s). A schema contract pays the reference final plus the same again for
// the re-pack; a contract without a schema pays the final only.
func TestEtaForChargesTheRepackOnceAtTheReferenceSize(t *testing.T) {
	st := mechanicalSchemaAutoContract()
	v := nodeCShapedGSQ()

	// Confirm the fixture still reaches the auto-wall cap (900 s) with a configured final of 8192, the
	// worked example: the eta below must be independent of both.
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
	const cold = 69.0
	once := math.Ceil(seatrate.FinalBudgetFloor / 5.4)
	both := math.Ceil(2 * seatrate.FinalBudgetFloor / 5.4)
	if eta != cold+both {
		t.Fatalf("eta = %.0f s, want cold %.0f + the reference final and ONE re-pack %.0f = %.0f (not the re-pack twice, not at the unfitted 8192 tokens)", eta, cold, both, cold+both)
	}
	// Without a schema there is no re-pack: the final only.
	plain := st
	plain.Contract.OutputSchema = nil
	etaPlain, ok := etaFor(plain, v)
	if !ok || etaPlain != cold+once {
		t.Fatalf("a contract with no schema reads eta %.0f s (ok=%v), want cold %.0f + the reference final %.0f = %.0f", etaPlain, ok, cold, once, cold+once)
	}

	// The placement_reason narration must report the same fixed number.
	verdict := chosenVerdictDetail(st, v)
	if !strings.Contains(verdict, "eta") {
		t.Fatalf("chosenVerdictDetail = %q, want it to name the eta", verdict)
	}
}

// TestEtaForPricesTheReferenceFinalWhateverTheSeatsConfiguredFinalIs replaces
// TestEtaForUnchangedWhenTheFinalIsNotFloored, which said a fast seat whose wall fit held its configured
// budget saw an eta "completely unaffected" by the re-pack fix. That is false under ADR 0079: every seat
// prices the SAME work, a reference final (and re-pack) of seatrate.FinalBudgetFloor tokens, so a seat
// configured with a 512-token final and one configured with 8,192 read the same eta at the same rate.
// The sanity bound of the old test stays: a fast seat's eta is a small positive number under the wall.
func TestEtaForPricesTheReferenceFinalWhateverTheSeatsConfiguredFinalIs(t *testing.T) {
	st := oneStepSchemaContract(300, false)                    // explicit 300 s wall
	small := etaFixtureRemote("fast-small-final", 8192, 34, 0) // step budget 512: configured final 512
	big := etaFixtureRemote("fast-big-final", 8192, 34, 0)
	big.SeatBudget = &SeatBudgetView{StepTokens: 8192, Thinking: "off"} // configured final 8192

	eta, ok := etaFor(st, small)
	if !ok {
		t.Fatal("etaFor must have an opinion")
	}
	if eta <= 0 || eta > 300 {
		t.Fatalf("eta = %.1f, want a small positive number well under the 300 s wall (34 tok/s)", eta)
	}
	want := math.Ceil(2 * seatrate.FinalBudgetFloor / 34.0) // the reference final and its re-pack
	if eta != want {
		t.Fatalf("eta = %.1f, want %.0f: the reference final and re-pack (2 x %d tokens) at 34 tok/s, not the seat's configured 512-token ones", eta, want, seatrate.FinalBudgetFloor)
	}
	if etaBig, _ := etaFor(st, big); etaBig != eta {
		t.Fatalf("a seat configured with an 8,192-token final reads eta %.1f, a seat configured with 512 reads %.1f: the same work at the same rate must cost the same", etaBig, eta)
	}
}
