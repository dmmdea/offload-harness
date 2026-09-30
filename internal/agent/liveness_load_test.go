package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The allowance a prefill or a re-pack starts under is sized for the load the
// run sees (ADR 0066, register C-66): the measured prefill rate is one request's,
// and a seat shared by `load` requests gives each about 1/load of it. Allowance
// itself — a seat the run has to itself — is unchanged.
func TestAllowanceLoadScalesPrefillAndRepack(t *testing.T) {
	p := StallPolicy{Floor: 60 * time.Second, PrefillTokS: 400, TokS: 4, Slack: 30 * time.Second, Repack: 120 * time.Second}
	solo := p.Allowance(PhasePrefill, 24000) // 24000/400*1.5 + 30 = 120 s
	if solo != 120*time.Second {
		t.Fatalf("solo prefill allowance = %s, want 120s (the arithmetic this test scales)", solo)
	}
	// Four requests share the seat: each prefills at 100 tok/s: 24000/100*1.5 + 30.
	want := time.Duration(24000.0/(400.0/4)*1.5*float64(time.Second)) + 30*time.Second
	if got := p.AllowanceLoad(PhasePrefill, 24000, 4); got != want || want != 390*time.Second {
		t.Fatalf("AllowanceLoad(prefill, 24000, 4) = %s, want the 4x arithmetic %s (390s)", got, want)
	}
	if got := p.Allowance(PhasePrefill, 24000); got != solo {
		t.Fatalf("Allowance changed to %s: it is the solo arithmetic and must not move", got)
	}
	// The re-pack's flat bound stretches by the load; the floor still holds.
	if got := p.AllowanceLoad(PhaseRepack, 0, 3); got != 360*time.Second {
		t.Fatalf("AllowanceLoad(repack, 0, 3) = %s, want 3 x 120s", got)
	}
	if got := p.Allowance(PhaseRepack, 0); got != 120*time.Second {
		t.Fatalf("Allowance(repack) = %s, want the flat 120s", got)
	}
	// A tiny prompt on a loaded seat still floors at the definition of a silent seat.
	if got := p.AllowanceLoad(PhasePrefill, 10, 4); got != 60*time.Second {
		t.Fatalf("AllowanceLoad(prefill, 10, 4) = %s, want the 60s floor", got)
	}
}

// load 1 (and any nonsense below it) is Allowance for every phase — no phase's
// arithmetic moves for a run that has the seat to itself.
func TestAllowanceLoadOfOneIsAllowanceForEveryPhase(t *testing.T) {
	p := StallPolicy{Admission: 90 * time.Second, Floor: 60 * time.Second, PrefillTokS: 300, TokS: 4, Slack: 30 * time.Second,
		Repack: 120 * time.Second, ToolTimeout: 5 * time.Minute, ColdLoad: 10 * time.Minute, EnginePoll: 10 * time.Second}
	for _, ph := range []Phase{PhaseAdmission, PhasePrefill, PhaseDecoding, PhaseTool, PhaseRepack, PhaseColdLoad, PhaseQueued, Phase("nonsense")} {
		for _, load := range []int{-3, 0, 1} {
			if got, want := p.AllowanceLoad(ph, 9000, load), p.Allowance(ph, 9000); got != want {
				t.Fatalf("AllowanceLoad(%s, 9000, %d) = %s, want Allowance %s", ph, load, got, want)
			}
		}
	}
	// Decoding, tool and cold-load are not load-scaled: their bounds are not a
	// single-request prefill rate.
	for _, ph := range []Phase{PhaseDecoding, PhaseTool, PhaseColdLoad, PhaseAdmission} {
		if got, want := p.AllowanceLoad(ph, 9000, 5), p.Allowance(ph, 9000); got != want {
			t.Fatalf("AllowanceLoad(%s, 9000, 5) = %s, want the unscaled %s", ph, got, want)
		}
	}
}

// The monitor sizes a prefill's allowance with the load the sampler reports when
// the phase begins, and the reason it would file prints that load beside the
// arithmetic — so a reader can check "24000 tok / (400 tok/s / load 4)".
func TestNotePrintsTheLoad(t *testing.T) {
	pol := StallPolicy{Floor: time.Minute, PrefillTokS: 400, TokS: 4, Slack: 30 * time.Second, Repack: 120 * time.Second}
	_, m := NewMonitor(context.Background(), pol, time.Hour)
	defer m.Stop()
	m.WithLoad(func() int { return 4 })
	m.Phase(PhasePrefill, 24000)
	if _, _, _, allow := m.Snapshot(); allow != 390*time.Second {
		t.Fatalf("prefill allowance under load 4 = %s, want 390s", allow)
	}
	m.mu.Lock()
	note := m.note()
	m.mu.Unlock()
	if note != ": 24000 tok / (400 tok/s / load 4) x 1.5 + 30s" {
		t.Fatalf("note = %q, want the load printed inside the arithmetic", note)
	}
	m.Phase(PhaseRepack, 0)
	m.mu.Lock()
	note = m.note()
	m.mu.Unlock()
	if note != ": re-pack bound 120s x load 4" {
		t.Fatalf("re-pack note = %q", note)
	}
	if _, _, _, allow := m.Snapshot(); allow != 480*time.Second {
		t.Fatalf("re-pack allowance under load 4 = %s, want 480s", allow)
	}
	// A run with the seat to itself prints exactly what it always did.
	_, solo := NewMonitor(context.Background(), pol, time.Hour)
	defer solo.Stop()
	solo.WithLoad(func() int { return 1 })
	solo.Phase(PhasePrefill, 24000)
	solo.mu.Lock()
	note = solo.note()
	solo.mu.Unlock()
	if note != ": 24000 tok / 400 tok/s x 1.5 + 30s" {
		t.Fatalf("solo note = %q, want the pre-0.144 arithmetic", note)
	}
}

