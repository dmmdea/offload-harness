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

// drainSwap is a llama-swap stand-in: /running lists the seat when loaded, and
// /upstream/<model>/metrics serves a vLLM-shaped exposition with the in-flight
// count the test controls. It also records whether the upstream path was ever
// touched while the seat was NOT loaded — the one thing a drain must never do.
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
}

func (f *drainSwap) handler(model string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if f.loaded.Load() {
			running = append(running, map[string]string{"model": model, "state": "ready"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	mux.HandleFunc("/upstream/"+model+"/slots", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/upstream/"+model+"/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
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

func TestParseInflightSumsOnlyTheInflightGauges(t *testing.T) {
	body := `# HELP
vllm:num_requests_running{engine="0",model_name="m"} 2.0
vllm:num_requests_waiting{engine="0",model_name="m"} 1.0
vllm:num_requests_running_total 500
llamacpp:requests_processing 1
vllm:prompt_tokens_total 12345
`
	if got := parseInflight(strings.NewReader(body)); got != 4 {
		t.Fatalf("inflight = %d, want 4 (2 running + 1 waiting + 1 processing; the _total counter must not count)", got)
	}
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

func TestParseSlotsInflightCountsProcessingSlots(t *testing.T) {
	n, err := parseSlotsInflight(strings.NewReader(`[{"id":0,"is_processing":true,"n_ctx":65536},{"id":1,"is_processing":false},{"id":2,"is_processing":true}]`))
	if err != nil || n != 2 {
		t.Fatalf("parse = %d, %v; want 2 processing", n, err)
	}
	if _, err := parseSlotsInflight(strings.NewReader(`{"error":"x"}`)); err == nil {
		t.Fatal("a non-array body must be an error, never zero in flight")
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
