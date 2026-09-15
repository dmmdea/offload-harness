// Composite tier (ADR 0039, Task 6): the placement memo is process-wide. The
// in-loop offload builds a fresh Pipeline per contract (NewInLoopPipeline per
// agent_run, NewRecordlessOffload per node-side contract), so a memo owned by
// the pipeline would give every in-flight contract its own readers — 32
// contracts on the pair seat would GET /running (and exec nvidia-smi) 32
// times per 2 s window, the multiplicity the Global Constraint forbids.
package pipeline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// TestInLoopPipelinesShareOneLiveSnapshot: three pipelines over the same box
// — two in-loop ones as the offload closures build them and a recordless one
// as the node-side contract builds it — read the pair's agent seat in one
// window and llama-swap sees ONE /running GET. Nothing here touches
// nvidia-smi or the presence probe: the seat read is the reader every cascade
// decision pays, so it is the one counted.
func TestInLoopPipelinesShareOneLiveSnapshot(t *testing.T) {
	var running atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			running.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []any{}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]string{{"id": "agent-pool"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.CompositeFixture()
	cfg.Home = t.TempDir()
	cfg.Endpoint = srv.URL
	cfg.Model = "workhorse"
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	if err := cfg.ValidateLayers(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}

	pipes := []*Pipeline{
		NewInLoopPipeline(cfg, 5*time.Second, nil), // what NewInLoopOffload builds per agent_run
		NewInLoopPipeline(cfg, 5*time.Second, nil), // a second in-flight contract's cascade
		NewRecordlessPipeline(cfg, 5*time.Second),  // what NewRecordlessOffload builds per node-side contract
	}
	for i, p := range pipes {
		st := p.live().Seat(placement.LayerPair, placement.RoleAgent)
		if !st.Known || st.Loaded {
			t.Fatalf("pipeline %d: seat state = %+v, want a known, idle reading", i, st)
		}
	}
	if got := running.Load(); got != 1 {
		t.Fatalf("/running GETs = %d across %d pipelines in one window, want 1 (one Snapshot per box, not per pipeline)", got, len(pipes))
	}
}
