package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// llama-swap run with -watch-config reloads on ANY write to its config: it builds a brand-new
// server (every model COLD, no running process adopted), swaps it in, and only then shuts the old
// one down. A warm-back whose health request was parked in the old router (the request IS the
// load) is answered "<router> is shutting down", HTTP 500, while /running already speaks for the
// new, empty server. The warm used to read that empty /running, call the seat "not loading" and
// give up, leaving the seat cold and the warm-owed marker standing. These tests pin the repair:
// the warm re-sends the load to the new server, inside a bounded grace, and the marker clears once
// the seat is observed loaded. The fake is warmOrderSwap's reload knobs (gpu_acquire_warm_test.go).

// warmNotes collects what a warm says through warmNote.
type warmNotes struct {
	mu    sync.Mutex
	lines []string
}

func (n *warmNotes) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, s)
}

func (n *warmNotes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.lines...)
}

// fastWarmRetry shortens every clock of the warm for one test (the re-send backoff, the reload
// grace, the watch of a load that outlasts the health wait), collects what the warm says through
// warmNote, and restores all of it when the test ends.
func fastWarmRetry(t *testing.T, grace time.Duration) *warmNotes {
	t.Helper()
	oldFirst, oldMax, oldGrace := warmRetryFirst, warmRetryMax, warmReloadGrace
	oldWatch, oldEvery, oldNote := warmWatch, warmWatchEvery, warmNote
	notes := &warmNotes{}
	warmRetryFirst, warmRetryMax, warmReloadGrace = 5*time.Millisecond, 20*time.Millisecond, grace
	warmWatch, warmWatchEvery = 2*time.Second, 10*time.Millisecond
	warmNote = notes.add
	t.Cleanup(func() {
		warmRetryFirst, warmRetryMax, warmReloadGrace = oldFirst, oldMax, oldGrace
		warmWatch, warmWatchEvery, warmNote = oldWatch, oldEvery, oldNote
	})
	return notes
}

// testCtx bounds a test's warm so a broken loop fails the test instead of hanging it.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// reloadingSwap serves f like a llama-swap for the life of the test.
func reloadingSwap(t *testing.T, f *warmOrderSwap) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler("seat"))
	t.Cleanup(srv.Close)
	return srv
}

