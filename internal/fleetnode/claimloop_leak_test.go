package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// TestReclaimOfAKnownJobReleasesItsMaterialization: on Admit's refusal the
// claim loop still owns what BuildRequest materialized — Admit never fires
// OnDropped on a refusal and the run closure is never entered.
func TestReclaimOfAKnownJobReleasesItsMaterialization(t *testing.T) {
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		LoopbackListener: true,
	})
	// The id is ALREADY known locally: the lease-expiry re-claim case.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	if !jobs.Admit("pulled-1", AcceptSpec{Agent: true}, func(ctx context.Context) (json.RawMessage, error) {
		<-block
		return nil, nil
	}) {
		t.Fatal("seed admit refused")
	}

	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-1", TaskType: "agent",
				Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()

	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok {
		t.Fatal("claimOne reported no claim")
	}

	entries, err := os.ReadDir(filepath.Join(cfg.BaseDir(), "pipeline-jobs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused re-claim stranded %d job dir(s) under pipeline-jobs/: %v — the second materialization is never freed (Admit does not fire OnDropped on a refusal and run is never entered)", len(entries), entries)
	}
}
