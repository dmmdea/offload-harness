package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The lease hand-off race (register D-124, 2026-09-18 02:57 on the Lenovo):
// a holder's release-side warm-back ran unordered against the NEXT lease's
// --unload-seat and loaded the agent seat onto a card that lease held
// exclusively; three trial rows measured the previous holder's seat. These
// tests pin the order: with a successor queued the warm is skipped (the last
// holder pays it), a holder that lost the card never warms, and the warm that
// does run is heartbeat for its length.

// orderedSwap wraps the drain fake and records the ORDER of unloads and warms.
type orderedSwap struct {
	*drainSwap
	mu     sync.Mutex
	events []string
	// warmHold is how long a warm (a model load) takes; onWarm runs at its start.
	warmHold time.Duration
	onWarm   func()
}

func (o *orderedSwap) note(ev string) {
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}

func (o *orderedSwap) log() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func (o *orderedSwap) handler(model string) http.Handler {
	inner := o.drainSwap.handler(model)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/unload/"+model, func(w http.ResponseWriter, r *http.Request) {
		o.note("unload")
		inner.ServeHTTP(w, r)
	})
	mux.HandleFunc("/upstream/"+model+"/health", func(w http.ResponseWriter, r *http.Request) {
		o.note("warm")
		if o.onWarm != nil {
			o.onWarm()
		}
		time.Sleep(o.warmHold)
		inner.ServeHTTP(w, r)
	})
	mux.Handle("/", inner)
	return mux
}

func handoffFixture(t *testing.T, f *orderedSwap) (cfgPath string, m *gpulease.Manager) {
	t.Helper()
	srv := httptest.NewServer(f.handler("seat"))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfgPath = filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "seat"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, m
}

// Two leases queued on one card, both --unload-seat: the first must NOT warm
// the seat back (its successor would unload it again — and on the incident
// night the warm landed on the successor's exclusive card); the LAST holder
// warms it once. Order on the wire: unload, unload, warm.
func TestReserveLeavesTheWarmBackToTheLastHolderWhenALeaseIsQueued(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	t.Setenv("LO_HELPER_SLEEP_MS", "400")
	first := make(chan error, 1)
	go func() {
		args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--unload-seat", "--reason", "first"}, helperCmd()...)
		first <- runGPUReserve(args)
	}()
	// Wait until the first holder has the card, then queue the second.
	deadline := time.Now().Add(5 * time.Second)
	for !m.Inspect().Held && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !m.Inspect().Held {
		t.Fatal("the first reserve never took the card")
	}
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--unload-seat", "--reason", "second"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	got := f.log()
	want := []string{"unload", "unload", "warm"}
	if len(got) != len(want) {
		t.Fatalf("wire order %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wire order %v, want %v", got, want)
		}
	}
	if owed := m.SeatWarmOwed(); owed != "" {
		t.Fatalf("the last holder paid the warm; the marker must be cleared, still %q", owed)
	}
	if !f.loaded.Load() {
		t.Fatal("the seat must be loaded once the last holder released")
	}
	if m.Inspect().Held {
		t.Fatal("both leases must have released")
	}
}

