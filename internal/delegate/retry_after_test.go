// retry_after_test.go: item 7 (PR-5, register D-105/D-106) — a dispatch 503
// carrying its own Retry-After header is a CAPACITY refusal the delegator
// waits out and retries on the SAME node once, rather than immediately
// spending a re-placement on a different node. The retry must not mark the
// node `tried` from placeAndRun's perspective: it never surfaces as a
// discrete refusal at all when it succeeds.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// retryAfterNode serves /fleet/health (agent-enabled, resident), refuses the
// FIRST /fleet/dispatch with 503 + Retry-After: 1, accepts every dispatch
// after that, and answers every poll "done" at once.
func retryAfterNode(t *testing.T) (dispatches *atomic.Int64, url string) {
	t.Helper()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-retry-after", "agent_enabled": true, "agent_seat": "remote-seat",
			"agent_seat_resident": true, "agent_ctx_tokens": 32768, "queue_depth": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			JobID string `json:"job_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "queue full"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-retry-after", Seat: "remote-seat",
			Output: "qube from retry-after", Structured: json.RawMessage(`{"answer":"qube"}`), StopReason: "done"}
		data, _ := json.Marshal(wire)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "done", "data": json.RawMessage(data), "job_id": r.PathValue("id")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &n, srv.URL
}

// TestRun503RetryAfterLandsOnTheSameNode: the job lands on the node that
// refused with Retry-After (not re-placed elsewhere), and placement_reason
// says so.
func TestRun503RetryAfterLandsOnTheSameNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	dispatches, url := retryAfterNode(t)

	start := time.Now()
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %s, want the Retry-After: 1 wait to have been honored (>= ~1s)", elapsed)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the retry to have succeeded", sum)
	}
	pr := results[0]
	if pr.Node != "node-retry-after" {
		t.Fatalf("Node = %q, want node-retry-after (the SAME node that refused)", pr.Node)
	}
	if pr.Replacements != 0 {
		t.Fatalf("Replacements = %d, want 0 — a 503 honored via Retry-After must never be treated as a re-placement", pr.Replacements)
	}
	if !strings.Contains(pr.PlacementReason, "Retry-After") {
		t.Fatalf("PlacementReason = %q, want it to say the Retry-After wait was honored", pr.PlacementReason)
	}
	if dispatches.Load() != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (the refusal, then the honored retry)", dispatches.Load())
	}
}

// TestPollOnceSendsTheWaitParameter pins the wire shape: pollOnce's GET
// carries ?wait=<pollWaitSec>.
func TestPollOnceSendsTheWaitParameter(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "running"})
	}))
	defer srv.Close()
	r := &runner{cfg: testCfg(t)}
	if _, err := r.pollOnce(context.Background(), srv.URL, "job-1"); err != nil {
		t.Fatal(err)
	}
	if sawQuery != "wait=12" {
		t.Fatalf("poll query = %q, want wait=12", sawQuery)
	}
}
