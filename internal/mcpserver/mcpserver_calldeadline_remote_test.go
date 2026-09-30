// The whole-call deadline through the door, against a fleet node (ADR 0065).
//
// The engine-side tests fake the node inside package delegate; this one drives
// the real agent_delegate handler at a scripted fleet node over HTTP, so the
// door's deadline, the engine's cancellation and the best-effort withdraw are
// proved together — the PR-6 acceptance "cancels and withdraws outstanding jobs".

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// fleetNodeStub is a scripted fleet node: health advertises the agent lane,
// dispatch acks and remembers each job's goal, a "fast" job finishes at once,
// every other job stays `accepted` (queued in the node's backlog, never started) for
// as long as it is polled, and DELETE /fleet/jobs/{id} records who asked to withdraw
// what and confirms it, as a node does for a job it has not started. A job last seen
// `running` is never asked to be withdrawn (ADR 0064), so the slow job must be queued.
type fleetNodeStub struct {
	goals sync.Map // job id -> goal

	mu        sync.Mutex
	withdrawn []string
}

func (n *fleetNodeStub) withdrawals() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.withdrawn...)
}

func newFleetNodeStub(t *testing.T) (*fleetNodeStub, string) {
	t.Helper()
	n := &fleetNodeStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-a", "queue_depth": 0, "agent_seat": "remote-seat", "agent_ctx_tokens": 32768,
			"agent_seat_resident": true, "agent_enabled": true, "jobs_queued": 0, "jobs_running": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			JobID   string          `json:"job_id"`
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		var c struct {
			Goal string `json:"goal"`
		}
		_ = json.Unmarshal(env.Payload, &c)
		n.goals.Store(env.JobID, c.Goal)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		goal, _ := n.goals.Load(id)
		if g, _ := goal.(string); strings.Contains(g, "fast") {
			wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-a", Seat: "remote-seat",
				Output: "done on the node", Structured: json.RawMessage(`{"answer":"ok"}`), StopReason: "done"})
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": id, "state": "done", "data": json.RawMessage(wire)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": id, "state": "accepted"})
	})
	mux.HandleFunc("DELETE /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.withdrawn = append(n.withdrawn, r.PathValue("id"))
		n.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "withdrawn", "withdrawn": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return n, srv.URL
}

// TestCallDeadlineCancelsAndWithdrawsOutstandingJobs: through agent_delegate, two
// subtasks are placed on a fleet node; one finishes, the other is still queued on
// it when the call deadline passes. The call returns at the deadline with the
// finished result and a call-deadline defer that names the node and the job, the
// polling ends, and the node is asked once to withdraw exactly the outstanding
// job (and says so: the reason carries its confirmation).
func TestCallDeadlineCancelsAndWithdrawsOutstandingJobs(t *testing.T) {
	node, url := newFleetNodeStub(t)
	s := deadlineServer(t, 1, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the local seat ran although both subtasks were placed on the fleet node")
		return core.AgentWireResult{}, nil
	})
	schema := map[string]any{"properties": map[string]any{"answer": map[string]any{"type": "string"}}}
	args, _ := json.Marshal(map[string]any{
		"subtasks": []any{
			map[string]any{"goal": "fast one", "output_schema": schema},
			map[string]any{"goal": "slow one", "output_schema": schema},
		},
		"route": "remote", "remotes": []string{url},
	})

	res, elapsed := callWithin(t, 8*time.Second, s.handleAgentDelegate, string(args))

	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("the call returned after %s, want about the 1s deadline", elapsed)
	}
	if res.IsError {
		t.Fatal("IsError = true: a finished result plus one call-deadline defer is a delivered call")
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["succeeded"] != float64(1) || summary["deferred"] != float64(1) || summary["failed"] != float64(0) {
		t.Fatalf("summary = %v, want the finished job and one call-deadline defer", summary)
	}
	results, _ := m["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both subtasks", m["results"])
	}
	fast, _ := results[0].(map[string]any)
	if fast["node"] != "node-a" || fast["deferred"] == true {
		t.Fatalf("the finished subtask = %v, want the node's own result", fast)
	}
	slow, _ := results[1].(map[string]any)
	reason, _ := slow["reason"].(string)
	jobID, _ := slow["job_id"].(string)
	if slow["deferred"] != true || slow["defer_class"] != core.DeferClassBudget || !strings.HasPrefix(reason, "call deadline reached; 1 unfinished") {
		t.Fatalf("the outstanding subtask = %v, want a budget defer reading %q", slow, "call deadline reached; 1 unfinished")
	}
	if !strings.HasPrefix(jobID, "agd-") || !strings.Contains(reason, jobID) || !strings.Contains(reason, "node-a") {
		t.Fatalf("reason %q should name node-a and the job %q so the caller can reconcile it", reason, jobID)
	}

	// The node was asked to withdraw exactly the outstanding job, once — never the
	// finished one. (The ask is made inside the unwind, before the call returns.)
	got := node.withdrawals()
	if len(got) != 1 || got[0] != jobID {
		t.Fatalf("the node was asked to withdraw %v, want exactly the outstanding job [%s]", got, jobID)
	}
	if !strings.Contains(reason, "the node confirmed it took the job back") {
		t.Fatalf("reason %q should carry the node's confirmation of the withdraw", reason)
	}
}
