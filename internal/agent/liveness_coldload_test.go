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
	return p
}

// While the seat reads loading the prefill clock does not run; once it reads
// ready the prefill allowance starts afresh.
func TestMonitorColdLoadHoldSuspendsThePrefillClock(t *testing.T) {
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
	seat.loading.Store(false)
	time.Sleep(25 * time.Millisecond) // one or two probe ticks
	if m.CurrentPhase() != PhasePrefill {
		t.Fatalf("a ready seat must resume the prefill clock, phase = %s", m.CurrentPhase())
	}
	_, _, last, allow := m.Snapshot()
	if allow != 40*time.Millisecond || time.Since(last) > 30*time.Millisecond {
		t.Fatalf("the prefill clock must restart fresh: allow=%s since=%s", allow, time.Since(last))
	}
	// The prefill clock runs again: a ready, silent seat stalls in prefill.
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
	if len(seen) != 2 || seen[0] != PhaseColdLoad || seen[1] != PhasePrefill {
		t.Fatalf("hook must hear exactly the two transitions, got %v", seen)
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
