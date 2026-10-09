package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Register D-124, clause (b): an acquire with --unload-seat must find the card
// EMPTY, however the previous holder's warm-back stands — owed, or in flight.
// The live failure (2026-09-19): after `gpu reserve --unload-seat` the seat was
// STILL LOADED, because the previous holder's deferred warm-back landed between
// the new holder's unload and its first load.
//
// WHERE THE ORDER COMES FROM. The warm-back runs under the releasing holder's own
// lease (warmBackGuarded, then Release) and is skipped when a lease is queued
// behind it, so a queued `--unload-seat` acquire cannot take the card, drain it
// or unload anything until the warm has settled. There is NO acquire-side wait:
// the releasing lease outliving its warm is the whole mechanism, and a second
// wait on top of it would only double the queue. These tests pin that order from
// the acquirer's side — what its command finds when it starts — and from the
// lease's side — whether it was still held when the warm landed. A warm that
// ran after the release would fail BOTH.
//
// The orderings, as they stand:
//
//  1. Wrapper form. The warm runs before Release(), under the holder's lease,
//     heartbeat for its length. A queued acquirer is either seen as a waiter
//     (the warm is skipped; it belongs to the last holder) or waits for the
//     release that follows the warm. Its drain and unload run after the warm
//     settled.
//  2. Owed, not in flight (a holder lost its lease before it could warm): the
//     marker stays; the next --unload-seat holder drains a cold seat, runs its
//     command on the cleared card and pays the warm at its own release.
//  3. `gpu release --warm-seat --epoch N` with the detached holder alive: the
//     warm runs under the holder's lease, and the release follows it.
//  4. `gpu release --warm-seat` on a FREE card (the detached holder's window
//     ended first): nothing holds the card, so the warm runs unleased and an
//     acquirer is not ordered behind it. That is the operator-explicit path and
//     is left as it is.

// seatEvent is one thing the llama-swap stand-in saw, with the seat's listing
// and the machine-wide lease as they were at that instant.
type seatEvent struct {
	name   string
	loaded bool   // the seat is listed in /running
	holder string // the reason the current lease holder gave; "" when the card is free
}

// warmOrderSwap is the drain fake plus an ordered event log, a warm that takes
// time, and the two shapes a slow load can take on the wire.
type warmOrderSwap struct {
	*drainSwap
	mu     sync.Mutex
	events []seatEvent
	// warmHold is how long a warm (a model load) takes.
	warmHold time.Duration
	// visibleLoad lists the seat as `starting` while the load runs, which is what
	// llama-swap does once the swap reached it. Left false the seat is NOT listed
	// until it lands: the worst case, where an acquirer's drain cannot see the
	// load at all and only the order protects it.
	visibleLoad bool
	// healthFails answers the warm's health request 500 at once and finishes the
	// load in the background: llama-swap's own health wait ran out before a long
	// cold load did.
	healthFails bool
	// holder names who holds the lease right now ("" when nobody does).
	holder func() string
	// onWarm runs when a warm request arrives.
	onWarm func()

	// The llama-swap -watch-config reload shape. With that flag any write to the
	// config swaps in a brand-new, COLD server and shuts the old one down; the warm's
	// health request, parked in the old router, is answered "<router> is shutting down"
	// (HTTP 500) while /running already speaks for the new, empty server.
	// reloadFails is how many health requests (counted from the first) are answered
	// that way; they load nothing, so /running stays empty.
	reloadFails int32
	// reloadStatus and reloadBody replace the shutting-down 500 for those requests
	// (a 502/503/504, a 404, a bare 500): a zero status keeps the reload answer.
	reloadStatus int
	reloadBody   string
	// reloadHijack closes the connection instead of answering: the shape of a full
	// llama-swap restart, whose listener goes away.
	reloadHijack bool
	// runningDownUntilHit answers /running 503 while fewer health requests than this
	// have arrived: the whole server is down, not just the old router.
	runningDownUntilHit int32
	// reloadAnswerFn, when set, decides the answer of the reloadFails requests by hit number
	// (a reload that is followed by a failed start); it replaces reloadStatus and reloadBody.
	reloadAnswerFn func(hit int32) (status int, body string)
	// onHealth runs on every health request with its 1-based hit number, before it is answered.
	onHealth func(hit int32)
	// healthHits counts every health request, reloaded or not.
	healthHits atomic.Int32
}

