package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// msPolicy: allowances in tens of milliseconds. Windows timers tick at ~15 ms,
// so nothing here is tighter than 40 ms.
func msPolicy() StallPolicy {
	return StallPolicy{Admission: 80 * time.Millisecond, PrefillTokS: 1000, TokS: 100,
		ToolTimeout: 40 * time.Millisecond, Repack: 120 * time.Millisecond, Floor: 40 * time.Millisecond, Slack: 10 * time.Millisecond}
}

func TestAllowanceArithmetic(t *testing.T) {
	p := StallPolicy{Floor: 60 * time.Second, PrefillTokS: 2000, TokS: 4, Slack: 30 * time.Second}
	want := time.Duration(214442.0/2000*1.5*float64(time.Second)) + 30*time.Second
	if got := p.Allowance(PhasePrefill, 214442); got != want {
		t.Fatalf("prefill allowance = %s, want %s", got, want)
	}
	if got := p.Allowance(PhasePrefill, 100); got != 60*time.Second {
		t.Fatalf("small prompt must floor at 60s, got %s", got)
	}
	if got := p.Allowance(PhaseDecoding, 0); got != 60*time.Second { // 20 deltas at 4 tok/s = 5 s < floor
		t.Fatalf("decoding allowance = %s", got)
	}
	slow := StallPolicy{Floor: 60 * time.Second, TokS: 0.1}
	if got := slow.Allowance(PhaseDecoding, 0); got != 200*time.Second {
		t.Fatalf("a 0.1 tok/s seat gets 200s between deltas, got %s", got)
	}
	unknown := StallPolicy{Floor: 60 * time.Second, Slack: 30 * time.Second}
	if got := unknown.Allowance(PhasePrefill, 100*100); got != 150*time.Second+30*time.Second {
		t.Fatalf("unknown prefill rate must assume %.0f tok/s, got %s", assumedPrefillTokS, got)
	}
	if got := unknown.Allowance(PhaseTool, 0); got != 60*time.Second {
		t.Fatalf("tool with no timeout floors, got %s", got)
	}
	if got := (StallPolicy{Floor: 60 * time.Second, Slack: 30 * time.Second, ToolTimeout: 5 * time.Minute}).Allowance(PhaseTool, 0); got != 5*time.Minute+30*time.Second {
		t.Fatalf("tool allowance = %s", got)
	}
	if got := unknown.toolAllowance(-1); got != time.Hour {
		t.Fatalf("an uncapped tool gets an hour, got %s", got)
	}
}

// The re-pack allowance follows the answer it re-writes and the seat's rate
// (register C-66, RC-6). It used to be a flat 120 s whatever the size: a 685-token
// answer on a seat publishing 2.7 tok/s needs ~250 s of generation, and on a seat
// that answers JSON in one piece the monitor hears nothing until it lands, so
// 58 of the 83 re-packs on one seat on 2026-09-29 were filed as stalled while the
// engine was producing.
func TestAllowanceArithmeticRepackFollowsTheAnswerAndTheSeatRate(t *testing.T) {
	p := StallPolicy{Floor: 60 * time.Second, Repack: 120 * time.Second, Slack: 30 * time.Second, TokS: 2.7}
	tokens, rate := 685.0, 2.7
	want := time.Duration(tokens/rate*1.5*float64(time.Second)) + 30*time.Second // 410.6 s
	if got := p.Allowance(PhaseRepack, 685); got != want || got < 410*time.Second {
		t.Fatalf("685 tok at 2.7 tok/s allows %s, want %s (>= 410 s)", got, want)
	}
	// The rate unknown, or the size unknown: the flat bound, as before.
	noRate := StallPolicy{Floor: 60 * time.Second, Repack: 120 * time.Second, Slack: 30 * time.Second}
	if got := noRate.Allowance(PhaseRepack, 685); got != 120*time.Second {
		t.Fatalf("TokS 0 must keep the 120 s floor, got %s", got)
	}
	if got := p.Allowance(PhaseRepack, 0); got != 120*time.Second {
		t.Fatalf("an unknown answer size must keep the 120 s floor, got %s", got)
	}
	// A fast seat never gets less than the flat bound or the floor.
	fast := StallPolicy{Floor: 60 * time.Second, Repack: 120 * time.Second, Slack: 30 * time.Second, TokS: 100}
	if got := fast.Allowance(PhaseRepack, 685); got != 120*time.Second {
		t.Fatalf("a fast seat keeps the flat bound, got %s", got)
	}
	// Only a HIGHER floor than the flat bound wins over an arithmetic that is smaller.
	strict := StallPolicy{Floor: 300 * time.Second, Repack: 120 * time.Second, Slack: 30 * time.Second, TokS: 100}
	if got := strict.Allowance(PhaseRepack, 685); got != 300*time.Second {
		t.Fatalf("the floor still bounds the allowance from below, got %s", got)
	}
}

