package delegate_test

import (
	"encoding/json"
	"strings"
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
			bound, note := delegate.AutoPollBoundForTest(view, contract, "remote-seat")

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

// TestTheLayerSeatIsNotSizedFromTheAgentSeatsRate (D-116 review finding 2):
// "one arithmetic, two sources of numbers" is one CLOCK only while both halves
// are fed the same seat. The test above always hands both sides the same one,
// which certifies the shared function and never the seat identity — so this
// drives the two entry points with the seats a COMPOSITE placement actually
// produces: the delegator dispatches a layer, the node re-decides it, switches
// to that layer's seat and sizes its wall from THAT seat's rate, while health
// advertises the planner seat's rate alone.
//
// The delegator cannot size that wall, so it must not pretend to: the bound
// falls back to the cap, which is >= anything the node can choose.
func TestTheLayerSeatIsNotSizedFromTheAgentSeatsRate(t *testing.T) {
	contract := core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "answer the question",
		OutputSchema:  json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`),
		MaxSteps:      12,
		TimeoutSec:    core.AgentTimeoutSecDefault,
		TimeoutAuto:   true,
	}

	// The NODE: it runs the dispatched layer's seat — slow and large, which is
	// the entire point of a long layer — and sizes its wall from that rate.
	cfg := config.Config{AgentMaxTokens: 4096, AgentThinking: "on"}
	nodeWall, nodeNote := pipeline.AutoWallFor(cfg, contract, "long-27b",
		seatrate.Seat{TokS: 5, ColdLoadSec: 120, Samples: 6})
	if nodeWall <= 0 {
		t.Fatalf("the node sized no wall: %s", nodeNote)
	}

	// The DELEGATOR: health carries the PLANNER seat's numbers, and nothing at
	// all about long-27b (placement.LayerRow publishes seats, never rates).
	view := delegate.NodeView{
		NodeID:     "node-a",
		AgentSeat:  "planner-4b",
		SeatRate:   &delegate.SeatRateView{TokS: 120, ColdLoadSec: 8, Samples: 9},
		SeatBudget: &delegate.SeatBudgetView{StepTokens: 4096, FinalTokens: seatrate.FinalBudgetFor(4096), Thinking: "on"},
	}
	bound, note := delegate.AutoPollBoundForTest(view, contract, "long-27b")

	if want := time.Duration(core.AgentTimeoutSecCap) * delegate.PollSecondForTest(); bound != want {
		t.Fatalf("bound = %s, want the cap %s: the delegator sized a clock from a seat the run does not use (%s)", bound, want, note)
	}
	if nodeSized := time.Duration(nodeWall) * delegate.PollSecondForTest(); bound < nodeSized {
		t.Fatalf("the two clocks disagree: the node would run a %d s wall (%s) and the delegator polls %s (%s)",
			nodeWall, nodeNote, bound, note)
	}
	if !strings.Contains(note, "long-27b") || !strings.Contains(note, "planner-4b") {
		t.Errorf("note = %q, want it to name the run seat and the one whose rate IS advertised", note)
	}
}