// shuttingDownBody is what llama-swap's router answers a request it was holding when its
// config reload shut it down (HTTP 500, the error text has no status mapping).
const shuttingDownBody = `{"src":"llama-swap","error":{"message":"unspecific error: matrix is shutting down","type":"server_error","param":null,"code":"internal_error"}}`

// answerReload is the stand-in's answer to a health request that landed on a reloading server.
func (f *warmOrderSwap) answerReload(w http.ResponseWriter, hit int32) {
	if f.reloadHijack {
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			c.Close()
		}
		return
	}
	status, body := f.reloadStatus, f.reloadBody
	if f.reloadAnswerFn != nil {
		status, body = f.reloadAnswerFn(hit)
	}
	if status == 0 {
		status, body = http.StatusInternalServerError, shuttingDownBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *warmOrderSwap) note(name string) {
	holder := ""
	if f.holder != nil {
		holder = f.holder()
	}
	f.mu.Lock()
	f.events = append(f.events, seatEvent{name: name, loaded: f.loaded.Load(), holder: holder})
	f.mu.Unlock()
}

func (f *warmOrderSwap) snapshot() []seatEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seatEvent(nil), f.events...)
}

// indexOf is the position of the first event called name at or after from, or -1.
func indexOf(ev []seatEvent, name string, from int) int {
	for i := from; i >= 0 && i < len(ev); i++ {
		if ev[i].name == name {
			return i
		}
	}
	return -1
}

func eventNames(ev []seatEvent) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.name)
	}
	return out
}

