package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stall timer is a time.AfterFunc, and Timer.Reset cannot recall a callback
// that has already been launched: the callback runs, finds the Monitor's lock
// held by the method that is moving the run on (and re-arming the timer), and
// takes the lock the moment that method lets go. It then judges the NEW phase
// against the NEW last-progress time, so it files a stall with no silence in it:
// "no progress for 0s in tool (allowed 120s; 1 tok so far)".
//
// These tests build exactly that order without a hook in the product. The test
// holds the Monitor's lock, queues the method that moves the run on (it parks on
// the lock first), waits until the stall timer has EXPIRED (Timer.Stop reports
// false only once it has), lets the launched callback park behind the method, and
// lets go. The lock is a FIFO queue of parked waiters, so the method runs first
// and the stale callback second, as it does when the timer expires inside the
// method's critical section.

// staleStallFire runs move() ahead of a stall-timer callback that expired
// before it, and returns once the callback has had time to run.
func staleStallFire(t *testing.T, m *Monitor, move func()) {
	t.Helper()
	m.mu.Lock()
	moved := make(chan struct{})
	go func() {
		defer close(moved)
		move() // parks on m.mu first
	}()
	time.Sleep(50 * time.Millisecond)
	// Timer.Stop is true while the timer is still pending (it is stopped; arm it
	// again, shortly) and false once it has expired: the callback is launched.
	deadline := time.Now().Add(10 * time.Second)
	for m.timer.Stop() {
		if time.Now().After(deadline) {
			m.mu.Unlock()
			t.Fatal("the stall timer never expired")
		}
		m.timer.Reset(5 * time.Millisecond)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the launched callback parks behind move()
	m.mu.Unlock()
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("the move never ran")
	}
	time.Sleep(100 * time.Millisecond) // the stale callback takes the lock after it
}

// staleCallbackPolicy: the allowance of every phase is `floor`, a tool's its cap.
// staleStallFire expires the timer itself, so a test that must not meet a real
// stall while it runs takes a floor longer than the run.
func staleCallbackPolicy(floor time.Duration) StallPolicy {
	return StallPolicy{Floor: floor, TokS: 1000}
}

// The timer armed for decoding expires while the run moves into a tool. The tool
// has just started (a tool call is progress), so nothing is overdue: no stall.
func TestStallTimerExpiredBeforeAPhaseChangeFilesNoStallInTheNewPhase(t *testing.T) {
	_, m := NewMonitor(context.Background(), staleCallbackPolicy(80*time.Millisecond), time.Hour)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)

	staleStallFire(t, m, func() { m.ToolPhase(time.Minute) })

	if err := m.Cause(); err != nil {
		t.Fatalf("a stall timer armed for decoding expired as the run moved into a tool and filed %q", err)
	}
	if got := m.CurrentPhase(); got != PhaseTool {
		t.Fatalf("phase = %s, want tool", got)
	}
}

// The same with a byte, not a phase change: the timer armed for the silence
// before the byte expires while the byte is being booked.
func TestStallTimerExpiredBeforeProgressFilesNoStall(t *testing.T) {
	_, m := NewMonitor(context.Background(), staleCallbackPolicy(10*time.Second), time.Hour)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)

	staleStallFire(t, m, func() { m.Progress(1) })

	if err := m.Cause(); err != nil {
		t.Fatalf("a stall timer expired as a token was being booked and filed %q", err)
	}
}

// A callback that is ignored as stale must not cost the run its stall: the timer
// the move re-armed is the one that files it, and the silence it names is real.
func TestIgnoredStaleStallCallbackLeavesTheRealTimerArmed(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), staleCallbackPolicy(80*time.Millisecond), time.Hour)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)

	staleStallFire(t, m, func() { m.ToolPhase(time.Second) })

	if err := m.Cause(); err != nil {
		t.Fatalf("the stale callback filed %q before the tool's second had passed", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the silent tool was never stalled: the stale callback cost the run its timer")
	}
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) {
		t.Fatalf("cause = %v, want a *StallError", context.Cause(ctx))
	}
	if se.Phase != PhaseTool || se.Silent < se.Allowed {
		t.Fatalf("stall = %q (phase %s, silent %s, allowed %s), want the tool's stall after its full %s", se, se.Phase, se.Silent, se.Allowed, se.Allowed)
	}
}

// Control: real silence still stalls, and a stall names a silence at least as
// long as the bound it was filed against.
func TestRealSilenceStillStallsAfterItsAllowance(t *testing.T) {
	for name, enter := range map[string]func(*Monitor){
		"decoding": func(m *Monitor) { m.Phase(PhaseDecoding, 0) },
		"tool":     func(m *Monitor) { m.ToolPhase(100 * time.Millisecond) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, m := NewMonitor(context.Background(), staleCallbackPolicy(80*time.Millisecond), time.Hour)
			defer m.Stop()
			enter(m)
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("a silent run was never stalled")
			}
			var se *StallError
			if !errors.As(context.Cause(ctx), &se) {
				t.Fatalf("cause = %v, want a *StallError", context.Cause(ctx))
			}
			if se.Silent < se.Allowed {
				t.Fatalf("stall %q names %s of silence against an allowance of %s", se, se.Silent, se.Allowed)
			}
		})
	}
}
