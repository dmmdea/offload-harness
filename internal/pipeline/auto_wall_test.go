package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// TestAutoWallForClampsTheSeatRateEstimate (register D-03): the auto wall IS
// the published estimate, held to the wire bounds — a slow seat's long contract
// stops at the cap, a fast seat's short one never runs under the default, and
// a seat with no rate yet keeps the default (0 = "do not stamp").
func TestAutoWallForClampsTheSeatRateEstimate(t *testing.T) {
	cfg := config.Config{AgentMaxTokens: 1024, AgentThinking: "off"}
	twelve := core.AgentContract{Goal: "g", MaxSteps: 12}

	// Inside the bounds: the wall equals the estimate, to the second.
	mid := seatrate.Seat{TokS: 10, Samples: 3}
	est := wallEstimateFor(cfg, twelve, "seat", mid, 0, core.AgentTimeoutSecDefault)
	if est.TotalSec <= core.AgentTimeoutSecDefault || est.TotalSec >= core.AgentTimeoutSecCap {
		t.Fatalf("fixture drifted: estimate %d s is not strictly inside %d..%d", est.TotalSec, core.AgentTimeoutSecDefault, core.AgentTimeoutSecCap)
	}
	if got, note := autoWallFor(cfg, twelve, "seat", mid); got != est.TotalSec || !strings.Contains(note, "auto wall for seat") {
		t.Fatalf("mid-rate: wall=%d note=%q, want %d and an auto-wall note", got, note, est.TotalSec)
	}
	// A 1 tok/s seat wants far more than the cap: the cap.
	if got, _ := autoWallFor(cfg, twelve, "seat", seatrate.Seat{TokS: 1, Samples: 3}); got != core.AgentTimeoutSecCap {
		t.Fatalf("slow seat: wall=%d, want the cap %d", got, core.AgentTimeoutSecCap)
	}
	// A 10,000 tok/s seat on a one-step contract wants seconds: the default.
	if got, _ := autoWallFor(cfg, core.AgentContract{Goal: "g", MaxSteps: 1}, "seat", seatrate.Seat{TokS: 10000, Samples: 3}); got != core.AgentTimeoutSecDefault {
		t.Fatalf("fast seat: wall=%d, want the default %d", got, core.AgentTimeoutSecDefault)
	}
	// No rate yet: nothing to stamp, and the note says why.
	if got, note := autoWallFor(cfg, twelve, "seat", seatrate.Seat{}); got != 0 || !strings.Contains(note, "no rate yet") {
		t.Fatalf("no rate: wall=%d note=%q, want 0 and a no-rate note", got, note)
	}
}
