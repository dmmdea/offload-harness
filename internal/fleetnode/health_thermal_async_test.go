package fleetnode

// F11, fix round: the optional thermal query never sits on the memory reading's path.
//
// The probe used to run the thermal query inline: a second nvidia-smi that hung held the memory probe for up to its 10 s
// bound, the sampler stamped the snapshot AFTER the probe returned (so the memory figures were older than their timestamp
// said), the next tick was pushed back, and fleet-serve's first, synchronous sample delayed the node's start. The thermal
// refresh now runs on its own goroutine, one at a time, and the probe only reads what the last one stored.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// smiDeviceProbeWith builds the production probe over an injected thermal reader, clock and log, and nothing else: its
// thermal answer arrives on a later tick, as it does in production.
func smiDeviceProbeWith(run func() (string, error), thermal thermalReader, now func() time.Time, logf func(string, ...any)) DeviceProbe {
	p, _ := newSmiProbe(run, thermal, now, logf)
	return p
}

// settledProbe is the production probe for a test about WHAT is published and WHEN the query runs, not about the
// ordering of the two: each call is a tick, then the wait for the thermal query that tick started, then the next tick at
// the same instant, which reads the answer the first asked for. The second tick starts no query (the first set the
// next one 30 s or 60 s out), so the reader's call count is the production tick's.
func settledProbe(run func() (string, error), thermal thermalReader, now func() time.Time, logf func(string, ...any)) DeviceProbe {
	p, c := newSmiProbe(run, thermal, now, logf)
	return func() ([]GPUDevice, error) {
		if _, err := p(); err != nil {
			return nil, err
		}
		if c != nil {
			c.wg.Wait()
		}
		return p()
	}
}

// blockingThermal is a thermal reader that holds until released, like a wedged second nvidia-smi.
type blockingThermal struct {
	mu      sync.Mutex
	calls   int
	rows    []gpuprobe.Thermal
	started chan struct{} // closed by the first call
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func newBlockingThermal(rows []gpuprobe.Thermal) *blockingThermal {
	return &blockingThermal{rows: rows, started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingThermal) read(ctx context.Context) ([]gpuprobe.Thermal, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.rows, nil
}

func (b *blockingThermal) unblock() { b.relOnce.Do(func() { close(b.release) }) }

func (b *blockingThermal) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// within runs f and fails the test when it has not returned after d. A probe that waits for the held reader is the failure
// this catches, so the held reader is released on the way out and nothing is left running.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: still running after %s", what, d)
	}
}

func TestSmiDeviceProbeDoesNotWaitForTheThermalQuery(t *testing.T) {
	b := newBlockingThermal(hotCard0())
	t.Cleanup(b.unblock)
	p := smiDeviceProbeWith(smiRun(smiTwoCards), b.read, newThermalClock().now, (&logLines{}).logf)
	var devs []GPUDevice
	within(t, 3*time.Second, "a probe while the thermal query is held", func() { devs, _ = p() })
	if len(devs) != 2 || devs[0].Name != "NVIDIA GeForce RTX 5060 Ti" || devs[0].TotalGiB <= 0 {
		t.Fatalf("devices = %+v, want the whole memory reading while the thermal query is still out", devs)
	}
	if devs[0].TemperatureC != nil {
		t.Fatalf("device 0 carries a temperature before any thermal answer exists: %+v", devs[0])
	}
}

// The node's first sample is synchronous (runSamplerLoop), so a thermal query that hangs at startup used to hold the
// node's start for its whole bound.
func TestTheSamplerStartsWhileTheThermalQueryIsHeld(t *testing.T) {
	b := newBlockingThermal(hotCard0())
	t.Cleanup(b.unblock)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := smiDeviceProbeWith(smiRun(smiTwoCards), b.read, newThermalClock().now, (&logLines{}).logf)
	var smp *Sampler
	within(t, 3*time.Second, "the sampler's first sample while the thermal query is held", func() {
		smp = StartDeviceProbeSampler(ctx, time.Hour, p, "")
	})
	snap, ok := smp.Load()
	if !ok || len(snap.Devices) != 2 {
		t.Fatalf("snapshot = %+v (ok %v), want the memory reading of both cards the moment the sampler returns", snap, ok)
	}
}

// One refresh at a time: ticks that fall due while a query is still out start no second one.
func TestSmiDeviceProbeStartsOneThermalQueryAtATime(t *testing.T) {
	clk := newThermalClock()
	b := newBlockingThermal(hotCard0())
	t.Cleanup(b.unblock)
	p, cache := newSmiProbe(smiRun(smiTwoCards), b.read, clk.now, (&logLines{}).logf)
	within(t, 3*time.Second, "probes while the thermal query is held", func() {
		for i := 0; i < 6; i++ { // 6 ticks 31 s apart: each is past the 30 s window
			if _, err := p(); err != nil {
				t.Errorf("probe: %v", err)
			}
			clk.advance(31 * time.Second)
		}
	})
	// Every query a tick started is counted by the cache's wait group, so releasing the held one and waiting for all of
	// them makes the count final.
	b.unblock()
	cache.wg.Wait()
	if got := b.count(); got != 1 {
		t.Fatalf("%d thermal queries were started while the first was still out, want 1", got)
	}
}

// The answer lands on a later tick, once the query returns.
func TestSmiDeviceProbePublishesTheThermalAnswerOnALaterTick(t *testing.T) {
	b := newBlockingThermal(hotCard0())
	t.Cleanup(b.unblock)
	p := smiDeviceProbeWith(smiRun(smiTwoCards), b.read, newThermalClock().now, (&logLines{}).logf)
	within(t, 3*time.Second, "the first probe", func() { _, _ = p() })
	<-b.started
	b.unblock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var devs []GPUDevice
		within(t, 3*time.Second, "a later probe", func() { devs, _ = p() })
		if devs[0].TemperatureC != nil {
			if *devs[0].TemperatureC != 83 {
				t.Fatalf("device 0 = %+v, want the 83 C the query returned", devs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the thermal answer never reached a later tick")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// An answer is aged from when its query STARTED: its data was taken then, and counting from the moment the query returned
// would publish a stale "cool" for as long as the query ran past the 90 s the field promises.
func TestAThermalAnswerIsAgedFromWhenItsQueryStarted(t *testing.T) {
	clk := newThermalClock()
	var calls int
	var mu sync.Mutex
	read := func(ctx context.Context) ([]gpuprobe.Thermal, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			clk.advance(50 * time.Second) // the first query takes 50 s of the node's time
			return hotCard0(), nil
		}
		return nil, errors.New("nvidia-smi: exit status 9")
	}
	p, cache := newSmiProbe(smiRun(smiTwoCards), read, clk.now, (&logLines{}).logf)
	if _, err := p(); err != nil { // t = 0: starts the query, which returns at t = 50
		t.Fatal(err)
	}
	cache.wg.Wait()
	tick := func(label string, want bool) {
		t.Helper()
		devs, err := p()
		if err != nil {
			t.Fatal(err)
		}
		cache.wg.Wait()
		if got := devs[0].TemperatureC != nil; got != want {
			t.Fatalf("%s: device 0 carries a temperature = %v, want %v", label, got, want)
		}
	}
	tick("t=50, the answer's data is 50 s old", true)
	clk.advance(41 * time.Second)
	tick("t=91, the answer's data is 91 s old although it returned 41 s ago", false)
}
