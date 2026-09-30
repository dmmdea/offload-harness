package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Pins for the busy hold's state-machine seams (ADR 0061). Each one fails
// against the mutant named in its comment; the seven monitor tests in
// liveness_busyhold_test.go leave all of them green.

// The hold is entered, left on progress, left again through the loop's own
// phase change, and re-entered in the SAME run. The contention figure is the
// sum of the holds and a wedged engine on the second one is stalled from ITS
// baseline. Mutants: Phase()/ToolPhase() forgetting the held time
// (endBusyLocked), QueuedTotal ignoring a hold still in progress.
func TestBusyHoldIsReenteredInOneRunAndTheTotalIsTheSumOfBothHolds(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	h := &hookLog{}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, h.hook)
	m.Phase(PhaseDecoding, 0)
	m.Progress(5)
	time.Sleep(500 * time.Millisecond) // hold #1 (~300 ms after the 200 ms decoding allowance)
	m.Progress(9)                      // its turn came
	q1 := m.QueuedTotal()
	if q1 < 200*time.Millisecond {
		t.Fatalf("hold #1 total = %s", q1)
	}
	m.ToolPhase(40 * time.Millisecond) // the loop moves on...
	m.Phase(PhasePrefill, 10)          // ...and the next request is silent too (allowance floors at 40 ms)
	time.Sleep(400 * time.Millisecond) // hold #2
	if ctx.Err() != nil || m.CurrentPhase() != PhaseQueued {
		t.Fatalf("second hold: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
	if q2 := m.QueuedTotal(); q2 < q1+250*time.Millisecond {
		t.Fatalf("QueuedTotal in hold #2 = %s, want hold #1 (%s) + ~350 ms", q2, q1)
	}
	phases, _ := h.snapshot()
	if len(phases) < 2 {
		t.Fatalf("the hold was announced %d times across two holds", len(phases))
	}
	eng.advance.Store(false) // the engine wedges during hold #2
	<-ctx.Done()
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.Waited != PhasePrefill {
		t.Fatalf("cause = %#v, want an engine-flat stall that waited in prefill", m.Cause())
	}
	if se.EngineSilent > 400*time.Millisecond {
		t.Fatalf("EngineSilent = %s: measured from hold #1's baseline, not hold #2's", se.EngineSilent)
	}
}

// The loop moving on while held (the request failed and was re-issued) ends
// the hold and keeps what it cost; the total does not keep growing outside a
// hold. Mutant: endBusyLocked dropping the held time.
func TestBusyHoldEndedByAPhaseChangeKeepsItsTotal(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	_, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(500 * time.Millisecond)
	m.Phase(PhasePrefill, 10)
	q := m.QueuedTotal()
	if m.CurrentPhase() == PhaseQueued || q < 250*time.Millisecond {
		t.Fatalf("phase=%s QueuedTotal=%s", m.CurrentPhase(), q)
	}
	time.Sleep(60 * time.Millisecond)
	if m.CurrentPhase() != PhaseQueued { // it may have re-entered a hold: only assert the total when it did not
		if q2 := m.QueuedTotal(); q2 > q+20*time.Millisecond {
			t.Fatalf("QueuedTotal grew outside a hold: %s -> %s", q, q2)
		}
	}
}

// QueuedTotal counts a hold still in progress: finish() reads it on the stall
// and ceiling paths, where the run ends inside the hold. Mutant: dropping the
// live term.
func TestBusyHoldQueuedTotalCountsAHoldInProgress(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	_, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(600 * time.Millisecond)
	if q := m.QueuedTotal(); q < 300*time.Millisecond {
		t.Fatalf("QueuedTotal mid-hold = %s, want ~400 ms", q)
	}
}

// Leaving the hold restores the waited phase's own allowance: no engine read
// inside it. Mutant: leaveQueuedLocked not restoring m.allow, which leaves the
// 10 s poll as the request's allowance and re-reads the engine within it.
func TestBusyHoldLeavingItRestoresThePhasesAllowance(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0) // allowance 200 ms
	time.Sleep(450 * time.Millisecond)
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s", m.CurrentPhase())
	}
	m.Progress(4)
	if _, _, _, allow := m.Snapshot(); allow != 200*time.Millisecond {
		t.Fatalf("allowance after leaving the hold = %s, want the decoding allowance (200 ms), not the 20 ms poll", allow)
	}
	reads := eng.reads.Load()
	time.Sleep(120 * time.Millisecond) // > the 20 ms poll, < the 200 ms allowance
	if eng.reads.Load() != reads || m.CurrentPhase() != PhaseDecoding || ctx.Err() != nil {
		t.Fatalf("the engine was re-read inside the restored allowance: reads %d->%d phase=%s", reads, eng.reads.Load(), m.CurrentPhase())
	}
}

