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
// makes every read show one more engine step, `produce` one more produced
// token (default: advancing produces); `loading` reports a seat load;
// `fail` makes the engine unreadable; `failNext` fails that many reads.
type scriptedEngine struct {
	advance   atomic.Bool
	noProduce atomic.Bool
	loading   atomic.Bool
	fail      atomic.Bool
	failNext  atomic.Int64
	steps     atomic.Int64
	toks      atomic.Int64
	reads     atomic.Int64
}

func (e *scriptedEngine) probe(context.Context) (EngineReading, error) {
	e.reads.Add(1)
	if e.fail.Load() {
		return EngineReading{}, errors.New("metrics: connection refused")
	}
	if e.failNext.Load() > 0 {
		e.failNext.Add(-1)
		return EngineReading{}, errors.New("metrics: context deadline exceeded")
	}
	if e.loading.Load() {
		return EngineReading{Loading: true, State: "starting"}, nil
	}
	if e.advance.Load() {
		e.steps.Add(1)
		if !e.noProduce.Load() {
			e.toks.Add(1)
		}
	}
	return EngineReading{
		Fingerprint:      "v|" + strconv.FormatInt(e.steps.Load(), 10),
		TokenFingerprint: "vt|" + strconv.FormatInt(e.toks.Load(), 10),
		Summary:          "vllm-metrics: 4 running, 0 waiting",
	}, nil
}

// busyPolicy: decoding allowance 200 ms (20 deltas at 100 tok/s), engine read
// every 20 ms, flat bound 150 ms (under the decoding allowance, so the phase's
// own allowance, 200 ms, is what applies while decoding).
func busyPolicy() StallPolicy {
	p := msPolicy()
	p.EngineFlat, p.EnginePoll, p.EngineProbeTimeout = 150*time.Millisecond, 20*time.Millisecond, time.Second
	return p
}

// hookLog records the phases and allowances the monitor publishes.
type hookLog struct {
	mu     sync.Mutex
	phases []Phase
	allows []time.Duration
}

func (h *hookLog) hook(ph Phase, allow time.Duration) {
	h.mu.Lock()
	h.phases = append(h.phases, ph)
	h.allows = append(h.allows, allow)
	h.mu.Unlock()
}

func (h *hookLog) snapshot() ([]Phase, []time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Phase(nil), h.phases...), append([]time.Duration(nil), h.allows...)
}

// A request silent for 5x its decoding allowance while its seat's engine keeps
// working for others is WAITING ITS TURN — never a stall (the 2026-09-29 class:
// four decode "stalls" in one second on a seat that was producing). Its first
// delta ends the hold and restores the decoding allowance.
func TestBusyHoldKeepsARequestWhoseEngineWorksForOthers(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	h := &hookLog{}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, h.hook)
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
	phases, _ := h.snapshot()
	if len(phases) < 2 {
		t.Fatalf("hold events = %v: the hold must re-publish while the engine works (the delegator's rolling allowance)", phases)
	}
	for _, p := range phases {
		if p != PhaseQueued {
			t.Fatalf("hold events = %v, want only %s", phases, PhaseQueued)
		}
	}
}

// The hold publishes a ROLLING allowance — the flat bound plus one poll —
// re-stamped on every reading that shows the engine working, and nothing on a
// reading that shows it flat: the delegator then keeps polling exactly as long
// as the node would hold, and gives a node that died mid-hold up within one
// flat bound instead of at the run's ceiling.
func TestBusyHoldPublishesARollingAllowanceOnlyWhileTheEngineWorks(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	h := &hookLog{}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, h.hook)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(400 * time.Millisecond)
	_, allows := h.snapshot()
	if len(allows) < 3 {
		t.Fatalf("published %d allowances in a 200 ms hold at a 20 ms poll, want one per moving read", len(allows))
	}
	for _, a := range allows {
		if a != 220*time.Millisecond {
			t.Fatalf("published allowances = %v, want the flat bound (200 ms) + one poll (20 ms), never the ceiling", allows)
		}
	}
	eng.advance.Store(false) // the engine goes flat: no more refreshes
	before := len(allows)
	time.Sleep(100 * time.Millisecond)
	_, allows = h.snapshot()
	if len(allows) > before+1 {
		t.Fatalf("a flat engine kept re-publishing (%d -> %d): a reading that shows nothing changed is not progress", before, len(allows))
	}
	<-ctx.Done() // and the flat bound ends it
}

