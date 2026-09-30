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
	// ...including a decode rate slow enough that its bound is above the floor
	// (20 deltas at 0.1 tok/s = 200 s): the floor cannot hide a scaled term.
	slow := StallPolicy{Floor: 60 * time.Second, TokS: 0.1}
	if got := slow.AllowanceLoad(PhaseDecoding, 0, 5); got != 200*time.Second || got != slow.Allowance(PhaseDecoding, 0) {
		t.Fatalf("AllowanceLoad(decoding, 0, 5) on a 0.1 tok/s seat = %s, want the unscaled 200s", got)
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
// allowance) — and on an engine that cannot see a prefill (its counters stay
// flat while it prefills) that allowance is the load-scaled one: on a seat shared
// by four, a prefill's silence is four times as long before the engine's silence
// is a seat down.
func TestBusyHoldFlatBoundScalesWithTheEnginesLoad(t *testing.T) {
	// Frozen counters, 3 running + 1 waiting = load 4, on a source blind to prefill.
	eng := newDownEngine("ready")
	eng.running, eng.waiting, eng.blind = 3, 1, true
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

// An engine whose counters DO move through a prefill (vLLM's KV gauge,
// llama-server's decode counter) needs no stretched bound: its fingerprint stays
// flat only when it is hung, whatever is queued behind it. The load-scaled bound
// is for the engines that cannot see a prefill; giving every engine a bound that
// grows with the queue held a hung vLLM seat for minutes to hours.
func TestBusyHoldFlatBoundIgnoresTheLoadOnAnEngineThatSeesItsPrefill(t *testing.T) {
	eng := newDownEngine("ready") // 5 running, 40 waiting, frozen, and NOT blind
	eng.running, eng.waiting = 5, 40
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhasePrefill, 100) // solo 160 ms; a load-45 stretch would be 100/(1000/45)*1.5+10ms = 3.385 s
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a frozen engine that sees its prefill was never declared down")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Allowed != 160*time.Millisecond {
		t.Fatalf("allowed = %v, want the solo 160ms: 45 queued requests do not stretch the bound of an engine that sees its prefill", se)
	}
}