func (f *warmOrderSwap) handler(model string) http.Handler {
	inner := f.drainSwap.handler(model)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/unload/"+model, func(w http.ResponseWriter, r *http.Request) {
		f.note("unload")
		inner.ServeHTTP(w, r)
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		if f.healthHits.Load() < f.runningDownUntilHit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	mux.HandleFunc("/upstream/"+model+"/health", func(w http.ResponseWriter, r *http.Request) {
		hit := f.healthHits.Add(1)
		if f.onHealth != nil {
			f.onHealth(hit)
		}
		if hit <= f.reloadFails {
			f.answerReload(w, hit)
			return
		}
		f.note("warm")
		if f.onWarm != nil {
			f.onWarm()
		}
		if f.visibleLoad {
			f.loaded.Store(true)
			f.starting.Store(true)
		}
		// The event is recorded BEFORE the seat shows as ready: once it does, the
		// warm's watch can return and the lease can be released, and the event
		// would then read a lease that has already moved on.
		land := func() {
			f.warms.Add(1)
			f.note("landed")
			f.starting.Store(false)
			f.loaded.Store(true)
		}
		if f.healthFails {
			go func() {
				time.Sleep(f.warmHold)
				land()
			}()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		time.Sleep(f.warmHold)
		land()
		w.WriteHeader(http.StatusOK)
	})
	// A wrapped command reports where it started and ended; the seat's listing
	// at that instant is the card the command found.
	mux.HandleFunc("/mark/", func(w http.ResponseWriter, r *http.Request) {
		f.note(strings.TrimPrefix(r.URL.Path, "/mark/"))
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", inner)
	return mux
}

// probeSeat is the wrapped command of these tests: it reports its start, stays
// alive for LO_HELPER_SLEEP_MS, and reports its end. A no-op unless the parent
// names the stand-in's address. It always exits 0: a wrapped command that
// fails makes the reserve exit the whole test binary.
func probeSeat(label string) {
	base := os.Getenv("LO_HELPER_PROBE_URL")
	if base == "" {
		return
	}
	mark := func(name string) {
		if resp, err := http.Get(base + "/mark/" + name); err == nil {
			resp.Body.Close()
		}
	}
	mark(label + "-start")
	ms, _ := strconv.Atoi(os.Getenv("LO_HELPER_SLEEP_MS"))
	time.Sleep(time.Duration(ms) * time.Millisecond)
	mark(label + "-end")
}

func TestHelperSeatProbeA(t *testing.T) { probeSeat("A") }
func TestHelperSeatProbeB(t *testing.T) { probeSeat("B") }

// helperCmdFor is helperCmd for a named helper test, anchored so one helper's
// name can never select another.
func helperCmdFor(name string) []string {
	return []string{"--", os.Args[0], "-test.run=^" + name + "$", "-test.timeout=30s"}
}

func warmOrderFixture(t *testing.T, f *warmOrderSwap) (cfgPath string, m *gpulease.Manager) {
	t.Helper()
	srv := httptest.NewServer(f.handler("seat"))
	t.Cleanup(srv.Close)
	t.Setenv("LO_HELPER_PROBE_URL", srv.URL)
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
	f.holder = func() string {
		if info := m.Inspect(); info.Held {
			return info.Reason
		}
		return ""
	}
	return cfgPath, m
}

func reserveArgs(cfgPath, reason, probe string) []string {
	return append([]string{"--config", cfgPath, "--wait", "30s", "--drain", "--unload-seat", "--reason", reason}, helperCmdFor(probe)...)
}

// handOffDuringTheWarm: A finishes its command and warms the seat back; B (also
// --drain --unload-seat) arrives while that warm is in flight. B must not
// unload anything before A's warm has landed, and B's command must start on an
// empty card and keep it empty.
func handOffDuringTheWarm(t *testing.T, healthFails bool) {
	t.Helper()
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 700 * time.Millisecond, healthFails: healthFails, visibleLoad: healthFails}
	f.loaded.Store(true)
	cfgPath, m := warmOrderFixture(t, f)
	if healthFails {
		old, oldEvery := warmWatch, warmWatchEvery
		warmWatch, warmWatchEvery = 10*time.Second, 10*time.Millisecond
		t.Cleanup(func() { warmWatch, warmWatchEvery = old, oldEvery })
	}
	t.Setenv("LO_HELPER_SLEEP_MS", "1000")
	bDone := make(chan error, 1)
	var once sync.Once
	f.onWarm = func() {
		once.Do(func() {
			go func() { bDone <- runGPUReserve(reserveArgs(cfgPath, "B", "TestHelperSeatProbeB")) }()
		})
	}
	if err := runGPUReserve(reserveArgs(cfgPath, "A", "TestHelperSeatProbeA")); err != nil {
		t.Fatalf("A: %v", err)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B: %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("B never returned")
	}
	ev := f.snapshot()
	landed := indexOf(ev, "landed", 0)
	if landed < 0 {
		t.Fatalf("A's warm never landed: %v", eventNames(ev))
	}
	if ev[landed].holder != "A" {
		t.Errorf("A's warm landed while the card was held by %q: the releasing lease (A's) must outlive its warm: %v", ev[landed].holder, eventNames(ev))
	}
	first := indexOf(ev, "unload", 0)
	bUnload := indexOf(ev, "unload", first+1)
	bStart := indexOf(ev, "B-start", 0)
	bEnd := indexOf(ev, "B-end", 0)
	if first < 0 || bUnload < 0 || bStart < 0 || bEnd < 0 {
		t.Fatalf("B never cleared the seat and ran: %v", eventNames(ev))
	}
	if bUnload < landed {
		t.Errorf("B unloaded the seat before A's warm had landed, so the warm landed on B's cleared card: %v", eventNames(ev))
	}
	if !(bUnload < bStart) {
		t.Errorf("B must clear the seat before its command starts: %v", eventNames(ev))
	}
	if ev[bStart].loaded {
		t.Errorf("B's command started on a card with the seat loaded: %v", eventNames(ev))
	}
	if late := indexOf(ev, "landed", bStart); late >= 0 && late < bEnd {
		t.Errorf("a warm-back landed while B's command ran on the cleared card: %v", eventNames(ev))
	}
	if n := f.unloads.Load(); n != 2 {
		t.Errorf("each reserve clears the seat once: unloads=%d", n)
	}
	if n := f.warms.Load(); n != 2 || m.SeatWarmOwed() != "" || !f.loaded.Load() {
		t.Errorf("the last holder pays the one remaining warm: warms=%d owed=%q loaded=%v", n, m.SeatWarmOwed(), f.loaded.Load())
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("both reserves must have released: %+v", info)
	}
}

// The ordinary shape: the warm's own request carries the load.
func TestAnUnloadSeatAcquireStartsOnAnEmptyCardWhileTheLastHoldersWarmIsInFlight(t *testing.T) {
	handOffDuringTheWarm(t, false)
}

// The warm's health request is answered 5xx while the seat is still loading
// (llama-swap's health wait ran out). The warm watches the load, still under
// the lease, so the hand-off order holds.
func TestAnUnloadSeatAcquireStartsOnAnEmptyCardWhenTheWarmOutlastsTheHealthWait(t *testing.T) {
	handOffDuringTheWarm(t, true)
}

// A warm that is OWED but not in flight: the previous holder lost its lease
// before it could pay it. The next --unload-seat holder drains a cold seat,
// runs on the cleared card, and pays the one warm at its own release — never
// ahead of its command.
func TestAnUnloadSeatAcquireOverAnOwedWarmFindsAColdCardAndPaysTheWarmAtTheEnd(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 100 * time.Millisecond}
	f.loaded.Store(true)
	cfgPath, m := warmOrderFixture(t, f)
	t.Setenv("LO_HELPER_SLEEP_MS", "600")
	aDone := make(chan error, 1)
	go func() { aDone <- runGPUReserve(reserveArgs(cfgPath, "A", "TestHelperSeatProbeA")) }()
	deadline := time.Now().Add(30 * time.Second)
	for indexOf(f.snapshot(), "A-start", 0) < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("A's command never started: %v", eventNames(f.snapshot()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The card leaves A under its command (an operator release, a reclaim): A
	// is fenced out and must not warm.
	if _, err := m.ReleaseByEpoch(0); err != nil {
		t.Fatal(err)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	if f.warms.Load() != 0 || f.loaded.Load() || m.SeatWarmOwed() != "seat" {
		t.Fatalf("A lost its lease: no warm, the seat stays cleared, the warm stays owed: warms=%d loaded=%v owed=%q", f.warms.Load(), f.loaded.Load(), m.SeatWarmOwed())
	}
	if err := runGPUReserve(reserveArgs(cfgPath, "B", "TestHelperSeatProbeB")); err != nil {
		t.Fatalf("B: %v", err)
	}
	ev := f.snapshot()
	bStart, bEnd := indexOf(ev, "B-start", 0), indexOf(ev, "B-end", 0)
	if bStart < 0 || bEnd < 0 {
		t.Fatalf("B never ran: %v", eventNames(ev))
	}
	if ev[bStart].loaded {
		t.Errorf("B's command found the owed seat already warmed: the warm is paid after the command, not before it: %v", eventNames(ev))
	}
	landed := indexOf(ev, "landed", 0)
	if landed < bEnd {
		t.Errorf("the owed warm must land after B's command, not during or before it: %v", eventNames(ev))
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" || !f.loaded.Load() {
		t.Errorf("B pays the one owed warm: warms=%d owed=%q loaded=%v", n, m.SeatWarmOwed(), f.loaded.Load())
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("B must have released: %+v", info)
	}
}

// `gpu release --warm-seat --epoch N` with the detached holder alive: the warm
// runs under the lease it is about to release, so a queued acquirer cannot take
// the card, drain it or unload anything until the warm has landed.
func TestReleaseWarmSeatWarmsBeforeItReleasesTheLease(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 400 * time.Millisecond}
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
	ev := f.snapshot()
	landed := indexOf(ev, "landed", 0)
	if landed < 0 {
		t.Fatalf("the warm never landed: %v", eventNames(ev))
	}
	if ev[landed].holder != "detached" {
		t.Errorf("the warm landed while the card was held by %q, not by the lease being released: the releasing lease must outlive its warm: %v", ev[landed].holder, eventNames(ev))
	}
	if m.Inspect().Held || m.SeatWarmOwed() != "" || f.warms.Load() != 1 {
		t.Errorf("released and warmed once with the marker cleared: held=%v owed=%q warms=%d", m.Inspect().Held, m.SeatWarmOwed(), f.warms.Load())
	}
}