// A holder whose lease was taken away (an operator `gpu release`, a reclaim)
// must not warm the seat onto a card that is no longer its own; the warm stays
// owed for the holder that ends up last.
func TestReserveDoesNotWarmBackAfterLosingTheLease(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	t.Setenv("LO_HELPER_SLEEP_MS", "400")
	done := make(chan error, 1)
	go func() {
		args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--unload-seat", "--reason", "cut"}, helperCmd()...)
		done <- runGPUReserve(args)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.unloads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.unloads.Load() == 0 {
		t.Fatal("the holder never unloaded the seat")
	}
	// The operator frees the card under the running command (or the next
	// waiter reclaims it): the holder is fenced out from here on.
	if _, err := m.ReleaseByEpoch(0); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if f.warms.Load() != 0 {
		t.Fatalf("a fenced-out holder must not warm the seat back, warms=%d events=%v", f.warms.Load(), f.log())
	}
	if owed := m.SeatWarmOwed(); owed != "seat" {
		t.Fatalf("the warm stays owed to the last holder, marker %q", owed)
	}
}

// The warm is a model load — minutes on a 27B — and the heartbeat TTL is two:
// the lease must be renewed for the warm's whole length or a queued waiter
// with an expired window reclaims the card mid-load (the other face of the
// same race).
func TestReserveRenewsTheLeaseWhileWarmingBack(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}, warmHold: 300 * time.Millisecond}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 30 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	// Sampled INSIDE the warm handler: the lease at the start of the load and
	// again just before it answers — the whole load happens in between.
	var mu sync.Mutex
	var atStart, atEnd time.Time
	var heldAtStart, heldAtEnd bool
	f.onWarm = func() {
		info := m.Inspect()
		mu.Lock()
		heldAtStart, atStart = info.Held, info.HeartbeatAt
		mu.Unlock()
		// Sample through the load and keep the LATEST heartbeat seen: one
		// read can land on a heartbeat write and fall back to the acquire
		// stamp (seen on CI), which is the reader's contract, not a lost beat.
		until := time.Now().Add(f.warmHold)
		for time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
			info = m.Inspect()
			mu.Lock()
			if info.Held {
				heldAtEnd = true
			}
			if info.HeartbeatAt.After(atEnd) {
				atEnd = info.HeartbeatAt
			}
			mu.Unlock()
		}
	}
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	args := append([]string{"--config", cfgPath, "--wait", "10s", "--drain", "--unload-seat", "--reason", "warm"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !heldAtStart || !heldAtEnd {
		t.Fatalf("the lease must be held for the warm's whole length (start %v, end %v)", heldAtStart, heldAtEnd)
	}
	if !atEnd.After(atStart) {
		t.Fatalf("the heartbeat must move while the warm runs: start %v latest %v", atStart, atEnd)
	}
	if f.warms.Load() != 1 || m.SeatWarmOwed() != "" {
		t.Fatalf("exactly one warm, marker cleared: warms=%d owed=%q", f.warms.Load(), m.SeatWarmOwed())
	}
}

// `gpu release --warm-seat` obeys the same rule: with a waiter queued the warm
// is left to the last holder, and a release of a stale epoch never warms.
func TestReleaseWarmSeatSkipsTheWarmWhenALeaseIsQueued(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	cfgPath, m := handoffFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(gpulease.ClassText, gpulease.Options{Reason: "next", TTL: time.Hour, Wait: 5 * time.Second, WaitOut: true})
		if aerr == nil {
			_ = l.Release()
		}
		waited <- aerr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(m.Waiters()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(m.Waiters()) == 0 {
		t.Fatal("the queued acquire never registered as a waiter")
	}
	if err := runGPURelease([]string{"--config", cfgPath, "--warm-seat", "--epoch", strconv.FormatUint(holder.Epoch(), 10)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if aerr := <-waited; aerr != nil {
		t.Fatalf("the waiter must acquire after the release: %v", aerr)
	}
	if f.warms.Load() != 0 {
		t.Fatalf("release --warm-seat must leave the warm to the queued lease, warms=%d", f.warms.Load())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Fatalf("the warm stays owed, marker %q", m.SeatWarmOwed())
	}
	// Nobody queued any more: the same verb now pays the warm.
	if err := runGPURelease([]string{"--config", cfgPath, "--warm-seat"}); err != nil {
		t.Fatalf("release (free card): %v", err)
	}
	if f.warms.Load() != 1 || m.SeatWarmOwed() != "" {
		t.Fatalf("with nobody queued the warm runs and clears the marker: warms=%d owed=%q", f.warms.Load(), m.SeatWarmOwed())
	}
}