// A hung engine's HTTP front end keeps accepting requests, so its waiting count
// grows for as long as it is hung — and the engine's gauges are never work (the
// fingerprint excludes them, internal/seatload/activity.go). The flat bound of a
// prefill-blind engine is sized with the load the engine had when it LAST DID
// WORK: a bound that followed the latest reading receded by more than the
// silence lengthened, and a frozen engine with a growing queue was never
// declared down.
func TestBusyHoldFrozenEngineWithAGrowingQueueIsStillDeclaredDown(t *testing.T) {
	eng := newDownEngine("ready") // 5 running; one more waiting on every read
	eng.blind, eng.growWait = true, true
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhasePrefill, 100) // solo 160 ms; at the first look (5 running + 1 waiting = load 6): 910 ms
	select {
	case <-ctx.Done():
	case <-time.After(6 * time.Second):
		t.Fatalf("a frozen engine whose waiting count grows was never declared down (%d reads)", eng.readCount())
	}
	var sd *SeatDownError
	if !errors.As(m.Cause(), &sd) || sd.Kind != SeatDownWedged {
		t.Fatalf("cause = %#v, want a wedged seat-down", m.Cause())
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Allowed != 910*time.Millisecond {
		t.Fatalf("allowed = %v, want the 910ms sized with the load at the first look, not the queue's growth since", se)
	}
	if peak := m.PeakLoad(); peak <= 6 {
		t.Fatalf("PeakLoad = %d: the growing queue must still make the run not solo", peak)
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

// A run that nothing has answered about is not solo: PeakLoad 0 is "never looked",
// which the rates store must not read as the seat to itself. Phase used to raise
// the peak to 1 for every phase, so a run with no sampler (an unopenable registry)
// looked solo at the store boundary and its shared-seat sample moved the rate.
func TestMonitorPeakLoadIsZeroUntilSomethingAnswers(t *testing.T) {
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhasePrefill, 10)
	m.Phase(PhaseDecoding, 0)
	m.Phase(PhaseRepack, 0)
	m.SampleLoad()
	if got := m.PeakLoad(); got != 0 {
		t.Fatalf("PeakLoad = %d with no sampler and no engine reading, want 0 (never looked)", got)
	}
	// A sampler that answers 1 makes it a known-solo run; only a prefill or a
	// re-pack asks it.
	_, s := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer s.Stop()
	s.WithLoad(func() int { return 0 }) // "0 or less reads as 1": the sampler answered, the seat is the run's own
	s.Phase(PhaseDecoding, 0)
	if got := s.PeakLoad(); got != 0 {
		t.Fatalf("PeakLoad = %d after a phase that never asks the sampler, want 0", got)
	}
	s.Phase(PhasePrefill, 10)
	if got := s.PeakLoad(); got != 1 {
		t.Fatalf("PeakLoad = %d after the sampler answered, want 1 (a known solo run)", got)
	}
}

// waitPeak polls PeakLoad until it reaches want or a second passes: the engine
// read behind SampleLoad is asynchronous.
func waitPeak(t *testing.T, m *Monitor, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.PeakLoad() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PeakLoad = %d after 1s, want %d", m.PeakLoad(), want)
}

// The registry knows only this box's runs. A peer it cannot see (a cascade call, a
// run of another process) still shows in the engine's own running + waiting, which
// SampleLoad reads at the first delta when the registry saw nobody else.
func TestSampleLoadSeesAPeerOnlyTheEngineCanSee(t *testing.T) {
	var reads atomic.Int64
	probe := func(context.Context) (EngineReading, error) {
		reads.Add(1)
		return EngineReading{Fingerprint: "v|1", Summary: "vllm-metrics: 2 running, 1 waiting", Running: 2, Waiting: 1}, nil
	}
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithLoad(func() int { return 1 }) // the registry: nobody else on this box
	m.WithEngineProbe(probe)
	m.Phase(PhasePrefill, 10)
	if m.PeakLoad() != 1 {
		t.Fatalf("PeakLoad = %d after the registry sample, want 1", m.PeakLoad())
	}
	m.SampleLoad()
	waitPeak(t, m, 3)
	if got := m.PeakLoad(); got != 3 {
		t.Fatalf("PeakLoad = %d, want the engine's 2 running + 1 waiting", got)
	}
	// The registry already showing a peer makes the engine read redundant.
	before := reads.Load()
	_, shared := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer shared.Stop()
	shared.WithLoad(func() int { return 4 })
	shared.WithEngineProbe(probe)
	shared.Phase(PhasePrefill, 10)
	shared.SampleLoad()
	time.Sleep(50 * time.Millisecond)
	if reads.Load() != before {
		t.Fatalf("the engine was read %d time(s) for a run the registry already showed sharing the seat", reads.Load()-before)
	}
}

// With no registry at all the engine is the only witness: one running request is
// the run itself, so a known-solo run needs no sampler.
func TestSampleLoadEngineAloneMakesTheRunKnownSolo(t *testing.T) {
	probe := func(context.Context) (EngineReading, error) {
		return EngineReading{Fingerprint: "v|1", Summary: "vllm-metrics: 1 running, 0 waiting", Running: 1}, nil
	}
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhasePrefill, 10)
	m.SampleLoad()
	waitPeak(t, m, 1)
	if got := m.PeakLoad(); got != 1 {
		t.Fatalf("PeakLoad = %d, want 1 (the engine saw only this run)", got)
	}
}

