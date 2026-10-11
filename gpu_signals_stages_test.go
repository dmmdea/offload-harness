package main

// 0.180 media-branch final check: `gpu reserve` registered its signal handler only after the card was opened, the
// queue wait (acquireQueued, acquireAutoCards) and the drain/unload loop, so a console close during a --wait 8h or a
// long drain was unhandled: Go ended the process at once, the PAIR card stayed open until a sweep closed it "harness
// process exited before the job finished", and the waiter's record stayed in the line. The handler is now registered
// before anything else and every stage returns on it: the queue wait through gpulease.Options.Cancel, the card
// choice through the allocator's build and sleep, the drain through its context. These tests deliver the signal a
// console close would through the wrapper's seam (a real one cannot be driven from a test) at each stage.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestHelperWriteMarker is the wrapped command of the stage tests: it writes LO_HELPER_MARKER if the wrapper ever
// started it. A no-op unless set.
func TestHelperWriteMarker(t *testing.T) {
	if p := os.Getenv("LO_HELPER_MARKER"); p != "" {
		_ = os.WriteFile(p, []byte("started"), 0o644)
	}
}

// stageRig is a lease root, a fake seat and a fake PAIR ingress that records the frames posted to it.
type stageRig struct {
	cfg    string
	m      *gpulease.Manager
	swap   *orderedSwap
	marker string

	mu     sync.Mutex
	frames []map[string]any
}