// Progress arriving while an engine read is in flight wins: the read's answer
// is about a request state that no longer exists. Mutant: dropping the
// `!m.last.Equal(last)` guard in checkEngineLocked.
func TestBusyHoldIgnoresAnEngineReadThatReturnsAfterProgress(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 8)
	probe := func(context.Context) (EngineReading, error) {
		started <- struct{}{}
		<-gate
		return EngineReading{Fingerprint: "v|1", TokenFingerprint: "vt|1", Summary: "flat"}, nil
	}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhaseDecoding, 0) // allowance 200 ms
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never started")
	}
	m.Progress(3) // the request moved while the read was out
	close(gate)
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("a run that produced during the read was stalled: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("phase = %s, want decoding", m.CurrentPhase())
	}
}

// PRODUCTION ROUTING. LivenessPolicyFor always arms a cold-load ceiling, so a
// silent prefill, re-pack or admission takes onStall's awaiting-a-byte branch
// (seat probe first, then the engine) — never the `engineArmed &&
// engineCheckablePhase` branch every other monitor test exercises with
// ColdLoad 0. Mutant: deleting `case m.engineArmedLocked()` from that branch
// leaves every monitor test green and disables the hold for prefill and
// re-pack in production.
func TestBusyHoldCoversPrefillAndRepackWhenAColdLoadCeilingIsArmed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase Phase
	}{{"prefill", PhasePrefill}, {"repack", PhaseRepack}} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &scriptedEngine{}
			eng.advance.Store(true)
			p := busyPolicy()
			p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = 2*time.Second, "test ceiling", 10*time.Millisecond, 250*time.Millisecond
			ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
			defer m.Stop()
			m.WithEngineProbe(eng.probe)
			m.WithSeatProbe((&scriptedSeat{}).probe, nil) // llama-swap reads the seat ready
			m.Phase(tc.phase, 10)
			time.Sleep(700 * time.Millisecond) // well past the phase's allowance
			if ctx.Err() != nil || m.CurrentPhase() != PhaseQueued {
				t.Fatalf("%s silence on a working engine: phase=%s cause=%v", tc.name, m.CurrentPhase(), context.Cause(ctx))
			}
			eng.advance.Store(false)
			<-ctx.Done()
			var se *StallError
			if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.Waited != tc.phase {
				t.Fatalf("cause = %#v, want an engine-flat stall that waited in %s", m.Cause(), tc.phase)
			}
		})
	}
}

// A hung engine read is bounded by EngineProbeTimeout (never under 1 s) and
// read as "cannot tell": at the first look that is the pre-0.143.0 rule.
func TestBusyHoldHungEngineReadIsBoundedAndReadAsUnreadable(t *testing.T) {
	probe := func(ctx context.Context) (EngineReading, error) { <-ctx.Done(); return EngineReading{}, ctx.Err() }
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhaseDecoding, 0)
	select {
	case <-ctx.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("a hung engine read held the monitor's decision")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.EngineFlat || !se.Unreadable {
		t.Fatalf("cause = %#v", m.Cause())
	}
}
