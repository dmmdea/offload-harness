package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedEngine is an EngineProbe whose engine the test drives: `advance`
// makes every read show one more engine step; `loading` reports a seat load;
// `fail` makes the engine unreadable.
type scriptedEngine struct {
	advance atomic.Bool
	loading atomic.Bool
	fail    atomic.Bool
	steps   atomic.Int64
	reads   atomic.Int64
}

func (e *scriptedEngine) probe(context.Context) (EngineReading, error) {
	e.reads.Add(1)
	if e.fail.Load() {
		return EngineReading{}, errors.New("metrics: connection refused")
	}
	if e.loading.Load() {
		return EngineReading{Loading: true, State: "starting"}, nil
	}
	if e.advance.Load() {
		e.steps.Add(1)
	}
	return EngineReading{Fingerprint: "v|" + strconv.FormatInt(e.steps.Load(), 10), Summary: "vllm-metrics: 4 running, 0 waiting"}, nil
}

// busyPolicy: decoding allowance 200 ms (20 deltas at 100 tok/s), engine read
// every 20 ms, flat bound 150 ms (under the decoding allowance, so the phase's
// own allowance is what applies while decoding).
func busyPolicy() StallPolicy {
	p := msPolicy()
	p.EngineFlat, p.EnginePoll = 150*time.Millisecond, 20*time.Millisecond
	return p
}

// A request silent for 5x its decoding allowance while its seat's engine keeps
// working for others is WAITING ITS TURN — never a stall (the 2026-09-29 class:
// four decode "stalls" in one second on a seat that was producing). Its first
// delta ends the hold and restores the decoding allowance.
func TestBusyHoldKeepsARequestWhoseEngineWorksForOthers(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	var mu sync.Mutex
	var seen []Phase
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, func(ph Phase, _ time.Duration) { mu.Lock(); seen = append(seen, ph); mu.Unlock() })
	m.Phase(PhaseDecoding, 0)
	m.Progress(5)
	time.Sleep(time.Second)
	if ctx.Err() != nil {
		t.Fatalf("a request whose engine is working was stalled: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want %s", m.CurrentPhase(), PhaseQueued)
	}
	if eng.reads.Load() < 5 {
		t.Fatalf("the engine was read %d times during a 1 s hold (poll 20 ms)", eng.reads.Load())
	}
	m.Progress(9) // its turn came
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("after its first delta the request must be decoding again, phase = %s", m.CurrentPhase())
	}
	if q := m.QueuedTotal(); q < 600*time.Millisecond {
		t.Fatalf("QueuedTotal = %s, want the ~800 ms held", q)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != PhaseQueued {
		t.Fatalf("observer events = %v, want exactly one hold entry (not one per poll)", seen)
	}
}

// An engine that does NO work for anyone while the request waits is a wedged
// seat: that is the stall, and its reason names the engine's silence and the
// phase the request waited in.
func TestBusyHoldFilesAStallWhenTheEngineDoesNoWork(t *testing.T) {
	eng := &scriptedEngine{} // never advances
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a wedged engine was never declared stalled")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.Waited != PhaseDecoding {
		t.Fatalf("cause = %#v, want an engine-flat stall that waited in decoding", m.Cause())
	}
	// Silence to the decoding allowance (200 ms), then the engine flat for
	// max(150 ms, the 200 ms decoding allowance).
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("stalled after %s, before the allowance + the flat bound", el)
	}
	if !strings.Contains(se.Error(), "the seat's engine did no work") || !strings.Contains(se.Error(), "4 running") {
		t.Fatalf("reason = %q", se.Error())
	}
}

// A SOLO prefill emits nothing until its first token; its engine shows the
// work (KV blocks allocated chunk by chunk). Held, then ended by the first
// delta, which moves the run to decoding.
func TestBusyHoldCoversASoloPrefillWhoseEngineWorks(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhasePrefill, 10) // allowance floors at 40 ms
	time.Sleep(500 * time.Millisecond)
	if ctx.Err() != nil || m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
	m.Progress(1)
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("a prefill's first delta must end in decoding, phase = %s", m.CurrentPhase())
	}
}

