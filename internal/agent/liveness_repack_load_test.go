package agent

import (
	"context"
	"testing"
	"time"
)

// The two decisions that size a re-pack meet here (register C-66): the allowance is
// the flat bound raised to the expected answer at the seat's decode rate, and the
// seat may be shared (ADR 0066). The flat bound stretches by the load; the
// generation estimate does not, because it is counted at the seat's own decode rate
// like the decoding allowance, and scaling it would hold a dead seat for `load`
// times what one request needs to write the expected answer.
func TestRepackAllowanceComposesTheExpectedAnswerWithTheLoad(t *testing.T) {
	// 685 expected tokens at 2.7 tok/s: 685 / 2.7 x 1.5 + 30 s = ~410.6 s, over the
	// flat 120 s (the answer that stalled 119 of 119 killed re-packs at a flat bound).
	slow := StallPolicy{Floor: 60 * time.Second, TokS: 2.7, Slack: 30 * time.Second, Repack: 120 * time.Second}
	tokens, rate := 685.0, 2.7
	estimate := time.Duration(tokens/rate*1.5*float64(time.Second)) + 30*time.Second
	if got := slow.Allowance(PhaseRepack, 685); got != estimate {
		t.Fatalf("solo allowance = %s, want the estimate %s", got, estimate)
	}
	for _, tc := range []struct {
		load int
		want time.Duration
	}{
		{1, estimate},
		{3, estimate},              // 3 x 120 s = 360 s is under the estimate: the estimate governs, unscaled
		{5, 5 * 120 * time.Second}, // 600 s is over it: the stretched flat bound governs
		{-2, estimate},             // nonsense reads as solo
	} {
		if got := slow.AllowanceLoad(PhaseRepack, 685, tc.load); got != tc.want {
			t.Errorf("AllowanceLoad(repack, 685, %d) on a 2.7 tok/s seat = %s, want %s", tc.load, got, tc.want)
		}
	}
	// A fast seat: the estimate is far under the flat bound, which alone stretches.
	fast := StallPolicy{Floor: 60 * time.Second, TokS: 200, Slack: 30 * time.Second, Repack: 120 * time.Second}
	for load, want := range map[int]time.Duration{1: 120 * time.Second, 3: 360 * time.Second} {
		if got := fast.AllowanceLoad(PhaseRepack, 685, load); got != want {
			t.Errorf("AllowanceLoad(repack, 685, %d) on a 200 tok/s seat = %s, want %s", load, got, want)
		}
	}
	// No measured rate, or no expected size: the flat bound alone, stretched.
	noRate := StallPolicy{Floor: 60 * time.Second, Slack: 30 * time.Second, Repack: 120 * time.Second}
	if got := noRate.AllowanceLoad(PhaseRepack, 685, 4); got != 480*time.Second {
		t.Errorf("no measured rate, load 4 = %s, want the flat 480s", got)
	}
	if got := slow.AllowanceLoad(PhaseRepack, 0, 4); got != 480*time.Second {
		t.Errorf("unknown size, load 4 = %s, want the flat 480s", got)
	}
}

// The stall reason names what sized the allowance: the arithmetic when the rate and
// the size are known (with the stretched flat bound beside it on a shared seat), the
// flat bound and why otherwise, the load in every shared case.
func TestRepackNoteNamesTheArithmeticAndTheLoad(t *testing.T) {
	note := func(pol StallPolicy, pending, load int) string {
		_, m := NewMonitor(context.Background(), pol, time.Hour)
		defer m.Stop()
		m.WithLoad(func() int { return load })
		m.Phase(PhaseRepack, pending)
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.note()
	}
	known := StallPolicy{Floor: time.Minute, TokS: 2.7, Slack: 30 * time.Second, Repack: 120 * time.Second}
	noRate := StallPolicy{Floor: time.Minute, Slack: 30 * time.Second, Repack: 120 * time.Second}
	for _, tc := range []struct {
		name string
		pol  StallPolicy
		pend int
		load int
		want string
	}{
		{"solo, rate and size known", known, 685, 1, ": 685 expected tok / 2.7 tok/s x 1.5 + 30s"},
		{"shared, rate and size known", known, 685, 3, ": 685 expected tok / 2.7 tok/s x 1.5 + 30s; flat re-pack bound 120s x load 3"},
		{"solo, no measured rate", noRate, 685, 1, ": flat re-pack bound, no measured decode rate for this seat"},
		{"solo, size unknown", known, 0, 1, ": flat re-pack bound, expected size unknown"},
		{"shared, no measured rate", noRate, 685, 4, ": re-pack bound 120s x load 4"},
		{"shared, size unknown", known, 0, 4, ": re-pack bound 120s x load 4"},
	} {
		if got := note(tc.pol, tc.pend, tc.load); got != tc.want {
			t.Errorf("%s: note = %q, want %q", tc.name, got, tc.want)
		}
	}
}
