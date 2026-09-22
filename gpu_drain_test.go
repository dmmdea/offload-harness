package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drainSwap is a llama-swap stand-in: /running lists the seat when loaded, with
// the seat's own address as `proxy`, where /metrics serves a vLLM-shaped
// exposition with the in-flight count the test controls (the gauge is read at
// the seat, never through /upstream, which resets llama-swap's idle timer). It
// also records whether the seat was ever asked while NOT loaded — the one
// thing a drain must never do.
type drainSwap struct {
	loaded                    atomic.Bool
	inflight                  atomic.Int64
	upstreamHitsWhileUnloaded atomic.Int64
	unloads                   atomic.Int64
	warms                     atomic.Int64
	// metricsStatus, when non-zero, is what /metrics answers instead of an
	// exposition — 501 models a llama-server started without --metrics; /slots
	// then serves `inflight` processing slots out of two (0.113.19).
	metricsStatus atomic.Int64
	slotsHits     atomic.Int64
	// starting lists the loaded seat as `starting` (a load in progress);
	// upstreamHitsWhileStarting counts upstream reads in that state — a real
	// llama-swap would hold each one for the whole load (register D-92).
	starting                  atomic.Bool
	upstreamHitsWhileStarting atomic.Int64
}

func (f *drainSwap) handler(model string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if f.loaded.Load() {
			state := "ready"
			if f.starting.Load() {
				state = "starting"
			}
			running = append(running, map[string]string{"model": model, "state": state, "proxy": "http://" + r.Host + "/direct/" + model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	mux.HandleFunc("/direct/"+model+"/slots", func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		f.slotsHits.Add(1)
		n := f.inflight.Load()
		slots := []map[string]any{}
		for i := 0; i < 2; i++ {
			slots = append(slots, map[string]any{"id": i, "n_ctx": 65536, "is_processing": int64(i) < n, "id_task": -1})
		}
		_ = json.NewEncoder(w).Encode(slots)
	})
	mux.HandleFunc("/direct/"+model+"/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		if f.starting.Load() {
			f.upstreamHitsWhileStarting.Add(1)
		}
		if st := f.metricsStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		n := f.inflight.Load()
		_, _ = w.Write([]byte("# HELP vllm:num_requests_running x\nvllm:num_requests_running{engine=\"0\"} " +
			strconv.FormatInt(n, 10) + "\nvllm:num_requests_waiting{engine=\"0\"} 0.0\nvllm:num_requests_running_total 99\n"))
	})
	mux.HandleFunc("/upstream/"+model+"/health", func(w http.ResponseWriter, r *http.Request) {
		f.warms.Add(1)
		f.loaded.Store(true)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api/models/unload/"+model, func(w http.ResponseWriter, r *http.Request) {
		f.unloads.Add(1)
		f.loaded.Store(false)
		w.WriteHeader(200)
	})
	return mux
}

func TestDrainReturnsAtOnceWhenTheSeatIsNotLoadedAndNeverTouchesUpstream(t *testing.T) {
	f := &drainSwap{}
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	if err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", time.Second, time.Millisecond, nil); err != nil {
		t.Fatalf("drain of an unloaded seat: %v", err)
	}
	if f.upstreamHitsWhileUnloaded.Load() != 0 {
		t.Fatal("drain probed /upstream on an unloaded seat — that path LOADS the model")
	}
}

func TestDrainWaitsForInflightToReachZeroTwice(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(3)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.inflight.Store(0)
	}()
	start := time.Now()
	if err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 2*time.Second, 5*time.Millisecond, nil); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("drain returned while requests were still in flight")
	}
}

// TestDrainFallsBackToSlotsWhenTheSeatHasNoMetrics is the 2026-09-06 Lenovo
// case: a warm llama.cpp seat without --metrics answers /metrics 501, and the
// drain timed out on it. It now reads /slots: busy while a slot is processing,
// drained once two consecutive reads see none.
func TestDrainFallsBackToSlotsWhenTheSeatHasNoMetrics(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	f.metricsStatus.Store(http.StatusNotImplemented)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.inflight.Store(0)
	}()
	start := time.Now()
	if err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 2*time.Second, 5*time.Millisecond, nil); err != nil {
		t.Fatalf("drain via /slots: %v", err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("drain returned while a slot was still processing")
	}
	if f.slotsHits.Load() < 2 {
		t.Fatalf("/slots read %d time(s), want at least the two idle reads", f.slotsHits.Load())
	}
}