func newStageRig(t *testing.T) *stageRig {
	t.Helper()
	r := &stageRig{swap: &orderedSwap{drainSwap: &drainSwap{}}}
	r.swap.loaded.Store(true)
	seat := httptest.NewServer(r.swap.handler("seat"))
	t.Cleanup(seat.Close)
	pair := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var f map[string]any
		_ = json.NewDecoder(req.Body).Decode(&f)
		r.mu.Lock()
		r.frames = append(r.frames, f)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(pair.Close)
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_PAIR_APPDIR", appDir)
	root := t.TempDir()
	r.cfg = filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(seat.URL) + `, "agent_model": "seat", "pair_workloads_enabled": true, "pair_workloads_endpoint": ` + strconv.Quote(pair.URL) + `}`
	if err := os.WriteFile(r.cfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	r.marker = filepath.Join(root, "command-started")
	t.Setenv("LO_HELPER_MARKER", r.marker)
	return r
}

// cardEnd is what the job's card was closed with: the method of its last frame, the error it carried and every
// method seen.
func (r *stageRig) cardEnd(t *testing.T) (method, errText string, methods []string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frames {
		m, _ := f["method"].(string)
		methods = append(methods, m)
		if m == "workload:errored" {
			info, _ := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
			errText, _ = info["error"].(string)
		}
		method = m
	}
	return method, errText, methods
}

func (r *stageRig) commandStarted() bool {
	_, err := os.Stat(r.marker)
	return err == nil
}

// signalAfter makes the wrapper receive sig d after it starts listening, the way a console close would reach it.
func signalAfter(t *testing.T, d time.Duration, sig os.Signal) {
	t.Helper()
	old := wrapperSignalFeed
	wrapperSignalFeed = func(c chan<- os.Signal) {
		time.Sleep(d)
		c <- sig
	}
	t.Cleanup(func() { wrapperSignalFeed = old })
}

// reserveEndedBySignal runs `gpu reserve` in-process with args and returns what it returned, said, and how long after
// the signal it took.
func reserveEndedBySignal(t *testing.T, args []string, signalMs time.Duration) (err error, stderr string, took time.Duration) {
	t.Helper()
	start := time.Now()
	stderr = captureStderr(t, func() { err = runGPUReserve(args) })
	return err, stderr, time.Since(start) - signalMs
}

func markerCmd() []string {
	return []string{"--", os.Args[0], "-test.run=^TestHelperWriteMarker$", "-test.timeout=30s"}
}

// THE QUEUE: a console close during a long --wait ends the wait, takes the waiter out of the line, closes the card
// with what happened, and never starts the command. The other holder keeps its lease.
func TestASignalWhileQueuedEndsTheWaitAndLeavesTheLine(t *testing.T) {
	for name, sig := range map[string]os.Signal{"a console close (SIGTERM)": syscall.SIGTERM, "Ctrl-C": os.Interrupt} {
		sig := sig
		t.Run(name, func(t *testing.T) {
			r := newStageRig(t)
			holder, err := r.m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "the other session bench", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()
			signalAfter(t, 400*time.Millisecond, sig)

			args := append([]string{"--config", r.cfg, "--wait", "20s", "--reason", "queued"}, markerCmd()...)
			err, stderr, took := reserveEndedBySignal(t, args, 400*time.Millisecond)
			if err == nil {
				t.Fatalf("a wait ended by a signal is not a success\n%s", stderr)
			}
			if took > 6*time.Second {
				t.Errorf("the wait ended %s after the signal; a console close gives the process about five seconds\n%s", took, stderr)
			}
			if r.commandStarted() {
				t.Error("the command must not start after the signal")
			}
			if n := len(r.m.Waiters()); n != 0 {
				t.Errorf("the waiter must leave the line, %d record(s) remain", n)
			}
			if info := r.m.Inspect(); !info.Held || info.Reason != "the other session bench" {
				t.Errorf("the signal must not touch the other holder's lease: %+v", info)
			}
			method, errText, methods := r.cardEnd(t)
			if method != "workload:errored" {
				t.Fatalf("the card must be closed (failed) with the cause, frames %v\n%s", methods, stderr)
			}
			want := "SIGTERM"
			if sig == os.Interrupt {
				want = "interrupted"
			}
			for _, w := range []string{want, "while queued", "never started"} {
				if !strings.Contains(errText, w) {
					t.Errorf("the card's error %q lacks %q", errText, w)
				}
			}
			if sig == syscall.SIGTERM {
				// The cause is said as likely, never as fact: a plain kill is the same signal.
				if !strings.Contains(errText, "likely") || strings.Contains(errText, "its console was closed") {
					t.Errorf("the card names the cause as likely, not as known: %q", errText)
				}
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the returned error %q lacks %q", err, want)
			}
		})
	}
}

// THE DRAIN: a signal while the wrapper waits for the seat to go idle releases the lease at once, closes the card, never
// unloads or warms the seat, and does not say the warm stays owed (nothing was unloaded, so none is).
func TestASignalDuringTheDrainReleasesTheLeaseAndOwesNoWarm(t *testing.T) {
	r := newStageRig(t)
	r.swap.inflight.Store(3) // a request that never ends: the drain cannot finish
	signalAfter(t, 500*time.Millisecond, syscall.SIGTERM)

	args := append([]string{"--config", r.cfg, "--wait", "20s", "--drain", "--unload-seat", "--reason", "drain"}, markerCmd()...)
	err, stderr, took := reserveEndedBySignal(t, args, 500*time.Millisecond)
	if err == nil {
		t.Fatalf("a drain ended by a signal is not a success\n%s", stderr)
	}
	if took > 8*time.Second {
		t.Errorf("the drain ended %s after the signal\n%s", took, stderr)
	}
	if info := r.m.Inspect(); info.Held {
		t.Errorf("the lease must be released on the signal: %+v", info)
	}
	if r.commandStarted() {
		t.Error("the command must not start after the signal")
	}
	if n, w := r.swap.unloads.Load(), r.swap.warms.Load(); n != 0 || w != 0 {
		t.Errorf("the drain never finished: no unload (%d) and no warm (%d) may happen", n, w)
	}
	if owed := r.m.SeatWarmOwed(); owed != "" {
		t.Errorf("nothing was unloaded, so no warm is owed: %q", owed)
	}
	if strings.Contains(stderr, "the warm stays owed") || strings.Contains(stderr, "NOT warming the seat back") {
		t.Errorf("the wrapper claims a warm-back it skipped when none was owed:\n%s", stderr)
	}
	method, errText, methods := r.cardEnd(t)
	if method != "workload:errored" {
		t.Fatalf("the card must be closed with the cause, frames %v", methods)
	}
	for _, w := range []string{"SIGTERM", "draining", "never started"} {
		if !strings.Contains(errText, w) {
			t.Errorf("the card's error %q lacks %q", errText, w)
		}
	}
}

// BEFORE THE LEASE IS ASKED FOR: a signal that arrives while the wrapper plans (before its card opens) stops it with
// nothing taken and no card to close.
func TestASignalBeforeTheLeaseIsAskedForTakesNothing(t *testing.T) {
	r := newStageRig(t)
	old := wrapperSignalFeed
	wrapperSignalFeed = func(c chan<- os.Signal) { c <- syscall.SIGTERM } // already pending when the wrapper looks
	t.Cleanup(func() { wrapperSignalFeed = old })

	args := append([]string{"--config", r.cfg, "--wait", "20s", "--reason", "early"}, markerCmd()...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "SIGTERM") {
		t.Fatalf("a pending signal must end the reserve naming it: %v\n%s", err, stderr)
	}
	if info := r.m.Inspect(); info.Held || r.commandStarted() {
		t.Errorf("nothing may be taken or started: held=%v started=%v", info.Held, r.commandStarted())
	}
	// No card was opened, so none needs closing: a card opened "queued" and failed in the same breath is noise on the
	// Jobs list for a job that never asked for anything.
	if _, _, methods := r.cardEnd(t); len(methods) != 0 {
		t.Errorf("a signal before the card opened must open no card, frames: %v", methods)
	}
}

// THE CARD CHOICE: `--cards N` waits inside the allocator (polling for cards that qualify, then queueing on a set), and
// a cancel ends both: the queue through the lease line's own cancel, the polling through the allocator's read.
func TestASignalWhileChoosingCardsEndsTheAllocation(t *testing.T) {
	for name, vram := range map[string]float64{
		"every card is held, so the request queues on a set": 0,
		"no card has the VRAM, so the allocator polls":        100,
	} {
		vram := vram
		t.Run(name, func(t *testing.T) {
			_, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "whole node", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()
			cancel := make(chan struct{})
			time.AfterFunc(300*time.Millisecond, func() { close(cancel) })
			build := func() (gpulease.AllocInput, error) {
				return buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{vramGiB: vram})
			}
			var out bytes.Buffer
			start := time.Now()
			lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "auto", TTL: time.Hour, Cancel: cancel}, oneCard,
				time.Minute, build, &out, time.Sleep, time.Now)
			if lease != nil {
				_ = lease.Release()
				t.Fatal("a cancelled allocation must take nothing")
			}
			if !errors.Is(err, gpulease.ErrCancelled) {
				t.Fatalf("want gpulease.ErrCancelled, got %v\n%s", err, out.String())
			}
			if d := time.Since(start); d > 8*time.Second {
				t.Errorf("the allocation ended %s after the cancel", d)
			}
			if n := len(m.Waiters()); n != 0 {
				t.Errorf("the waiter must leave the line, %d remain", n)
			}
		})
	}
}

