package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedSeat is a SeatProbe whose answer the test flips.
type scriptedSeat struct {
	loading atomic.Bool
	fail    atomic.Bool
	calls   atomic.Int64
}

func (s *scriptedSeat) probe(context.Context) (bool, string, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return false, "", errors.New("running unreadable")
	}
	if s.loading.Load() {
		return true, "starting", nil
	}
	return false, "ready", nil
}

func coldPolicy(ceiling time.Duration) StallPolicy {
	p := msPolicy() // prefill of a 10-token prompt floors at 40 ms
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll = ceiling, "test ceiling", 10*time.Millisecond
	p.PostReady = 250 * time.Millisecond
	return p
}

// While the seat reads loading the prefill clock does not run. Once it reads
// ready the first completion gets the short post-ready bound (250 ms here),
// not the rest of the cold-load ceiling; the first delta ends the hold.
func TestMonitorColdLoadHoldLastsUntilTheFirstByte(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	var mu sync.Mutex
	var seen []Phase
	ctx, m := NewMonitor(context.Background(), coldPolicy(2*time.Second), 5*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, func(ph Phase, _ time.Duration) { mu.Lock(); seen = append(seen, ph); mu.Unlock() })
	m.Phase(PhasePrefill, 10)
	time.Sleep(400 * time.Millisecond) // 10x the 40 ms prefill allowance
	if ctx.Err() != nil {
		t.Fatalf("a loading seat was stalled: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("phase = %s, want %s", m.CurrentPhase(), PhaseColdLoad)
	}
	seat.loading.Store(false)          // ready, but no byte yet
	time.Sleep(120 * time.Millisecond) // many ticks, 3x the prefill allowance, inside the post-ready bound
	if ctx.Err() != nil || m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("a freshly loaded seat's first completion must stay held: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
	m.Progress(1)
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("the first byte must end the hold, phase = %s", m.CurrentPhase())
	}
	// The next prefill has no load behind it: a ready, silent seat stalls.
	m.Phase(PhasePrefill, 10)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a ready silent seat was never stalled")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhasePrefill {
		t.Fatalf("cause = %v, want a prefill stall", m.Cause())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != PhaseColdLoad {
		t.Fatalf("hook must hear exactly the transition into the hold, got %v", seen)
	}
}

// MarkSeatLoaded (the admission warm-up loaded the seat): the first request
// is held even though /running reads ready, under the SHORT post-ready bound
// (250 ms here), never the 5 s cold-load ceiling, and the reason names
// post-ready silence (review finding, PR #458).
func TestMonitorMarkSeatLoadedHoldsTheFirstCompletion(t *testing.T) {
	seat := &scriptedSeat{} // reads ready throughout
	ctx, m := NewMonitor(context.Background(), coldPolicy(5*time.Second), 10*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.MarkSeatLoaded()
	m.Phase(PhasePrefill, 10)
	time.Sleep(150 * time.Millisecond) // past the 40 ms prefill allowance, inside the ceiling
	if ctx.Err() != nil {
		t.Fatalf("the first completion after a load was stalled: %v", context.Cause(ctx))
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the warm-up hold was not bounded")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseColdLoad || !strings.Contains(se.Error(), "after the seat read ready") ||
		se.Allowed != 250*time.Millisecond {
		t.Fatalf("cause = %v", m.Cause())
	}
}

// The load can also happen before the loop's first step: its own probes (the
// tokenizer) go through llama-swap in PhaseAdmission. Measured live with the
// admission warm-up off: "stalled: no progress for 60s in admission".
func TestMonitorColdLoadHoldCoversAdmission(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	ctx, m := NewMonitor(context.Background(), coldPolicy(2*time.Second), 5*time.Second) // admission allowance 80 ms
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	time.Sleep(300 * time.Millisecond)
	if ctx.Err() != nil || m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("a load during admission was not held: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
}

// The hold is bounded: a seat that never finishes loading is a cold-load
// stall at the ceiling, and the reason says so.
func TestMonitorColdLoadCeilingFiles(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	ctx, m := NewMonitor(context.Background(), coldPolicy(150*time.Millisecond), 5*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the cold-load ceiling never fired")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseColdLoad || se.Allowed != 150*time.Millisecond || se.Silent < 150*time.Millisecond {
		t.Fatalf("cause = %#v", m.Cause())
	}
	msg := se.Error()
	if !strings.HasPrefix(msg, "stalled: seat still loading after ") || !strings.Contains(msg, `"starting"`) || !strings.Contains(msg, "test ceiling") {
		t.Fatalf("reason = %q", msg)
	}
}

// A probe that cannot read the seat is "cannot tell": the prefill clock runs
// exactly as before probes existed.
func TestMonitorColdLoadUnreadableProbeKeepsTheStall(t *testing.T) {
	seat := &scriptedSeat{}
	seat.fail.Store(true)
	ctx, m := NewMonitor(context.Background(), coldPolicy(5*time.Second), 5*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an unreadable probe held the run")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhasePrefill {
		t.Fatalf("cause = %v", m.Cause())
	}
}

// The first delta during a hold ends it: the seat is serving.
func TestMonitorColdLoadFirstDeltaEndsTheHold(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	_, m := NewMonitor(context.Background(), coldPolicy(5*time.Second), 5*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	time.Sleep(60 * time.Millisecond)
	if m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("phase = %s", m.CurrentPhase())
	}
	m.Progress(1)
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("a delta must end the hold, phase = %s", m.CurrentPhase())
	}
}

// No ColdLoad in the policy = no hold at all, whatever the probe says: the
// pre-0.140.0 rule.
func TestMonitorWithoutColdLoadCeilingNeverHolds(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("no ceiling configured, yet the run was held")
	}
	if seat.calls.Load() != 0 {
		t.Fatalf("the probe ran %d times without a cold-load ceiling", seat.calls.Load())
	}
}

// Review finding 2 (PR #458): a probe that ERRORS mid-hold must not be
// reported as the seat reading ready. The reason names the unreadable
// /running, and the post-ready bound (not the cold-load ceiling) applies.
func TestMonitorColdLoadUnreadableMidHoldIsReportedHonestly(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	pol := coldPolicy(5 * time.Second)
	pol.PostReady = 150 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), pol, 10*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	time.Sleep(100 * time.Millisecond)
	if m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("phase = %s", m.CurrentPhase())
	}
	seat.fail.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("an unreadable /running mid-hold was held toward the cold-load ceiling")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) {
		t.Fatalf("cause = %v", m.Cause())
	}
	msg := se.Error()
	if strings.Contains(msg, `read "ready`) || !strings.Contains(msg, "unreadable") {
		t.Fatalf("the reason must name the unreadable /running, not a ready seat: %q", msg)
	}
}

// Review finding 3 (PR #458): the hold never outlives the run's own ceiling,
// so a load that outlasts the run is still filed as a cold-load stall
// (infrastructure), not as the run ceiling (budget).
func TestMonitorColdLoadHoldIsClampedToTheRunCeiling(t *testing.T) {
	seat := &scriptedSeat{}
	seat.loading.Store(true)
	ctx, m := NewMonitor(context.Background(), coldPolicy(5*time.Second), 400*time.Millisecond)
	defer m.Stop()
	m.WithSeatProbe(seat.probe, nil)
	m.Phase(PhasePrefill, 10)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("nothing ended the run")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseColdLoad {
		t.Fatalf("a load outlasting the run must file a cold-load stall, got %v", m.Cause())
	}
}
