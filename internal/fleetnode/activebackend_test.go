package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/backendchain"
	"github.com/dmmdea/offload-harness/internal/config"
)

// TestHealthPayloadActiveBackendIsAdditive: the two new keys appear only when set, and `backends` (the
// hardware tags of ADR 0054) keeps its meaning and its place.
func TestHealthPayloadActiveBackendIsAdditive(t *testing.T) {
	b, _ := json.Marshal(healthPayload{NodeID: "n"})
	for _, k := range []string{`"lane_backends"`, `"seat_backends"`, `"active_backend"`} {
		if strings.Contains(string(b), k) {
			t.Fatalf("an unset report must add no key, found %s in %s", k, b)
		}
	}
	e := backendchain.Entry{Lane: "agent", Seat: "s", ActiveBackend: "vllm", BackendChain: []string{"vllm", "llama.cpp"}, BackendProbe: "endpoint", BackendReason: "r"}
	b, _ = json.Marshal(healthPayload{NodeID: "n", Backends: []string{"vulkan", "cpu"}, LaneBackends: []backendchain.Entry{e}, SeatBackends: []backendchain.Entry{e}})
	got := string(b)
	for _, want := range []string{`"backends":["vulkan","cpu"]`, `"lane_backends":[{"lane":"agent"`, `"seat_backends":[{"lane":"agent"`, `"active_backend":"vllm"`, `"backend_chain":["vllm","llama.cpp"]`, `"backend_probe":"endpoint"`, `"backend_reason":"r"`} {
		if !strings.Contains(got, want) {
			t.Errorf("payload %s lacks %s", got, want)
		}
	}
}

func backendHealth(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status %d: %s", rec.Code, rec.Body.String())
	}
	return decodeMap(t, rec)
}

func waitForHealthKey(t *testing.T, s *Server, key string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m := backendHealth(t, s)
		if _, ok := m[key]; ok {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("health never published %s", key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHealthPublishesActiveBackendsFromACachedRead: health never waits on the collection. The first
// request kicks one background read and answers without the keys (unknown, never "none"); once the read
// lands, the keys appear; and a burst of requests inside the TTL is one read, not one per request.
func TestHealthPublishesActiveBackendsFromACachedRead(t *testing.T) {
	cfg := config.Config{Endpoint: "http://llama-swap.invalid"}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	release := make(chan struct{})
	var calls atomic.Int32
	s.collectBackends = func(ctx context.Context, c config.Config) backendchain.Report {
		calls.Add(1)
		<-release
		return backendchain.Report{
			Lanes: []backendchain.Entry{{Lane: "agent", Seat: "x", ActiveBackend: "llama.cpp", BackendChain: []string{"llama.cpp"}, BackendProbe: "process", BackendReason: "r"}},
			Seats: []backendchain.Entry{{Seat: "x", ActiveBackend: "llama.cpp", BackendChain: []string{"llama.cpp"}, BackendProbe: "process", BackendReason: "r"}},
		}
	}
	start := time.Now()
	m := backendHealth(t, s)
	if time.Since(start) > time.Second {
		t.Fatalf("health waited %s on the collection", time.Since(start))
	}
	if _, ok := m["lane_backends"]; ok {
		t.Fatalf("a cold cache must publish nothing, got %v", m["lane_backends"])
	}
	for i := 0; i < 5; i++ {
		backendHealth(t, s)
	}
	close(release)
	m = waitForHealthKey(t, s, "lane_backends")
	lanes := m["lane_backends"].([]any)
	first := lanes[0].(map[string]any)
	if first["lane"] != "agent" || first["active_backend"] != "llama.cpp" {
		t.Errorf("lane_backends = %v", lanes)
	}
	if _, ok := m["seat_backends"]; !ok {
		t.Errorf("seat_backends missing: %v", m)
	}
	for i := 0; i < 5; i++ {
		backendHealth(t, s)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d collections for one TTL window, want 1", n)
	}
}

// TestHealthPublishesNothingWithoutAnEndpoint: no llama-swap endpoint, no collection.
func TestHealthPublishesNothingWithoutAnEndpoint(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, nil)
	s.collectBackends = func(context.Context, config.Config) backendchain.Report {
		t.Error("the collector ran on a node with no llama-swap endpoint")
		return backendchain.Report{}
	}
	for i := 0; i < 3; i++ {
		m := backendHealth(t, s)
		if _, ok := m["lane_backends"]; ok {
			t.Fatal("published without an endpoint")
		}
	}
	time.Sleep(50 * time.Millisecond)
}

// TestHealthRefreshNeverLoadsAModel: the real collector, run by the real health path against a
// llama-swap that FAILS THE TEST on a completion, an /upstream path or any POST. A green run proves the
// /fleet/health refresh loads nothing and never completes.
func TestHealthRefreshNeverLoadsAModel(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"offload-e4b","object":"model"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/running":
			fmt.Fprint(w, `{"running":[]}`)
		default:
			t.Errorf("forbidden request to llama-swap from the health refresh: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	m := waitForHealthKey(t, s, "lane_backends")
	var workhorse map[string]any
	for _, l := range m["lane_backends"].([]any) {
		if row := l.(map[string]any); row["lane"] == "workhorse" {
			workhorse = row
		}
	}
	if workhorse == nil || workhorse["active_backend"] == nil || workhorse["backend_probe"] != "process" {
		t.Errorf("workhorse lane = %v", workhorse)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range seen {
		if r != "GET /v1/models" && r != "GET /running" {
			t.Errorf("unexpected request %q", r)
		}
	}
}