// A stalled re-pack says how its allowance was sized, so a reader can check the
// arithmetic the way the prefill note lets them check that one.
func TestMonitorRepackStallNamesItsArithmetic(t *testing.T) {
	// 800 expected tok / 10,000 tok/s x 1.5 + 10 ms = 130 ms: over the 40 ms flat
	// bound, so the arithmetic is what sized this allowance.
	pol := StallPolicy{Floor: 40 * time.Millisecond, Repack: 40 * time.Millisecond, Slack: 10 * time.Millisecond, TokS: 10000}
	ctx, m := NewMonitor(context.Background(), pol, 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseRepack, 800)
	if _, _, _, allow := m.Snapshot(); allow != 130*time.Millisecond {
		t.Fatalf("allowance = %s, want 130ms", allow)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("silent re-pack was not stalled")
	}
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) || se.Phase != PhaseRepack {
		t.Fatalf("cause = %v, want a re-pack stall", context.Cause(ctx))
	}
	if !strings.Contains(se.Error(), "800 expected tok / 10000.0 tok/s x 1.5") {
		t.Fatalf("stall reason %q does not show the re-pack arithmetic", se.Error())
	}
}

func TestMonitorToolPhaseUsesTheToolsOwnCap(t *testing.T) {
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.ToolPhase(3 * time.Second)
	_, _, _, allow := m.Snapshot()
	if allow != 3*time.Second+10*time.Millisecond || m.CurrentPhase() != PhaseTool {
		t.Fatalf("allow=%s phase=%s", allow, m.CurrentPhase())
	}
	m.ToolPhase(0) // the policy's ToolTimeout (40ms) + slack (10ms)
	_, _, _, allow = m.Snapshot()
	if allow != 50*time.Millisecond {
		t.Fatalf("default tool allowance = %s", allow)
	}
}

func TestMonitorStallsWhenSilent(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0) // allowance = 20 deltas / 100 tok/s = 200ms (over the 40ms floor)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("silent run was not stalled")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseDecoding || se.Allowed != 200*time.Millisecond {
		t.Fatalf("cause = %v", m.Cause())
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > 5*time.Second || time.Until(dl) < 4*time.Second {
		t.Fatalf("Deadline() must report the ceiling: %v %v", dl, ok)
	}
	if !errors.As(context.Cause(ctx), &se) {
		t.Fatal("context.Cause must carry the StallError")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err = %v", ctx.Err())
	}
}

func TestMonitorPrefillNoteAndAssumedRate(t *testing.T) {
	pol := msPolicy()
	pol.PrefillTokS = 0
	pol.Slack = 30 * time.Second
	_, m := NewMonitor(context.Background(), pol, 5*time.Second)
	defer m.Stop()
	m.Phase(PhasePrefill, 10)
	m.mu.Lock()
	note := m.note()
	m.mu.Unlock()
	if note != ": 10 tok / 100 tok/s assumed x 1.5 + 30s" {
		t.Fatalf("note = %q", note)
	}
}

func TestMonitorProgressResetsTheStall(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)
	deadline := time.Now().Add(1200 * time.Millisecond) // 6x the 200ms allowance, kept alive by ticks
	for i := 1; time.Now().Before(deadline); i++ {
		m.Progress(i)
		time.Sleep(5 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("a producing run was cancelled: %v", context.Cause(ctx))
	}
	tok, tokS, last, allow := m.Snapshot()
	if tok == 0 || tokS <= 0 || time.Since(last) > 100*time.Millisecond || allow != 200*time.Millisecond {
		t.Fatalf("snapshot tok=%d tokS=%.1f last=%s allow=%s", tok, tokS, time.Since(last), allow)
	}
}

func TestMonitorFirstDeltaEndsPrefill(t *testing.T) {
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhasePrefill, 5000)
	if m.CurrentPhase() != PhasePrefill {
		t.Fatal("phase not set")
	}
	m.Progress(1)
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("first delta must end the prefill, phase = %s", m.CurrentPhase())
	}
	m.Phase(PhaseTool, 0)
	tok, _, _, _ := m.Snapshot()
	if tok != 1 {
		t.Fatalf("call tokens must fold into the run total on a phase change, got %d", tok)
	}
}

func TestMonitorCeilingFiresWhileProducing(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 120*time.Millisecond)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)
	for i := 1; ; i++ {
		m.Progress(i)
		if ctx.Err() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
		if i > 400 {
			t.Fatal("ceiling never fired")
		}
	}
	var ce *CeilingError
	if !errors.As(m.Cause(), &ce) || ce.Tokens == 0 || ce.Elapsed < 100*time.Millisecond {
		t.Fatalf("cause = %v", m.Cause())
	}
	if !errors.As(context.Cause(ctx), &ce) {
		t.Fatal("context.Cause must carry the CeilingError")
	}
}

func TestMonitorParentCancelIsNotAStall(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, m := NewMonitor(parent, msPolicy(), 5*time.Second)
	defer m.Stop()
	cancel()
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // let the deadline goroutine observe the parent
	if m.Cause() != nil {
		t.Fatalf("parent cancel must not be reported as stall/ceiling, got %v", m.Cause())
	}
}

func TestMonitorPhaseChangeCountsAsProgress(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0) // allowance 40ms
	time.Sleep(25 * time.Millisecond)
	m.Phase(PhaseRepack, 0) // allowance 120ms, timer reset
	time.Sleep(60 * time.Millisecond) // 85ms since the first phase, 60ms since the change
	if ctx.Err() != nil {
		t.Fatalf("a phase change must reset the stall timer: %v", context.Cause(ctx))
	}
}

func TestMonitorStopEndsTheContextWithoutACause(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	m.Stop()
	<-ctx.Done()
	if m.Cause() != nil {
		t.Fatalf("Stop must not file a cause, got %v", m.Cause())
	}
	m.Stop() // idempotent
	m.Progress(3)
	m.Phase(PhaseTool, 0) // no panic after Stop
}
