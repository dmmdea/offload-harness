package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A warm-back whose health request llama-swap answers 5xx (its own health wait
// ran out before a long cold load finished) watches the seat's state: a load in
// progress is waited out and a ready seat is a warm that succeeded; a seat that
// never started is the failure (2026-09-18 19:5x, the 3-card seat).
func TestWarmWatchesALoadThatOutlastsLlamaSwapsHealthWait(t *testing.T) {
	old, oldEvery := warmWatch, warmWatchEvery
	warmWatch, warmWatchEvery = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { warmWatch, warmWatchEvery = old, oldEvery })
	var state atomic.Value // "", "starting", "ready"
	state.Store("starting")
	mux := http.NewServeMux()
	mux.HandleFunc("/upstream/seat/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if s := state.Load().(string); s != "" {
			running = append(running, map[string]string{"model": "seat", "state": s})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	mux.HandleFunc("/upstream/seat/metrics", func(w http.ResponseWriter, r *http.Request) {
		t.Error("the warm watch read the seat's gauge through /upstream; it needs /running only")
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	go func() {
		time.Sleep(120 * time.Millisecond)
		state.Store("ready")
	}()
	start := time.Now()
	if err := warmSeat(context.Background(), srv.Client(), srv.URL, "seat"); err != nil {
		t.Fatalf("a load that finishes must be a successful warm: %v", err)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("the warm must wait for the load, took %s", took)
	}
	// Nothing loading after the 5xx: the failure, at once.
	state.Store("")
	start = time.Now()
	err := warmSeat(context.Background(), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "not loading") {
		t.Fatalf("a 5xx with no load in progress must fail as such, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("a seat that is not loading must fail without waiting out the watch")
	}
	// A load that never becomes ready fails at the watch bound.
	state.Store("starting")
	warmWatch = 150 * time.Millisecond
	err = warmSeat(context.Background(), srv.Client(), srv.URL, "seat")
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("a load that never finishes must fail at the watch bound, got %v", err)
	}
}