// The flat bound is never shorter than the waiting phase's own allowance: a
// long prefill on an engine that shows nothing mid-prefill (a /slots-only
// llama-server) keeps the prefill allowance it always had.
func TestBusyHoldFlatBoundIsAtLeastThePhasesOwnAllowance(t *testing.T) {
	eng := &scriptedEngine{} // flat
	p := busyPolicy()
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhasePrefill, 400)         // 400 / 1000 x 1.5 + 10 ms = 610 ms > the 150 ms flat bound
	time.Sleep(900 * time.Millisecond) // the allowance, then under the bound again
	if ctx.Err() != nil {
		t.Fatalf("stalled inside the prefill's own allowance: %v", context.Cause(ctx))
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a flat engine past the prefill allowance was never stalled")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.Waited != PhasePrefill || se.Allowed != 610*time.Millisecond {
		t.Fatalf("cause = %#v", m.Cause())
	}
}

// An unreadable engine is "cannot tell": the pre-0.143.0 rule decides, and the
// reason says the engine could not be read.
func TestBusyHoldWithAnUnreadableEngineKeepsTheOldRule(t *testing.T) {
	eng := &scriptedEngine{}
	eng.fail.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	start := time.Now()
	<-ctx.Done()
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Fatalf("an unreadable engine must not hold the run: stalled after %s", el)
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.EngineFlat || se.Phase != PhaseDecoding {
		t.Fatalf("cause = %#v", m.Cause())
	}
	if !strings.Contains(se.Error(), "the engine could not be read") || !strings.Contains(se.Error(), "connection refused") {
		t.Fatalf("reason = %q", se.Error())
	}
}

// A seat that starts LOADING under a waiting request (evicted by a swap) is
// the cold-load hold's, not the busy hold's.
func TestBusyHoldHandsALoadingSeatToTheColdLoadHold(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	p := busyPolicy()
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = 2*time.Second, "test ceiling", 10*time.Millisecond, 250*time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(300 * time.Millisecond) // held as queued
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want queued first", m.CurrentPhase())
	}
	eng.loading.Store(true)
	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil || m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("a loading seat must be held under the cold-load ceiling: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
}

// A tool call runs off the seat: its own cap governs even while the engine
// works for others.
func TestBusyHoldDoesNotExtendAToolCall(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.ToolPhase(40 * time.Millisecond)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a tool call that outlived its cap was held by a busy engine")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseTool || se.EngineFlat {
		t.Fatalf("cause = %#v", m.Cause())
	}
}

// EngineFlat 0 = no busy hold, even with a probe installed: the pre-0.143.0
// rule, byte for byte.
func TestBusyHoldIsOffWithoutAFlatBound(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	p := busyPolicy()
	p.EngineFlat = 0
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	<-ctx.Done()
	if eng.reads.Load() != 0 {
		t.Fatalf("the engine was read %d times with the hold off", eng.reads.Load())
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhaseDecoding || se.EngineFlat {
		t.Fatalf("cause = %#v", m.Cause())
	}
}

// What the hold PUBLISHES is the time left to the run's ceiling, not its
// 10 s poll: the delegator keeps polling a remote job only while now < last
// progress + published allowance + grace, so publishing the poll interval
// made it abandon a run ~70 s into a hold the node was right to keep.
func TestBusyHoldPublishesTheTimeLeftToTheCeiling(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	var mu sync.Mutex
	var published []time.Duration
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, func(ph Phase, allow time.Duration) {
		if ph == PhaseQueued {
			mu.Lock()
			published = append(published, allow)
			mu.Unlock()
		}
	})
	m.Phase(PhaseDecoding, 0)
	time.Sleep(400 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("stalled: %v", context.Cause(ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published allowances = %v, want exactly one (on entering the hold)", published)
	}
	if published[0] < 4*time.Second || published[0] > 5*time.Second {
		t.Fatalf("published allowance = %s, want the ~4.8 s left to the 5 s ceiling, never the 20 ms poll", published[0])
	}
}

// The ceiling still bounds a busy hold: an engine that works forever for
// others cannot keep one run alive past its ceiling.
func TestBusyHoldNeverOutlivesTheCeiling(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 700*time.Millisecond)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the ceiling did not end a run held by a busy engine")
	}
	var ce *CeilingError
	if !errors.As(m.Cause(), &ce) {
		t.Fatalf("cause = %#v, want the ceiling", m.Cause())
	}
}
