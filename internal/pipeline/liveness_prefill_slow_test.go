package pipeline

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// The prefill clock is sized from the seat's SLOW recent rate, not its average (register F10).
// A 4,613-token prefill on a heat-soaked seat sat silent 59 s of a 60 s allowance while the
// seat was working: the policy held the seat's EMA, a cool-seat average. The policy the node
// builds now takes the slowest of the seat's recent solo samples.
func TestLivenessPolicyForSizesThePrefillClockFromTheSlowRecentRate(t *testing.T) {
	cfg := config.Config{}
	avg := LivenessPolicyFor(cfg, seatrate.Seat{TokS: 20, PrefillTokS: 200}, 0)
	soaked := LivenessPolicyFor(cfg, seatrate.Seat{TokS: 20, PrefillTokS: 200, PrefillSlowTokS: 78}, 0)
	if soaked.PrefillTokS != 78 || avg.PrefillTokS != 200 {
		t.Fatalf("policy prefill rates: soaked %.0f (want the window's 78), average-only %.0f (want the EMA 200)", soaked.PrefillTokS, avg.PrefillTokS)
	}
	const incident = 4613 // tokens the engine had to prefill
	cool := avg.Allowance(agent.PhasePrefill, incident)
	slow := soaked.Allowance(agent.PhasePrefill, incident)
	// At 78 tok/s that prompt needs 59 s: the allowance must clear it by the margin and the slack.
	if slow < 100*time.Second {
		t.Fatalf("allowance on the slow rate = %s, want >= 100 s for a prompt that takes ~59 s", slow)
	}
	if slow <= cool {
		t.Fatalf("the slow window must widen the allowance: slow %s, average %s", slow, cool)
	}
	// A wedged seat still stalls: the allowance is bounded by prompt/rate, not unlimited.
	if slow > 5*time.Minute {
		t.Fatalf("allowance %s is no longer a bound", slow)
	}
}