// Only a prefill and a re-pack sample the load: the sampler is a registry read,
// and no other phase's allowance scales with it.
func TestMonitorSamplesTheLoadOnlyForPrefillAndRepack(t *testing.T) {
	var calls atomic.Int64
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithLoad(func() int { calls.Add(1); return 3 })
	m.Phase(PhaseDecoding, 0)
	m.ToolPhase(0)
	m.Phase(PhaseAdmission, 0)
	if n := calls.Load(); n != 0 {
		t.Fatalf("the load was sampled %d times for phases that do not scale with it", n)
	}
	m.Phase(PhasePrefill, 10)
	m.Phase(PhaseRepack, 0)
	if n := calls.Load(); n != 2 {
		t.Fatalf("the load was sampled %d times, want once for the prefill and once for the re-pack", n)
	}
}

// PeakLoad is the highest load the run ever saw, from the sampler and from the
// busy hold's engine readings; a run that never looked reads 0, a solo one 1.
func TestMonitorPeakLoadCountsTheSamplerAndTheEngine(t *testing.T) {
	_, never := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer never.Stop()
	if never.PeakLoad() != 0 {
		t.Fatalf("PeakLoad of a run that never looked = %d, want 0", never.PeakLoad())
	}

	_, solo := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer solo.Stop()
	solo.WithLoad(func() int { return 1 })
	solo.Phase(PhasePrefill, 10)
	if solo.PeakLoad() != 1 {
		t.Fatalf("PeakLoad of a solo run = %d, want 1", solo.PeakLoad())
	}

	var load atomic.Int64
	load.Store(1)
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithLoad(func() int { return int(load.Load()) })
	m.Phase(PhasePrefill, 10)
	load.Store(3)
	m.Phase(PhasePrefill, 10)
	load.Store(1)
	m.Phase(PhasePrefill, 10)
	if m.PeakLoad() != 3 {
		t.Fatalf("PeakLoad = %d, want the 3 seen at the second prefill (a later solo reading must not lower it)", m.PeakLoad())
	}

	// The engine's own gauges count too: five running, none waiting.
	eng := newDownEngine("ready")
	eng.set("ready", true)
	ctx, e := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer e.Stop()
	e.WithEngineProbe(eng.probe)
	e.Phase(PhaseDecoding, 0)
	time.Sleep(500 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("the run ended: %v", context.Cause(ctx))
	}
	if e.PeakLoad() != 5 {
		t.Fatalf("PeakLoad from the engine's readings = %d, want 5 (5 running, 0 waiting)", e.PeakLoad())
	}
}

// The busy hold's flat bound is max(the flat floor, the waiting phase's
// allowance) — and that allowance is the load-scaled one: on a seat shared by
// four, a prefill's silence is four times as long before the engine's silence is
// a seat down.
func TestBusyHoldFlatBoundScalesWithTheEnginesLoad(t *testing.T) {
	// Frozen counters, 3 running + 1 waiting = load 4.
	eng := newDownEngine("ready")
	eng.running, eng.waiting = 3, 1
	p := busyPolicy() // prefill 1000 tok/s, slack 10 ms, floor 40 ms; flat 150 ms
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhasePrefill, 100) // solo: 100/1000*1.5+10ms = 160 ms; at load 4: 100/250*1.5+10ms = 610 ms
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a frozen engine was never declared down")
	}
	var sd *SeatDownError
	if !errors.As(m.Cause(), &sd) {
		t.Fatalf("cause = %#v", m.Cause())
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Allowed != 610*time.Millisecond {
		t.Fatalf("allowed = %v, want the load-scaled 610ms (solo would be 160ms)", se)
	}
	// First look at the 160 ms solo allowance, then one 610 ms flat bound.
	if el := time.Since(start); el < 650*time.Millisecond {
		t.Fatalf("declared down after %s: the flat bound was not scaled by the load", el)
	}
	if !strings.Contains(sd.Error(), "3 running, 1 waiting") {
		t.Fatalf("reason = %q, want the engine's gauges (the load) in it", sd.Error())
	}
}

// The loop samples the load a second time at each call's FIRST delta — the end of
// the prefill window — so a peer that arrived while this request was prefilling
// is seen too. A touch (a counted busy-wait answer, progress 0) is no delta.
func TestLoopSamplesTheLoadAtTheFirstDelta(t *testing.T) {
	var calls atomic.Int64
	var touched atomic.Bool
	sampler := func() int { // solo when the prefill begins; three sharing once the touch is over, at the first delta
		if calls.Add(1) == 1 || !touched.Load() {
			return 1
		}
		return 3
	}
	c := &touchFirstClient{touched: &touched, progressFakeClient: progressFakeClient{ticks: 3, script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}}
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithLoad(sampler)
	if _, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the load was sampled %d times, want once when the prefill began and once at the first delta (not for the touch, not for later deltas)", n)
	}
	if m.PeakLoad() != 3 {
		t.Fatalf("PeakLoad = %d, want the 3 seen at the first delta", m.PeakLoad())
	}
}

// touchFirstClient sends a progress touch (0) before its deltas — what a counted
// busy-wait answer looks like to the monitor.
type touchFirstClient struct {
	progressFakeClient
	touched *atomic.Bool
}

func (f *touchFirstClient) Chat(ctx context.Context, msgs []Msg, tools []ToolSpec, maxTokens int) (Completion, error) {
	if fn := ProgressFromContext(ctx); fn != nil {
		fn(0)
		f.touched.Store(true) // the touch is over: everything the sampler is asked from here on is the first delta's
	}
	return f.progressFakeClient.Chat(ctx, msgs, tools, maxTokens)
}
