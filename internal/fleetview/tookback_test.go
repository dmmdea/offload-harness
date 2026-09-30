package fleetview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// A job a node took out of its backlog without running it is a terminal `error`
// record by design (ADR 0064): "withdrawn: ..." when its delegator gave it up,
// "reaped: ..." when nobody polled it within the poll lease. Both are the cleanup
// working, at a rate of dozens a day on a busy node, so neither is an operator
// event — listed as one they would fill the 200-entry ring with non-failures and
// push the real ones out.

func tookBackNode(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "n1", "agent_enabled": true})
	})
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"id": "agd-real", "task": "agent", "state": "error", "error": "stalled: no progress for 300s in prefill", "accepted_at": now},
			{"id": "agd-withdrawn", "task": "agent", "state": "error", "error": fleetnode.ErrWithdrawn, "accepted_at": now},
			{"id": "agd-reaped", "task": "agent", "state": "error", "error": fleetnode.ErrReaped, "accepted_at": now},
			{"id": "agd-shutdown", "task": "agent", "state": "error", "error": fleetnode.ErrInterrupted, "accepted_at": now},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPollerDoesNotListAJobTheNodeTookBackAsAnOperatorError: the failures stay,
// the cleanup does not.
func TestPollerDoesNotListAJobTheNodeTookBackAsAnOperatorError(t *testing.T) {
	srv := tookBackNode(t)
	p := NewPoller(config.Config{}, []string{srv.URL}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Run(ctx)
	for {
		ov := p.Snapshot()
		if len(ov.Nodes) == 1 && ov.Nodes[0].Reachable && len(ov.Nodes[0].Jobs) == 4 {
			var got []string
			for _, e := range ov.Errors {
				if e.Source == "job" {
					got = append(got, e.Message)
				}
			}
			if len(got) != 2 {
				t.Fatalf("job events = %q, want exactly the stall and the shutdown interruption: a withdrawn or reaped job is the cleanup working, not an error", got)
			}
			for _, m := range got {
				if strings.Contains(m, "withdrawn:") || strings.Contains(m, "reaped:") {
					t.Fatalf("event %q lists a job the node took back", m)
				}
			}
			// The rows themselves stay in the node's job list: the overview still shows what
			// became of them, it just does not call it a failure.
			if len(ov.Nodes[0].Jobs) != 4 {
				t.Fatalf("jobs listed = %d, want all 4", len(ov.Nodes[0].Jobs))
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

// TestTookBackPrefixesMatchTheNodesTerminalErrors pins the pairing this package
// cannot express in code: it does not import fleetnode (that would drag the whole
// pipeline into a status view), so the two openings are written here and read
// against the node's own constants.
func TestTookBackPrefixesMatchTheNodesTerminalErrors(t *testing.T) {
	for _, taken := range []string{fleetnode.ErrWithdrawn, fleetnode.ErrReaped} {
		if !nodeTookBack(taken) {
			t.Errorf("nodeTookBack(%q) = false, want true", taken)
		}
	}
	for _, real := range []string{fleetnode.ErrInterrupted, fleetnode.ErrNeverStarted, "boom", "", "stalled: no progress for 300s in prefill"} {
		if nodeTookBack(real) {
			t.Errorf("nodeTookBack(%q) = true, want false: that is a failure an operator wants to see", real)
		}
	}
}
