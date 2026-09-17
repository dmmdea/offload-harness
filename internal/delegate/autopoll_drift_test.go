package delegate_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// This file is the ANTI-DRIFT test of register D-116, and it is an EXTERNAL
// test package on purpose: internal/pipeline imports internal/delegate, so
// only a delegate_test file may import the node's side back and run both
// sizings in one process.
//
// The claim under test is the whole design: the delegator's poll bound and the
// node's sized wall are ONE arithmetic fed from two places. If either side
// grows a term the other lacks, this fails — which is the only way a reviewer
// can tell "the two agree" from "the two happen to agree today".

// TestDelegatorBoundEqualsTheNodesSizedWall (D-116, (a)): for the same seat
// numbers and the same contract, pipeline.AutoWallFor (the NODE, reading its
// machine-local seat-rates store and its own config) and delegate's
// autoPollBound (the DELEGATOR, reading what that node advertises on health)
// must produce the same number of seconds.
func TestDelegatorBoundEqualsTheNodesSizedWall(t *testing.T) {
	cases := []struct {
		name        string
		tokS        float64
		coldLoadSec float64
		samples     int
		stepTokens  int
		thinking    string
		maxSteps    int
		schema      bool
	}{
		{"the spec's case: 20 tok/s, 8 steps, an output_schema", 20, 30, 5, 1024, "auto", 8, true},
		{"no output_schema: no re-pack term", 20, 30, 5, 1024, "auto", 8, false},
		{"a one-step contract runs its single completion at the STEP budget", 20, 30, 5, 1024, "auto", 1, true},
		{"thinking off", 20, 30, 5, 1024, "off", 8, true},
		{"thinking on every step", 20, 30, 5, 1024, "on", 12, true},
		{"a 4096-token seat", 15, 45, 12, 4096, "auto", 12, true},
		{"fast enough to clamp at the wire default", 400, 2, 7, 1024, "auto", 4, false},
		{"slow enough to clamp at the cap", 3, 90, 3, 4096, "on", 12, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contract := core.AgentContract{
				SchemaVersion: core.AgentWireSchemaVersion,
				Goal:          "answer the question",
				MaxSteps:      tc.maxSteps,
				TimeoutSec:    core.AgentTimeoutSecDefault,
				TimeoutAuto:   true,
			}
			if tc.schema {
				contract.OutputSchema = json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`)
			}

			// The NODE: its own config and its own seat-rates entry.
			cfg := config.Config{AgentMaxTokens: tc.stepTokens, AgentThinking: tc.thinking}
			known := seatrate.Seat{TokS: tc.tokS, ColdLoadSec: tc.coldLoadSec, Samples: tc.samples}
			nodeWall, nodeNote := pipeline.AutoWallFor(cfg, contract, "remote-seat", known)
			if nodeWall <= 0 {
				t.Fatalf("the node sized no wall for a seat with a rate: %s", nodeNote)
			}

			// The DELEGATOR: the same facts as that node publishes them on
			// health (fleetnode.seatRate / fleetnode.seatBudget).
			view := delegate.NodeView{
				NodeID:    "node-a",
				AgentSeat: "remote-seat",
				SeatRate: &delegate.SeatRateView{
					TokS: tc.tokS, ColdLoadSec: tc.coldLoadSec, Samples: tc.samples,
					MinTurnSec: seatrate.MinTurnFor(tc.coldLoadSec, seatrate.FinalBudgetFor(tc.stepTokens), 0, tc.tokS),
				},
				SeatBudget: &delegate.SeatBudgetView{
					StepTokens: tc.stepTokens, FinalTokens: seatrate.FinalBudgetFor(tc.stepTokens), Thinking: tc.thinking,
				},
			}
			bound, note := delegate.AutoPollBoundForTest(view, contract)

			want := time.Duration(nodeWall) * delegate.PollSecondForTest()
			if bound != want {
				t.Fatalf("the two clocks disagree: the node would run a %d s wall (%s) and the delegator polls %s (%s)",
					nodeWall, nodeNote, bound, note)
			}
			if nodeWall < core.AgentTimeoutSecDefault || nodeWall > core.AgentTimeoutSecCap {
				t.Fatalf("the shared sizing returned %d s, outside the wire bounds [%d, %d]", nodeWall, core.AgentTimeoutSecDefault, core.AgentTimeoutSecCap)
			}
		})
	}
}