// The watch itself: it records the first signal, closes Done once, wakes a sleep early, and a nil watch (the
// detached form) is inert.
func TestTheSignalWatchRecordsTheFirstSignalAndWakesASleep(t *testing.T) {
	var nilWatch *signalWatch
	if nilWatch.Done() != nil || nilWatch.Signal() != nil {
		t.Error("a nil watch has no channel and no signal")
	}
	nilWatch.Stop()
	nilWatch.Sleep(time.Millisecond)

	old := wrapperSignalFeed
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	t.Cleanup(func() { wrapperSignalFeed = old })
	w := watchWrapperSignals()
	defer w.Stop()
	<-ready
	select {
	case <-w.Done():
		t.Fatal("Done is closed before any signal")
	default:
	}
	woke := make(chan time.Duration, 1)
	go func() {
		s := time.Now()
		w.Sleep(time.Minute)
		woke <- time.Since(s)
	}()
	time.Sleep(50 * time.Millisecond)
	feed <- syscall.SIGTERM
	select {
	case d := <-woke:
		if d > 5*time.Second {
			t.Errorf("a sleep must wake on the signal, took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sleep did not wake on the signal")
	}
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done is not closed after the signal")
	}
	if w.Signal() != syscall.SIGTERM {
		t.Errorf("Signal = %v, want SIGTERM", w.Signal())
	}
	w.Stop()
	w.Stop() // idempotent
}
