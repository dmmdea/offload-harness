package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// TestWallEstimateForUsesTheSeatsOwnNumbers (0.115.21, register D-03): the
// estimate takes the loop's real budgets (agent_max_tokens → the 4× final
// budget), the contract's step cap, the box's thinking policy, the slower of
// the remembered and the just-observed cold load, and the store's rate over
// the config fallback.
func TestWallEstimateForUsesTheSeatsOwnNumbers(t *testing.T) {
	cfg := config.Config{AgentMaxTokens: 4096, AgentThinking: "off", AgentSeatTokS: 12}
	contract := core.AgentContract{MaxSteps: 12}
	known := seatrate.Seat{TokS: 30, Samples: 3, ColdLoadSec: 180}
	est := wallEstimateFor(cfg, contract, "agent-pool", known, 210, 600)
	// store rate (30) over config (12); cold load = the observed 210 over the remembered 180
	if !strings.Contains(est.Note, "30.0 tok/s (store, 3 samples)") || !strings.Contains(est.Note, "cold load 210 s") {
		t.Fatalf("note = %q", est.Note)
	}
	if strings.Contains(est.Note, "think block") {
		t.Fatalf("thinking off must add no think block: %q", est.Note)
	}
	if !strings.Contains(est.Note, "final 8192 tok") || !strings.Contains(est.Note, "11 tool steps") {
		t.Fatalf("budgets not derived from the loop's: %q", est.Note)
	}
	if est.TotalSec < 590 || est.TotalSec > 600 || est.MinTurnSec != 484 || est.Below {
		t.Fatalf("estimate = %d s (min_turn %d, below %v), want ≈ 596 / 484 / false under a 600 s wall", est.TotalSec, est.MinTurnSec, est.Below)
	}
	// The contract's own thinking overrides the box; auto adds the think block and pushes a 600 s wall under.
	contract.Thinking = "auto"
	auto := wallEstimateFor(cfg, contract, "agent-pool", known, 210, 600)
	if !strings.Contains(auto.Note, "think block 4096 tok") || !auto.Below {
		t.Fatalf("auto: note = %q below = %v", auto.Note, auto.Below)
	}
	// A one-step contract's single completion runs at the plain step budget
	// (loop.go opens the final-budget turn only on the last of ≥ 2 steps):
	// its final leg is 4096 tokens, not the 8192 of a multi-step run.
	one := wallEstimateFor(cfg, core.AgentContract{MaxSteps: 1, Thinking: "off"}, "agent-pool", known, 210, 600)
	if !strings.Contains(one.Note, "final 4096 tok") || !strings.Contains(one.Note, "0 tool steps") || one.MinTurnSec != 347 {
		t.Fatalf("one-step: note = %q min_turn = %d (want final 4096 tok, 0 tool steps, 347 s)", one.Note, one.MinTurnSec)
	}
	// thinking "on": one think block charged and the note says it is a floor.
	on := wallEstimateFor(cfg, core.AgentContract{Thinking: "on"}, "agent-pool", known, 210, 600)
	if !strings.Contains(on.Note, "think block 4096 tok") || !strings.Contains(on.Note, "this is a floor") || on.TotalSec != auto.TotalSec {
		t.Fatalf("thinking on: note = %q total = %d (auto %d)", on.Note, on.TotalSec, auto.TotalSec)
	}
	// No store rate: the config fallback, named as such.
	fb := wallEstimateFor(cfg, core.AgentContract{}, "agent-pool", seatrate.Seat{}, 0, 300)
	if !strings.Contains(fb.Note, "12.0 tok/s (config agent_seat_tok_s, 0 samples)") || !strings.Contains(fb.Note, "cold load 0 s") {
		t.Fatalf("fallback note = %q", fb.Note)
	}
	// Neither: the note says how a sample gets recorded, no numbers.
	none := wallEstimateFor(config.Config{}, core.AgentContract{}, "qwen3.5-4b-vllm", seatrate.Seat{}, 34, 300)
	if none.TotalSec != 0 || !strings.Contains(none.Note, "no decode-rate sample for qwen3.5-4b-vllm") {
		t.Fatalf("no-rate = %+v", none)
	}
}
