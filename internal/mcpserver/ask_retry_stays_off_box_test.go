// offload_ask's cross-seat retry must not come back to this box.
//
// askRetry hands a failed first attempt to the fleet at route remote, which places the retry on a
// fleet node. Inside that call the delegator has a retry of its own: a node's answer that fails
// the contract's acceptance is retried once, and for a bare route remote that retry falls back to
// this box's seat. The ask's first attempt already ran there and failed, so the ask's retry paid
// this box's GPU a third run on the same goal while its note still said "retry on <node>".
//
// These tests drive the real delegator (s.reviewFleet nil) at a scripted fleet node, because a
// stubbed fleet seam replaces the very engine whose retry is the defect.

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// wrongAnswerNode is a fleet node whose agent seat is up and eligible and whose every job is
// already done at the first poll with a pair that cites nothing from the file, so the ask's
// grounding check fails on it as it does on the local seat. dispatches counts the jobs it took.
type wrongAnswerNode struct {
	dispatches atomic.Int64
	url        string
}

func newWrongAnswerNode(t *testing.T) *wrongAnswerNode {
	t.Helper()
	n := &wrongAnswerNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-a", "queue_depth": 0, "agent_seat": "remote-seat", "agent_ctx_tokens": 32768,
			"agent_seat_resident": true, "agent_enabled": true, "jobs_queued": 0, "jobs_running": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			JobID string `json:"job_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		n.dispatches.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-a", Seat: "remote-seat",
			Steps: 1, StopReason: "done", Output: "maybe 33",
			Structured: json.RawMessage(`{"answer":"maybe 33","evidence":"somewhere in the config"}`)})
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(wire)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	n.url = srv.URL
	return n
}

// TestOffloadAskRetryNeverReturnsToThisBoxWhenTheFleetNodeAlsoFails: the local seat answers wrong,
// the ask retries on the one fleet node, and that node answers wrong too. The delegator has no
// other node to try, and the only place left is this box's seat, which the ask's retry exists to
// move away from. The seat runs once, the fleet node runs once, and the note says the fleet's
// own retry was skipped.
func TestOffloadAskRetryNeverReturnsToThisBoxWhenTheFleetNodeAlsoFails(t *testing.T) {
	node := newWrongAnswerNode(t)
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = t.TempDir()
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.DelegateRemotes = []string{node.url}
	clock := &retryClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	var localCalls atomic.Int64
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.askNow = clock.now
	s.localAgent = func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls.Add(1)
		clock.at = clock.at.Add(20 * time.Second)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "local-seat", Steps: 2, StopReason: "done",
			Output:     "I think the cap is probably 32.",
			Structured: json.RawMessage(`{"answer":"probably 32","evidence":"it is in the config"}`)}, nil
	}
	out := askRetryOnce(t, s, "what is the queue cap")

	if got := localCalls.Load(); got != 1 {
		t.Fatalf("this box's seat ran %d times, want 1: the ask's retry came back to the box the first attempt failed on: %v", got, out)
	}
	if got := node.dispatches.Load(); got != 1 {
		t.Fatalf("the fleet node took %d jobs, want the one retry: %v", got, out)
	}
	if out["verified"] != false || out["answer"] != "probably 32" || out["seat"] != "local-seat" {
		t.Fatalf("both attempts failed, so the first answer stands unverified: %v", out)
	}
	if out["retried_on"] != "node-a" {
		t.Fatalf("retried_on = %v, want the node that ran the retry: %v", out["retried_on"], out)
	}
	note, _ := out["retry_note"].(string)
	if !strings.HasPrefix(note, "retry on node-a also failed_verification: [") || !strings.Contains(note, "the fleet's own retry: retry skipped: ") {
		t.Fatalf("retry_note = %q, want the node's failure and the fleet's own skipped retry", note)
	}
	// The retry stays off this box without claiming a pin: the tally still counts the call as unreasoned.
	st := s.pins.Snapshot()
	reasoned := 0
	for _, n := range st.Reasoned {
		reasoned += n
	}
	if st.Unreasoned != 1 || reasoned != 0 || st.Hints != 0 {
		t.Fatalf("pin tally = %+v, want the one retry counted as unreasoned and nothing reasoned", st)
	}
}
