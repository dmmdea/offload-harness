package fleetview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func fakeNode(t *testing.T, util int) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "n1", "vram_total_gb": 16.0, "vram_free_gb": 9.5,
			"gpu_util_pct": util, "gpu_util_known": true, "host_cpu_pct": 12,
			"host_ram_used_gb": 20.0, "host_ram_total_gb": 64.0,
			"agent_enabled": true, "agent_seat": "qwen3.5-9b-agent", "agent_seat_resident": true,
			"served_models": []string{"qwen3.5-9b-agent"}, "queue_depth": 1, "jobs_running": 1,
		})
	})
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"id": "agd-1", "task": "agent-run", "state": "error", "error": "boom", "accepted_at": time.Now().Unix()},
		}})
	})
	return httptest.NewServer(mux)
}

func TestPollerFoldsHealthJobsAndErrors(t *testing.T) {
	srv := fakeNode(t, 42)
	defer srv.Close()
	p := NewPoller(config.Config{AgentDelegationEnabled: true}, []string{srv.URL}, 30*time.Millisecond, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go p.Run(ctx)
	for {
		ov := p.Snapshot()
		if len(ov.Nodes) == 1 && ov.Nodes[0].Reachable && len(ov.Nodes[0].History) >= 2 {
			n := ov.Nodes[0]
			if n.GpuUtil != 42 || n.HostCPU != 12 || n.ServedModels[0] != "qwen3.5-9b-agent" || len(n.Jobs) != 1 {
				t.Fatalf("node: %+v", n)
			}
			if len(ov.Errors) == 0 || ov.Errors[0].Source != "job" || ov.Errors[0].Node != "n1" {
				t.Fatalf("errors: %+v", ov.Errors)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never folded: %+v", p.Snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestPollerUnreachableIsAnError(t *testing.T) {
	p := NewPoller(config.Config{}, []string{"http://127.0.0.1:1"}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	ov := p.Snapshot()
	if ov.Nodes[0].Reachable || len(ov.Errors) == 0 || ov.Errors[0].Source != "probe" {
		t.Fatalf("%+v", ov)
	}
}

// TestSnapshotIsADeepCopy guards against Snapshot() handing out maps that
// are still shared with the poller's own live state: fold assigns fresh
// []map[string]any slices into Node.Devices/Jobs each tick, but the map
// VALUES inside them were, before this fix, the same objects Snapshot
// returned — a caller mutating a returned map raced with the next fold.
func TestSnapshotIsADeepCopy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id":     "n1",
			"gpu_devices": []map[string]any{{"name": "gpu0"}},
		})
	})
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"id": "agd-1", "task": "t", "state": "ok"},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewPoller(config.Config{}, []string{srv.URL}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go p.Run(ctx)

	var ov1 Overview
	for {
		ov1 = p.Snapshot()
		if len(ov1.Nodes) == 1 && len(ov1.Nodes[0].Jobs) == 1 && len(ov1.Nodes[0].Devices) == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never populated: %+v", p.Snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}

	ov1.Nodes[0].Jobs[0]["state"] = "mutated"
	ov1.Nodes[0].Devices[0]["name"] = "mutated"

	ov2 := p.Snapshot()
	if ov2.Nodes[0].Jobs[0]["state"] == "mutated" {
		t.Fatalf("Jobs map shared with poller state: %+v", ov2.Nodes[0].Jobs[0])
	}
	if ov2.Nodes[0].Devices[0]["name"] == "mutated" {
		t.Fatalf("Devices map shared with poller state: %+v", ov2.Nodes[0].Devices[0])
	}
}

// TestSeenErrPrunedAfterJobClears verifies seenErr does not grow without
// bound: once a job that produced an Error drops out of the node's current
// jobs listing, its dedupe key is pruned — while the Error it already
// recorded stays in Errors.
func TestSeenErrPrunedAfterJobClears(t *testing.T) {
	var tick int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "n1"})
	})
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&tick, 1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
				{"id": "agd-1", "task": "t", "state": "error", "error": "boom"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewPoller(config.Config{}, []string{srv.URL}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go p.Run(ctx)

	deadline := time.Now().Add(800 * time.Millisecond)
	var present bool
	var errCount int
	for time.Now().Before(deadline) {
		p.mu.RLock()
		_, present = p.seenErr["n1|agd-1"]
		errCount = len(p.errors)
		p.mu.RUnlock()
		if !present && errCount > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if present {
		t.Fatalf("seenErr key n1|agd-1 not pruned after job cleared")
	}
	if errCount == 0 {
		t.Fatalf("recorded error was lost, want it retained in errors")
	}
}

func TestHistoryIsBounded(t *testing.T) {
	srv := fakeNode(t, 1)
	defer srv.Close()
	p := NewPoller(config.Config{}, []string{srv.URL}, 5*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if h := len(p.Snapshot().Nodes[0].History); h != 3 {
		t.Fatalf("history len %d want 3", h)
	}
}
