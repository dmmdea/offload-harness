package pipeline

import (
	"testing"
	"time"

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