// An engine that cannot be read, or that reports no gauges, is no observation.
func TestSampleLoadIgnoresAnEngineThatCannotBeRead(t *testing.T) {
	var reads atomic.Int64
	for name, probe := range map[string]EngineProbe{
		"unreadable": func(context.Context) (EngineReading, error) {
			reads.Add(1)
			return EngineReading{Running: 4, Waiting: 2}, errors.New("metrics: timeout")
		},
		"no gauges": func(context.Context) (EngineReading, error) {
			reads.Add(1)
			return EngineReading{Loading: true, State: "starting"}, nil
		},
	} {
		before := reads.Load()
		_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
		m.WithEngineProbe(probe)
		m.Phase(PhasePrefill, 10)
		m.SampleLoad()
		deadline := time.Now().Add(time.Second)
		for reads.Load() == before && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if got := m.PeakLoad(); got != 0 {
			t.Errorf("%s: PeakLoad = %d, want 0 (nothing was learned)", name, got)
		}
		m.Stop()
	}
}

// SampleLoad runs on the stream reader: an engine that answers only between
// batches must not hold it, and reads must not pile up behind a slow one.
func TestSampleLoadEngineReadIsAsyncAndSingle(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64
	probe := func(ctx context.Context) (EngineReading, error) {
		started.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return EngineReading{Fingerprint: "v|1", Running: 2}, nil
	}
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhasePrefill, 10)
	begin := time.Now()
	for i := 0; i < 5; i++ {
		m.SampleLoad()
	}
	if el := time.Since(begin); el > 200*time.Millisecond {
		t.Fatalf("SampleLoad blocked its caller for %s while the engine read was pending", el)
	}
	time.Sleep(50 * time.Millisecond)
	if n := started.Load(); n != 1 {
		t.Fatalf("%d engine reads started for 5 samples, want exactly one in flight", n)
	}
	close(release)
	waitPeak(t, m, 2)
}

// SettleLoad is the run's last look at its own witness: it returns as soon as the
// read lands, at once when none is in flight, and after its bound when the engine
// never answers — a finished run is never held for a hung engine.
func TestSettleLoadWaitsForTheReadBoundedByItsMax(t *testing.T) {
	_, none := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer none.Stop()
	begin := time.Now()
	none.SettleLoad(2 * time.Second)
	if el := time.Since(begin); el > 100*time.Millisecond {
		t.Fatalf("SettleLoad with no read in flight took %s, want an immediate return", el)
	}

	release := make(chan struct{})
	probe := func(ctx context.Context) (EngineReading, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return EngineReading{Fingerprint: "v|1", Running: 2}, nil
	}
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhasePrefill, 10)
	m.SampleLoad()
	begin = time.Now()
	m.SettleLoad(150 * time.Millisecond) // the engine is hung: the bound ends the wait
	if el := time.Since(begin); el < 120*time.Millisecond || el > 1500*time.Millisecond {
		t.Fatalf("SettleLoad on a hung engine took %s, want about its 150ms bound", el)
	}
	go func() { time.Sleep(60 * time.Millisecond); close(release) }()
	begin = time.Now()
	m.SettleLoad(5 * time.Second) // the read lands: the wait ends with it
	if el := time.Since(begin); el > 2*time.Second {
		t.Fatalf("SettleLoad took %s after the read landed", el)
	}
	if got := m.PeakLoad(); got != 2 {
		t.Fatalf("PeakLoad = %d after the settled read, want the engine's 2", got)
	}
}

// The in-flight flag clears when a read ends, or a single failed read would leave
// the run blind to its peers for good: once the engine has answered, a later
// first delta reads it again for as long as the run is not known to be shared.
func TestSampleLoadReadsAgainAfterTheEngineAnswered(t *testing.T) {
	var reads atomic.Int64
	probe := func(context.Context) (EngineReading, error) {
		if reads.Add(1) == 1 {
			return EngineReading{}, errors.New("metrics: timeout") // the first read learns nothing
		}
		return EngineReading{Fingerprint: "v|1", Running: 3}, nil
	}
	_, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhasePrefill, 10)
	m.SampleLoad()
	deadline := time.Now().Add(time.Second)
	for reads.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	m.SampleLoad() // the next call's first delta
	waitPeak(t, m, 3)
	if got := m.PeakLoad(); got != 3 || reads.Load() != 2 {
		t.Fatalf("PeakLoad = %d after %d read(s), want 3 after the second: a finished read must not block the next", got, reads.Load())
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