// A lease-form warm across a reload: the first two health requests land on the reloading server,
// the third on the new one and loads the seat. The wrapper's warm succeeds and the marker clears.
func TestWarmBackGuardedWrapperFormClearsTheOwedMarkerAfterARetriedWarm(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if !strings.Contains(out.String(), "warmed back") || strings.Contains(out.String(), "failed") {
		t.Fatalf("a warm across a reload must finish as warmed back: %s", out.String())
	}
	if owed := m.SeatWarmOwed(); owed != "" {
		t.Errorf("the warm landed, so the owed marker must clear, owed=%q", owed)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 3 || warms != 1 || !f.loaded.Load() {
		t.Errorf("two reloaded answers then one load: health requests=%d (want 3) warms=%d (want 1) loaded=%v", hits, warms, f.loaded.Load())
	}
}

// `gpu release --warm-seat --epoch N` across a reload: the warm retries under the lease being
// released, the marker clears, and the release follows.
func TestReleaseWarmSeatAcrossAReloadClearsTheMarkerAndReleases(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	if err := runGPURelease([]string{"--config", cfgPath, "--warm-seat", "--epoch", strconv.FormatUint(holder.Epoch(), 10)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if m.Inspect().Held {
		t.Error("the release must follow the warm")
	}
	if owed := m.SeatWarmOwed(); owed != "" {
		t.Errorf("the warm landed, so the owed marker must clear, owed=%q", owed)
	}
	if hits, warms := f.healthHits.Load(), f.warms.Load(); hits != 3 || warms != 1 {
		t.Errorf("health requests=%d (want 3) warms=%d (want 1)", hits, warms)
	}
}

// A warm that fails for a reason the seat's own state contradicts: the health request times out
// while the seat finished loading (here: it is listed ready the whole time). The tool must read
// the seat once more before it calls the warm failed, say what it saw, and clear the marker.
func TestWarmBackGuardedClearsTheMarkerWhenTheSeatIsObservedLoadedAfterAFailure(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	old := maintenanceClient
	maintenanceClient = &http.Client{Timeout: 150 * time.Millisecond}
	t.Cleanup(func() { maintenanceClient = old })
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 500 * time.Millisecond}
	f.onHealth = func(int32) { f.loaded.Store(true) } // up and ready; only the answer is stuck
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	for _, want := range []string{"the health request failed", "but the seat is loaded; treating it as warmed", "seat warmed back"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output must contain %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "warm-back of seat failed") {
		t.Errorf("a seat observed loaded is not a failed warm: %s", out.String())
	}
	if owed := m.SeatWarmOwed(); owed != "" {
		t.Errorf("a seat observed loaded owes nothing, owed=%q", owed)
	}
	if hits := f.healthHits.Load(); hits != 1 {
		t.Errorf("a client timeout is not a reload: the load request is sent once, got %d", hits)
	}
}

// "Loaded" is not "settled": a seat the final reading finds still starting (its load has not
// finished) is not a warm that happened, so the failure stands and the marker stays.
func TestWarmBackGuardedKeepsTheMarkerWhenTheSeatIsStillStartingAfterAFailure(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	old := maintenanceClient
	maintenanceClient = &http.Client{Timeout: 150 * time.Millisecond}
	t.Cleanup(func() { maintenanceClient = old })
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 800 * time.Millisecond}
	f.onHealth = func(int32) {
		f.loaded.Store(true)
		f.starting.Store(true) // listed, but the load has not finished
	}
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if !strings.Contains(out.String(), "warm-back of seat failed") || strings.Contains(out.String(), "treating it as warmed") {
		t.Errorf("a seat still starting is not a warm: %s", out.String())
	}
	if owed := m.SeatWarmOwed(); owed != "seat" {
		t.Errorf("the warm stays owed, owed=%q", owed)
	}
}

// The other face: a warm that failed and a seat that is still cold keeps its marker and says how
// the marker is cleared later.
func TestAFailedWarmOfAColdSeatKeepsTheOwedMarker(t *testing.T) {
	fastWarmRetry(t, 150*time.Millisecond)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	for _, want := range []string{"warm-back of seat failed", "no recovery within", "the warm stays owed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output must contain %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "treating it as warmed") || strings.Contains(out.String(), "seat warmed back") {
		t.Errorf("a cold seat is not a warm: %s", out.String())
	}
	if owed := m.SeatWarmOwed(); owed != "seat" {
		t.Errorf("a failed warm of a cold seat stays owed, owed=%q", owed)
	}
}

// A lease lost mid-warm abandons the warm, and that stays true even when the seat then reads
// loaded: the card is not ours any more, so the tool must not claim the warm or clear the marker
// on the strength of a reading taken after it lost the right to act.
func TestALostLeaseIsNotTreatedAsWarmedEvenIfTheSeatReadsLoaded(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 600 * time.Millisecond}
	f.loaded.Store(true) // listed ready from the start
	cfgPath, m := warmOrderFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 50 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	f.onWarm = func() { _, _ = m.ReleaseByEpoch(holder.Epoch()) }
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if !strings.Contains(out.String(), "LEASE LOST while warming") || !strings.Contains(out.String(), "warm-back of seat failed") {
		t.Errorf("a lease lost mid-warm must abandon the warm and say so: %s", out.String())
	}
	if strings.Contains(out.String(), "treating it as warmed") || strings.Contains(out.String(), "seat warmed back") {
		t.Errorf("a warm abandoned on a lost lease must not be reported as warmed: %s", out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Errorf("an abandoned warm stays owed: owed=%q", m.SeatWarmOwed())
	}
}

// The heartbeat runs through the back-off between re-sends, not only while a request is in
// flight: the retry holds the lease for up to the grace plus the load, and a heartbeat that
// stopped between two attempts would let the lease go stale under it.
func TestTheWarmBacksHeartbeatRunsAcrossARetriedWarm(t *testing.T) {
	fastWarmRetry(t, 10*time.Second)
	warmRetryFirst, warmRetryMax = 80*time.Millisecond, 80*time.Millisecond
	old := drainRenewEvery
	drainRenewEvery = 30 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 2}
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	// The lease's heartbeat as the stand-in sees it on the first and on the last request.
	var mu sync.Mutex
	var first, last time.Time
	f.onHealth = func(hit int32) {
		hb := m.Inspect().HeartbeatAt
		mu.Lock()
		defer mu.Unlock()
		if hit == 1 {
			first = hb
		}
		last = hb
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out.String(), "warmed back") || strings.Contains(out.String(), "LEASE LOST") {
		t.Fatalf("the retried warm must finish under its lease: %s", out.String())
	}
	if !last.After(first) {
		t.Fatalf("the heartbeat must move across the back-off between re-sends: first %v last %v", first, last)
	}
}

