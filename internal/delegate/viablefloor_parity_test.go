// viablefloor_parity_test.go pins ONE floor across two doors (register D-102).
//
// The delegator refuses a remote placement whose wall cannot hold one tool step
// and a minimal final (feasibleFinal, the INV-5 rider's clause (i)); the
// agent_run door applies the same refusal to a local run through
// seatrate.MinViableSec. They are two copies of one arithmetic, so a change to
// either that the other does not follow would let one door admit a wall the
// other calls infeasible for the same seat. This test fails when they drift.

package delegate

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

func TestMinViableSecMatchesTheDelegatorsFeasibilityFloor(t *testing.T) {
	cases := []struct {
		name     string
		tokS     float64
		cold     float64
		step     int
		thinking string
		steps    int
		schema   bool
	}{
		{"slow 27B, schema, thinking auto", 5.4, 69, 8192, "auto", 12, true},
		{"mid seat, no schema, thinking off", 24.6, 34, 1024, "off", 6, false},
		{"fast seat, thinking on", 38.9, 28.4, 2048, "on", 12, false},
		{"one-step contract", 7.17, 210, 4096, "off", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := eligibleRemote()
			v.SeatRate = &SeatRateView{TokS: tc.tokS, ColdLoadSec: tc.cold, Samples: 4}
			v.SeatBudget = &SeatBudgetView{StepTokens: tc.step, Thinking: tc.thinking}
			contract := core.AgentContract{
				SchemaVersion: core.AgentWireSchemaVersion, Goal: "summarize the docs", MaxSteps: tc.steps, Depth: 0,
			}
			if tc.schema {
				contract.OutputSchema = gateSchema
			}
			policy := seatrate.SeatPolicy{Seat: v.AgentSeat, TokS: tc.tokS, ColdLoadSec: tc.cold, StepTokens: tc.step, Thinking: tc.thinking}
			floor := seatrate.MinViableSec(seatrate.InputFor(policy, contract))
			if floor <= 0 {
				t.Fatalf("a rated seat must have a floor, got %d", floor)
			}
			held := Subtask{Contract: contract, EstTokens: 1000}
			held.Contract.TimeoutSec = floor
			if ok, reason := feasibleFinal(held, v); !ok {
				t.Errorf("a wall of exactly the floor (%d s) must be feasible on the delegator too: %s", floor, reason)
			}
			short := Subtask{Contract: contract, EstTokens: 1000}
			short.Contract.TimeoutSec = floor - 1
			if ok, _ := feasibleFinal(short, v); ok {
				t.Errorf("a wall one second under the floor (%d s) must be refused by the delegator as well", floor-1)
			}
		})
	}
}