// An engine that does NO work for anyone while the request waits is a wedged
// seat: that is the stall, and its reason names the engine's silence and the
// phase the request waited in. The flat tail is not contention.
func TestBusyHoldFilesAStallWhenTheEngineDoesNoWork(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(450 * time.Millisecond) // ~250 ms held with the engine working
	eng.advance.Store(false)
	flatAt := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a wedged engine was never declared stalled")
	}
	if el := time.Since(flatAt); el < 150*time.Millisecond {
		t.Fatalf("stalled %s after the engine went flat, before the 200 ms flat bound", el)
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.EngineThrash || se.Unreadable || se.Waited != PhaseDecoding {
		t.Fatalf("cause = %#v, want an engine-flat stall that waited in decoding", m.Cause())
	}
	if !strings.Contains(se.Error(), "the seat's engine did no work") || !strings.Contains(se.Error(), "4 running") {
		t.Fatalf("reason = %q", se.Error())
	}
	if q := m.QueuedTotal(); q > 400*time.Millisecond {
		t.Fatalf("QueuedTotal = %s: the flat tail after the engine stopped is not contention", q)
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
// long prefill on an engine that shows nothing mid-prefill keeps the prefill
// allowance it always had.
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

// At the FIRST look an unreadable engine is "cannot tell": the pre-0.143.0
// rule decides, and the reason says the engine could not be read — never that
// it did no work.
func TestBusyHoldWithAnUnreadableEngineAtTheFirstLookKeepsTheOldRule(t *testing.T) {
	eng := &scriptedEngine{}
	eng.fail.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	start := time.Now()
	<-ctx.Done()
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Fatalf("an unreadable engine at the first look must not hold the run: stalled after %s", el)
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.EngineFlat || !se.Unreadable || se.Phase != PhaseDecoding {
		t.Fatalf("cause = %#v", m.Cause())
	}
	if !strings.Contains(se.Error(), "the engine could not be read") || !strings.Contains(se.Error(), "connection refused") {
		t.Fatalf("reason = %q", se.Error())
	}
}

// INSIDE a hold one unreadable read is no new evidence: a llama-server answers
// /slots and /metrics only between batches, so the busier the engine the
// likelier a read times out. The hold goes on.
func TestBusyHoldSurvivesAnUnreadableReadInsideTheHold(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(400 * time.Millisecond) // established hold
	eng.failNext.Store(3)              // three reads time out (60 ms < the 200 ms flat bound)
	time.Sleep(400 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("an established hold was killed by unreadable reads inside the flat bound: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want the hold to go on", m.CurrentPhase())
	}
}

// ...but an engine that stays unreadable past the flat bound ends the hold,
// filed as unreadable, never as "the engine did no work".
func TestBusyHoldFilesAPersistentlyUnreadableEngineAsUnreadable(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(400 * time.Millisecond)
	eng.fail.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("an engine unreadable past the flat bound never ended the hold")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.Unreadable || se.EngineFlat || se.Phase != PhaseDecoding {
		t.Fatalf("cause = %#v, want an unreadable stall in decoding", m.Cause())
	}
	if strings.Contains(se.Error(), "did no work") || !strings.Contains(se.Error(), "could not be read") || !strings.Contains(se.Error(), "held") {
		t.Fatalf("reason = %q", se.Error())
	}
}

// A preempt-and-recompute thrash steps the engine (its work fingerprint moves)
// but produces nothing: past the token bound that is a stall, named as such.
func TestBusyHoldStallsAThrashThatProducesNoToken(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	eng.noProduce.Store(true)
	p := busyPolicy()
	p.EngineTokenFlat = 300 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a thrash that produced nothing was held to the ceiling")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineThrash || se.Allowed != 300*time.Millisecond {
		t.Fatalf("cause = %#v, want a thrash stall at the 300 ms token bound", m.Cause())
	}
	if !strings.Contains(se.Error(), "produced no token") {
		t.Fatalf("reason = %q", se.Error())
	}
}

// A seat that starts LOADING under a waiting request (evicted by a swap) is
// the cold-load hold's — and its ceiling starts when the load is seen, not at
// the request's last progress: a busy hold longer than the cold-load ceiling
// must not leave the load zero budget.
func TestBusyHoldHandsALoadAFullColdLoadCeiling(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	p := busyPolicy()
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = 400*time.Millisecond, "test ceiling", 10*time.Millisecond, 250*time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(700 * time.Millisecond) // held for longer than the 400 ms cold-load ceiling
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want queued first", m.CurrentPhase())
	}
	eng.loading.Store(true)
	time.Sleep(200 * time.Millisecond) // half the ceiling, counted from the load
	if ctx.Err() != nil || m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("a load seen after a long hold must get its own ceiling: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
}

// A post-ready hold that runs out reads the engine before it stalls: after a
// swap-in the held runs re-issue together, and the last in line waits while the
// engine prefills its siblings.
func TestPostReadyHoldReadsTheEngineBeforeStalling(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	seat := &scriptedSeat{} // ready
	p := busyPolicy()
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = 2*time.Second, "test ceiling", 10*time.Millisecond, 100*time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(seat.probe, nil)
	m.MarkSeatLoaded()
	m.Phase(PhasePrefill, 10)
	time.Sleep(600 * time.Millisecond) // well past the post-ready bound
	if ctx.Err() != nil {
		t.Fatalf("a post-ready request whose engine works for its siblings was stalled: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want the busy hold after the post-ready bound", m.CurrentPhase())
	}
	m.Progress(1)
	if m.CurrentPhase() != PhaseDecoding {
		t.Fatalf("the first delta must end in decoding (the request waited in prefill), phase = %s", m.CurrentPhase())
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