// The guards the warm started under are re-read before EVERY re-send. A retry can run for the
// grace plus a load, and in that time a successor can queue, the card can move to someone else,
// or another lease can take cards the seat sits on; a re-send then would be the warm landing on
// somebody else's lease (register D-124). The retry stops, says the same thing the first check
// would have said, leaves the marker alone, and the failure line is not printed.
func TestAWarmRetryReChecksTheGuardsBeforeEachResend(t *testing.T) {
	t.Run("a lease queues behind the warm", func(t *testing.T) {
		fastWarmRetry(t, 10*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		var started atomic.Bool
		queued := make(chan error, 1)
		f.onHealth = func(hit int32) {
			if hit != 2 {
				return
			}
			once.Do(func() {
				started.Store(true)
				go func() {
					l, aerr := m.Acquire(gpulease.ClassText, gpulease.Options{Reason: "behind", TTL: time.Hour, Wait: 20 * time.Second, WaitOut: true})
					if aerr == nil {
						_ = l.Release()
					}
					queued <- aerr
				}()
				deadline := time.Now().Add(10 * time.Second)
				for len(m.Waiters()) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			})
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
		_ = holder.Release()
		if started.Load() {
			select {
			case err := <-queued:
				if err != nil {
					t.Errorf("the queued lease must be granted once the holder lets go: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("the queued lease was never granted after the holder let go")
			}
		}
		if !strings.Contains(out.String(), "NOT warming seat back: 1 lease(s) queued behind this one") {
			t.Errorf("the re-check must say what the first check says: %s", out.String())
		}
		if hits := f.healthHits.Load(); hits != 2 {
			t.Errorf("no load request may be sent after a lease queued: health requests=%d (want 2)", hits)
		}
		if strings.Contains(out.String(), "warm-back of seat failed") || m.SeatWarmOwed() != "seat" {
			t.Errorf("a stopped retry is not a failed warm, and the warm stays owed (owed=%q): %s", m.SeatWarmOwed(), out.String())
		}
	})
	t.Run("the card moved on", func(t *testing.T) {
		fastWarmRetry(t, 10*time.Second)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		// The heartbeat is the slow path (15 s by default); the re-check is the fast one.
		f.onHealth = func(hit int32) {
			if hit == 2 {
				_, _ = m.ReleaseByEpoch(holder.Epoch())
			}
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
		_ = holder.Release()
		if !strings.Contains(out.String(), "NOT warming seat back: the card is no longer ours") {
			t.Errorf("the re-check must say what the first check says: %s", out.String())
		}
		if hits := f.healthHits.Load(); hits != 2 {
			t.Errorf("no load request may be sent after the lease is gone: health requests=%d (want 2)", hits)
		}
		if strings.Contains(out.String(), "warm-back of seat failed") || m.SeatWarmOwed() != "seat" {
			t.Errorf("a stopped retry is not a failed warm, and the warm stays owed (owed=%q): %s", m.SeatWarmOwed(), out.String())
		}
	})
	t.Run("another lease takes cards the seat sits on", func(t *testing.T) {
		fastWarmRetry(t, 10*time.Second)
		cfgPath, m, _ := warmCardFixture(t)
		// warmCardFixture's own stand-in has no reload knobs; serve one of ours on the same config.
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
		srv := reloadingSwap(t, f)
		cfgPath = rewriteWarmEndpoint(t, cfgPath, srv.URL)
		a := acquireCard(t, m, "film card 0", "gpu-aaaa0000-x")
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		f.onHealth = func(hit int32) {
			if hit != 2 {
				return
			}
			once.Do(func() {
				l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film card 2", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour})
				if err != nil {
					t.Errorf("the second card lease: %v", err)
					return
				}
				t.Cleanup(func() { _ = l.Release() })
			})
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
		if !strings.Contains(out.String(), "NOT warming seat back yet") || !strings.Contains(out.String(), "lease epoch") {
			t.Errorf("the re-check must say what the first check says: %s", out.String())
		}
		if hits := f.healthHits.Load(); hits != 2 {
			t.Errorf("no load request may be sent over a lease on the seat's cards: health requests=%d (want 2)", hits)
		}
		if strings.Contains(out.String(), "warm-back of seat failed") || m.SeatWarmOwed() != "seat" {
			t.Errorf("a stopped retry is not a failed warm, and the warm stays owed (owed=%q): %s", m.SeatWarmOwed(), out.String())
		}
	})
}

// rewriteWarmEndpoint points an existing test config at another llama-swap stand-in, keeping
// every other key (the state root, the seat, the card-scoped switch).
func rewriteWarmEndpoint(t *testing.T, cfgPath, endpoint string) string {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["endpoint"] = endpoint
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// The whole story of the stale marker, with the real seat read: a warm gives up (the reload
// never recovered inside the window) and leaves the marker; later the seat is loaded by someone
// else's request; the next `gpu status` reads the seat through the config's own endpoint and
// seat name, finds the card free and the seat loaded, and clears the marker. The names must
// agree end to end: the marker holds the seat the unload stamped, the status reads the seat the
// config names.
func TestAFailedWarmsMarkerIsClearedByGPUStatusOnceTheSeatIsLoaded(t *testing.T) {
	fastWarmRetry(t, 100*time.Millisecond)
	f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
	cfgPath, m := warmOrderFixture(t, f)
	useQuietStatus(t, statusCards(), nil)
	// The live activity read, minus the nvidia-smi sample a test host does not have.
	statusActivityFn = func(ctx context.Context, o gpuactivity.Options) gpuactivity.View {
		o.SampleGPU = false
		return gpuactivity.Snapshot(ctx, o)
	}
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	// Stamped when the unload happened, a warm's length ago (the status clear wants a marker older
	// than its own readings).
	markOwedAgo(t, m, "seat", time.Minute)
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	if !strings.Contains(out.String(), "warm-back of seat failed") || m.SeatWarmOwed() != "seat" {
		t.Fatalf("the warm must fail and leave the marker (owed=%q): %s", m.SeatWarmOwed(), out.String())
	}
	// While the card is held the marker is the holder's, even with the seat loaded.
	f.loaded.Store(true)
	held := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfgPath}); err != nil {
			t.Fatal(err)
		}
	})
	if m.SeatWarmOwed() != "seat" || !strings.Contains(held, "seat warm-back owed: seat") {
		t.Fatalf("a held card keeps the marker (owed=%q):\n%s", m.SeatWarmOwed(), held)
	}
	_ = holder.Release()
	free := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfgPath}); err != nil {
			t.Fatal(err)
		}
	})
	if m.SeatWarmOwed() != "" || strings.Contains(free, "warm-back owed") {
		t.Fatalf("a free card and a loaded seat owe nothing (owed=%q):\n%s", m.SeatWarmOwed(), free)
	}
}

