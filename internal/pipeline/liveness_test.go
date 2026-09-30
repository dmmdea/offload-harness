package pipeline

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

func TestCeilingForArithmetic(t *testing.T) {
	if got := CeilingFor(300, seatrate.Estimate{}); got != core.AgentCeilingSecFloor {
		t.Fatalf("no estimate, small wall: floor, got %d", got)
	}
	if got := CeilingFor(900, seatrate.Estimate{TotalSec: 400}); got != 1800 {
		t.Fatalf("2x900=1800 vs 3x400=1200 vs floor 1800 -> 1800, got %d", got)
	}
	if got := CeilingFor(900, seatrate.Estimate{TotalSec: 1000}); got != 3000 {
		t.Fatalf("3x1000, got %d", got)
	}
	if got := CeilingFor(900, seatrate.Estimate{TotalSec: 10000}); got != core.AgentCeilingSecCap {
		t.Fatalf("capped, got %d", got)
	}
}

func TestLivenessPolicyForReadsTheSeat(t *testing.T) {
	cfg := config.Config{AgentSeatTokS: 7}
	p := LivenessPolicyFor(cfg, seatrate.Seat{}, 300*time.Second)
	if p.TokS != 7 || p.PrefillTokS != 0 || p.Admission != 300*time.Second || p.Floor != livenessFloor || p.Slack != livenessSlack {
		t.Fatalf("policy = %+v", p)
	}
	p = LivenessPolicyFor(cfg, seatrate.Seat{TokS: 3.4, PrefillTokS: 1800}, 0)
	if p.TokS != 3.4 || p.PrefillTokS != 1800 {
		t.Fatalf("measured rates must win: %+v", p)
	}
}

// The re-pack's allowance follows the seat's measured rate, through the policy
// the node actually builds (register C-66, RC-6): 685 tokens at the 2.7 tok/s a
// slow seat publishes needs ~250 s of generation, and a flat 120 s killed
// re-packs that were producing. A seat with no rate keeps the flat bound.
func TestLivenessPolicyForRepackFollowsSeatRate(t *testing.T) {
	cfg := config.Config{}
	slow := LivenessPolicyFor(cfg, seatrate.Seat{TokS: 2.7}, 300*time.Second)
	if got := slow.Allowance(agent.PhaseRepack, 685); got < 410*time.Second {
		t.Fatalf("685 tok at 2.7 tok/s allows %s, want >= 410 s", got)
	}
	unknown := LivenessPolicyFor(cfg, seatrate.Seat{}, 300*time.Second)
	if got := unknown.Allowance(agent.PhaseRepack, 685); got != agentRepackChatTimeout {
		t.Fatalf("a seat with no rate must keep the flat %s bound, got %s", agentRepackChatTimeout, got)
	}
	// The box's configured rate stands in for a seat with no measurement yet.
	configured := LivenessPolicyFor(config.Config{AgentSeatTokS: 2.7}, seatrate.Seat{}, 300*time.Second)
	if got := configured.Allowance(agent.PhaseRepack, 685); got < 410*time.Second {
		t.Fatalf("the configured rate must size the allowance too, got %s", got)
	}
}

// expectedRepackTokens is the EXPECTED size, never the cap: a wedged engine's
// flat bound stretches to the phase's allowance (ADR 0061), so an allowance
// sized to the completion cap would hold a dead seat for it.
func TestExpectedRepackTokensIsTheAnswerNotTheCap(t *testing.T) {
	short := "The answer is 42."
	if got := expectedRepackTokens(short); got != len(short)/3+64 || got >= repackBudget(short) {
		t.Fatalf("expected = %d for a short answer, want %d (budget %d)", got, len(short)/3+64, repackBudget(short))
	}
	long := string(make([]byte, 60000))
	if got, cap := expectedRepackTokens(long), repackBudget(long); got != cap {
		t.Fatalf("a huge answer is bounded by the completion cap: got %d, cap %d", got, cap)
	}
}