// TestDrainDoesNotFallBackOnAMetricsServerError is the control arm: a 500 from
// /metrics is "could not read", never idle — no /slots read, the drain fails.
func TestDrainDoesNotFallBackOnAMetricsServerError(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(0)
	f.metricsStatus.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 40*time.Millisecond, 5*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("a 500 must fail the drain naming the status, got %v", err)
	}
	if f.slotsHits.Load() != 0 {
		t.Fatalf("/slots was read %d time(s) after a 500; only 501/404 fall back", f.slotsHits.Load())
	}
}

func TestDrainErrorsAtTheDeadlineWhileBusy(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 40*time.Millisecond, 5*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "1 in flight") {
		t.Fatalf("busy seat must fail the drain naming what it saw, got %v", err)
	}
}

func TestUnloadAndWarmGoThroughLlamaSwap(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	if err := unloadSeat(context.Background(), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatal(err)
	}
	if f.loaded.Load() || f.unloads.Load() != 1 {
		t.Fatalf("unload did not go through: loaded=%v unloads=%d", f.loaded.Load(), f.unloads.Load())
	}
	if err := warmSeat(context.Background(), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatal(err)
	}
	if !f.loaded.Load() || f.warms.Load() != 1 {
		t.Fatalf("warm did not go through: loaded=%v warms=%d", f.loaded.Load(), f.warms.Load())
	}
}

// aliasSwapNoRoster lists the seat in /running under its CANONICAL id only and
// cannot serve /v1/models — the reading for the ALIAS is then ambiguous.
func aliasSwapNoRoster(id string, listed bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusBadGateway) })
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		if listed {
			_, _ = w.Write([]byte(`{"running":[{"model":"` + id + `","state":"ready"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"running":[]}`))
	})
	return mux
}

// TestDrainRefusesToTrustAnAmbiguousReading is the review finding on 0.113.20:
// an unreadable roster used to turn an alias-bound, mid-request seat into "not
// loaded: nothing to drain". Now the drain keeps polling and fails at the
// deadline naming the roster — never a silent "drained".
func TestDrainRefusesToTrustAnAmbiguousReading(t *testing.T) {
	srv := httptest.NewServer(aliasSwapNoRoster("qwen3.8-27b-vllm", true))
	defer srv.Close()
	err := drainSeat(context.Background(), srv.Client(), srv.URL, "agent-pool", 40*time.Millisecond, 5*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "roster unreadable") {
		t.Fatalf("an ambiguous reading must fail the drain naming the roster, got %v", err)
	}
}

// TestDrainStillReturnsAtOnceWhenNothingIsRunning is the control arm: the same
// unreadable roster with an EMPTY /running is plainly idle — no regression for
// an unloaded seat behind a llama-swap that cannot serve its roster.
func TestDrainStillReturnsAtOnceWhenNothingIsRunning(t *testing.T) {
	srv := httptest.NewServer(aliasSwapNoRoster("qwen3.8-27b-vllm", false))
	defer srv.Close()
	if err := drainSeat(context.Background(), srv.Client(), srv.URL, "agent-pool", time.Second, time.Millisecond, nil); err != nil {
		t.Fatalf("empty /running must drain at once, got %v", err)
	}
}

// TestDrainWaitsThroughAStartingSeatWithoutTouchingTheUpstream (register
// D-92): another caller's contract triggered a cold load just before the lease;
// llama-swap lists the seat as `starting` and would hold any /upstream read
// for the whole load. The drain must poll /running until the seat is ready and
// only then read the in-flight count.
func TestDrainWaitsThroughAStartingSeatWithoutTouchingTheUpstream(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.starting.Store(true)
	f.inflight.Store(0)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	go func() {
		time.Sleep(40 * time.Millisecond)
		f.starting.Store(false) // the engine came up; nothing is queued on it
	}()
	var out strings.Builder
	start := time.Now()
	if err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 2*time.Second, 5*time.Millisecond, &out); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("drain returned while the seat was still starting")
	}
	if f.upstreamHitsWhileStarting.Load() != 0 {
		t.Fatalf("drain read /upstream %d time(s) while the seat was starting — llama-swap holds that request for the whole load", f.upstreamHitsWhileStarting.Load())
	}
	if !strings.Contains(out.String(), "a load is in progress") {
		t.Fatalf("drain output must name the load in progress; got %q", out.String())
	}
}

// TestDrainErrorsAtTheDeadlineWhileStarting: a load that outlasts the drain
// window is an error that names the state, never a drained verdict.
func TestDrainErrorsAtTheDeadlineWhileStarting(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.starting.Store(true)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	err := drainSeat(context.Background(), srv.Client(), srv.URL, "seat", 30*time.Millisecond, 5*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "seat starting") {
		t.Fatalf("drain of a seat that never finished starting: err = %v, want a deadline error naming the starting state", err)
	}
	if f.upstreamHitsWhileStarting.Load() != 0 {
		t.Fatal("drain read /upstream while the seat was starting")
	}
}