// The final reading clears the marker only while the guards still allow touching the card. The
// reading takes a moment and the heartbeat that would notice a lost lease ticks every 15 s, so
// a warm that fails with the seat reading loaded can find that the card has moved on, or that
// a successor has queued, by the time it would clear the marker. Then the marker is left for
// whoever owns it now, the output says why, and the warm is not reported as done.
func TestWarmBackGuardedLeavesTheMarkerWhenTheGuardsNoLongerAllowTheObservedLoadedClear(t *testing.T) {
	setup := func(t *testing.T) (*warmOrderSwap, string, *gpulease.Manager, *gpulease.Lease) {
		t.Helper()
		fastWarmRetry(t, 5*time.Second)
		old := maintenanceClient
		maintenanceClient = &http.Client{Timeout: 150 * time.Millisecond}
		t.Cleanup(func() { maintenanceClient = old })
		f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 500 * time.Millisecond}
		f.onHealth = func(int32) { f.loaded.Store(true) } // up and ready; only the answer is stuck
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		return f, cfgPath, m, holder
	}
	check := func(t *testing.T, out string, m *gpulease.Manager, want string) {
		t.Helper()
		for _, w := range []string{"warm-back of seat failed", "the seat reads loaded, but the marker is left alone", want, "the warm stays owed"} {
			if !strings.Contains(out, w) {
				t.Errorf("the output must contain %q: %s", w, out)
			}
		}
		if strings.Contains(out, "treating it as warmed") || strings.Contains(out, "seat warmed back") {
			t.Errorf("a warm that may not touch the marker is not reported as done: %s", out)
		}
		if owed := m.SeatWarmOwed(); owed != "seat" {
			t.Errorf("the marker stays for the lease that owns it now, owed=%q", owed)
		}
	}
	t.Run("the card moved on", func(t *testing.T) {
		f, cfgPath, m, holder := setup(t)
		f.onWarm = func() { _, _ = m.ReleaseByEpoch(holder.Epoch()) } // lost while the request hangs
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
		_ = holder.Release()
		check(t, out.String(), m, "the card is no longer ours")
	})
	t.Run("a lease queued behind the warm", func(t *testing.T) {
		f, cfgPath, m, holder := setup(t)
		var once sync.Once
		queued := make(chan error, 1)
		f.onWarm = func() {
			once.Do(func() {
				go func() {
					l, aerr := m.Acquire(gpulease.ClassText, gpulease.Options{Reason: "behind", TTL: time.Hour, Wait: 20 * time.Second, WaitOut: true})
					if aerr == nil {
						_ = l.Release()
					}
					queued <- aerr
				}()
				deadline := time.Now().Add(10 * time.Second)
				for len(m.Waiters()) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			})
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
		_ = holder.Release()
		select {
		case err := <-queued:
			if err != nil {
				t.Errorf("the queued lease must be granted once the holder lets go: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the queued lease was never granted after the holder let go")
		}
		check(t, out.String(), m, "1 lease(s) queued behind this one")
	})
}

// What a failed warm says follows what it knows. "The warm stays owed" is only true when a
// marker is there: an explicit `gpu release --warm-seat` over a seat nothing was owed to has no
// debt to keep. A window that ran out over an unreadable server says the state could not be
// read (not "not loading"), the announcement does not call an unreadable seat cold, and a
// confirming read that fails says it could not confirm.
func TestAFailedWarmSaysOnlyWhatItKnows(t *testing.T) {
	t.Run("nothing owed", func(t *testing.T) {
		fastWarmRetry(t, 100*time.Millisecond)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, holder.Epoch()), &out)
		_ = holder.Release()
		if !strings.Contains(out.String(), "warm-back of seat failed") || !strings.Contains(out.String(), "not loading") {
			t.Errorf("the failure is said: %s", out.String())
		}
		if strings.Contains(out.String(), "stays owed") || strings.Contains(out.String(), "gpu status") {
			t.Errorf("no marker is there, so no debt stays and nothing is left for `gpu status` to clear: %s", out.String())
		}
	})
	t.Run("owed", func(t *testing.T) {
		fastWarmRetry(t, 100*time.Millisecond)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20}
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, holder.Epoch()), &out)
		_ = holder.Release()
		if !strings.Contains(out.String(), "the warm stays owed (`gpu status` clears it once the seat is observed loaded)") {
			t.Errorf("a marker that stays is said to stay: %s", out.String())
		}
	})
	t.Run("an unreadable server", func(t *testing.T) {
		notes := fastWarmRetry(t, 100*time.Millisecond)
		f := &warmOrderSwap{drainSwap: &drainSwap{}, reloadFails: 1 << 20, reloadStatus: http.StatusServiceUnavailable, reloadBody: "gone"}
		f.runningDown = func() bool { return true }
		cfgPath, m := warmOrderFixture(t, f)
		holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkSeatWarmOwed("seat"); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
		_ = holder.Release()
		for _, want := range []string{"warm-back of seat failed", "the seat's state could not be read; no recovery within", "could not confirm the seat's state ("} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the output must contain %q: %s", want, out.String())
			}
		}
		if strings.Contains(out.String(), "not loading") {
			t.Errorf("an unreadable seat is not a seat that is not loading: %s", out.String())
		}
		got := notes.all()
		if len(got) != 1 || !strings.Contains(got[0], "the seat reads cold or unreadable") || strings.Contains(got[0], "the seat is cold") {
			t.Errorf("the announcement does not call an unreadable seat cold: %q", got)
		}
		if owed := m.SeatWarmOwed(); owed != "seat" {
			t.Errorf("an unconfirmed warm stays owed, owed=%q", owed)
		}
	})
}

// The observed-loaded clear is a compare-and-delete by seat, not a blind remove: a marker that
// names another seat by the time the warm finishes is somebody else's debt and stays.
func TestWarmBackGuardedObservedLoadedClearLeavesAMarkerThatNamesAnotherSeat(t *testing.T) {
	fastWarmRetry(t, 5*time.Second)
	old := maintenanceClient
	maintenanceClient = &http.Client{Timeout: 150 * time.Millisecond}
	t.Cleanup(func() { maintenanceClient = old })
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 500 * time.Millisecond}
	f.onHealth = func(int32) { f.loaded.Store(true) } // up and ready; only the answer is stuck
	cfgPath, m := warmOrderFixture(t, f)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	f.onWarm = func() { _ = m.MarkSeatWarmOwed("other-seat") }
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if !strings.Contains(out.String(), "treating it as warmed") {
		t.Errorf("the seat reads loaded and the guards allow: %s", out.String())
	}
	if owed := m.SeatWarmOwed(); owed != "other-seat" {
		t.Errorf("a marker for another seat is not this warm's to clear, owed=%q", owed)
	}
}
