package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// THE DEFECT (2026-09-14, register D-93): the drain read the engine's gauge
// alone. A run is a multi-step loop and the gauge reads zero between its
// steps, so the drain called the seat idle in that gap, unloaded it, and the
// run's next step died behind the fence. With a run registered on the seat the
// drain must keep waiting through the gap and return only when the run ends.
func TestDrainWaitsForARegisteredRunAcrossTheStepGap(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(0) // the gap between two steps: nothing on the engine
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()

	reg := gpuactivity.OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "agent_run", Origin: "node-a", MaxSteps: 12})
	if err != nil {
		t.Fatal(err)
	}
	h.OnStep(3, 2310)
	ended := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		h.End()
		close(ended)
	}()
	var out strings.Builder
	start := time.Now()
	p := drainProbe{client: srv.Client(), endpoint: srv.URL, model: "seat", runs: reg.OnSeat, every: 5 * time.Millisecond, out: &out, hint: "one seat turn is ~174 s"}
	if err := drainUntil(context.Background(), p, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	<-ended
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("drain returned while a run was still registered on the seat")
	}
	got := out.String()
	for _, want := range []string{"0 in flight; 1 run(s) registered", "agent_run pid " + strconv.Itoa(os.Getpid()), "step 3/12", "2310 tokens", "from node-a", "one seat turn is ~174 s"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress must name the run and the turn hint; missing %q in:\n%s", want, got)
		}
	}
	// Printed on CHANGE only: one state (0 in flight + this run at step 3)
	// held for the whole wait, so exactly one line went out.
	if n := strings.Count(got, "gpu reserve: draining"); n != 1 {
		t.Errorf("an unchanged state must print once, printed %d times:\n%s", n, got)
	}
}

// A run advancing a step is a state change worth one line; the same state is
// never re-printed inside the reminder window.
func TestDrainPrintsOnChangeNotOnEveryTick(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	reg := gpuactivity.OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(gpuactivity.Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	var out strings.Builder
	go func() {
		time.Sleep(60 * time.Millisecond)
		h.OnStep(1, 100) // step advanced: a new line
		time.Sleep(60 * time.Millisecond)
		h.OnStep(2, 900) // and another
	}()
	p := drainProbe{client: srv.Client(), endpoint: srv.URL, model: "seat", runs: reg.OnSeat, every: 2 * time.Millisecond, out: &out}
	err = drainUntil(context.Background(), p, time.Now().Add(220*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "1 in flight") || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("a busy seat must fail at the deadline naming what it saw and the queue budget: %v", err)
	}
	if n := strings.Count(out.String(), "gpu reserve: draining"); n != 3 {
		t.Fatalf("three states (step 0, 1, 2) must print three lines, got %d:\n%s", n, out.String())
	}
}

// A run registered on ANOTHER seat is not this seat's work.
func TestDrainIgnoresRunsOnOtherSeats(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	reg := gpuactivity.OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(gpuactivity.Run{Seat: "other-seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	p := drainProbe{client: srv.Client(), endpoint: srv.URL, model: "seat", runs: reg.OnSeat, every: 2 * time.Millisecond}
	if err := drainUntil(context.Background(), p, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("a run on another seat must not hold this drain: %v", err)
	}
}

// drainDeadline: the queue budget bounds the drain by default (floored), an
// explicit --drain-timeout wins.
func TestDrainDeadlineFollowsTheQueueBudget(t *testing.T) {
	queued := time.Now().Add(-time.Hour)
	if d := drainDeadline(0, queued, 8*time.Hour); d.Sub(queued) < 7*time.Hour+59*time.Minute {
		t.Fatalf("default deadline must be the rest of --wait from when queueing began, got %s", d.Sub(queued))
	}
	if d := drainDeadline(0, time.Now(), 0); time.Until(d) < drainFloor-time.Second {
		t.Fatalf("--wait 0 must still leave the drain floor, got %s", time.Until(d))
	}
	if d := drainDeadline(90*time.Second, time.Now(), 8*time.Hour); time.Until(d) > 91*time.Second {
		t.Fatalf("an explicit --drain-timeout must win, got %s", time.Until(d))
	}
}

// THE SECOND DEFECT: --unload-seat stamped the lease EXCLUSIVE at acquire, and
// the admission gate blocks every request under an exclusive lease, so the
// drain blocked the very run it was waiting for. The wrapper form must hold a
// DRAINING (non-exclusive) lease while it drains and stamp exclusive after.
func TestReserveDrainsUnderADrainingStampAndTurnsExclusiveAfter(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "seat"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	// Watch the lease while the drain runs: it must be held, DRAINING and NOT
	// exclusive until the seat goes idle.
	var sawDraining, sawExclusiveDuringDrain atomic.Bool
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Millisecond):
			}
			if info := m.Inspect(); info.Held && f.inflight.Load() > 0 {
				if info.Draining {
					sawDraining.Store(true)
				}
				if info.Exclusive {
					sawExclusiveDuringDrain.Store(true)
				}
			}
		}
	}()
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.inflight.Store(0) // the work finished
	}()
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	// The wrapped command reports the lease it inherited; TestHelperSleepMs
	// just exits. The unload is asserted through the fake.
	args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--unload-seat", "--reason", "arm"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	close(stop)
	if !sawDraining.Load() {
		t.Error("the lease was never observed DRAINING while the seat was busy")
	}
	if sawExclusiveDuringDrain.Load() {
		t.Error("the lease was EXCLUSIVE while the drain was still waiting — that is the deadlock")
	}
	if f.unloads.Load() != 1 {
		t.Errorf("the seat must be unloaded once after the drain, got %d", f.unloads.Load())
	}
	if f.warms.Load() != 1 {
		t.Errorf("the wrapper must warm the seat back once, got %d", f.warms.Load())
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the wrapper must release on exit; still held: %+v", info)
	}
}

// The drain can now run for hours, and the reclaim rule needs a stale heartbeat
// AND an expired window: the wrapper must heartbeat while it drains, not only
// once the wrapped command is running (reviewer finding, 0.117.0).
func TestReserveRenewsTheLeaseWhileDraining(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "seat"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	old := drainRenewEvery
	drainRenewEvery = 30 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	var first, latest time.Time
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if info := m.Inspect(); info.Held && f.inflight.Load() > 0 {
				if first.IsZero() {
					first = info.HeartbeatAt
				}
				if info.HeartbeatAt.After(latest) {
					latest = info.HeartbeatAt
				}
			}
		}
	}()
	go func() {
		time.Sleep(400 * time.Millisecond)
		f.inflight.Store(0)
	}()
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--reason", "long drain"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	close(stop)
	if first.IsZero() || !latest.After(first) {
		t.Fatalf("the heartbeat must move while the drain waits: first %v latest %v", first, latest)
	}
}

// The record names WHAT the wrapper runs, not only who.
func TestReserveStampsTheWrappedCommand(t *testing.T) {
	cfg, m := leaseFixture(t)
	t.Setenv("LO_HELPER_SLEEP_MS", "250")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--wait", "0", "--reason", "cmd stamp"}, helperCmd()...))
	}()
	deadline := time.Now().Add(3 * time.Second)
	var info gpulease.Info
	for time.Now().Before(deadline) {
		if info = m.Inspect(); info.Held {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !info.Held || !strings.Contains(info.Command, "-test.run=TestHelperSleepMs") {
		t.Fatalf("the lease record must carry the wrapped command, got %+v", info)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
