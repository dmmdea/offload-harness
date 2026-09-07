package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// TestPulledAgentJobCarriesTheAgentMarker is the BEHAVIOURAL guard for 255:
// a job the claim loop PULLS must be stamped Agent, or handleJob's bearer gate
// never fires for it and its result reads unauthenticated.
func TestPulledAgentJobCarriesTheAgentMarker(t *testing.T) {
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

	served := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
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
	id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg)
	if !ok || id != "pulled-1" {
		t.Fatalf("claimOne = %q, %v; want pulled-1, true", id, ok)
	}

	view, found := jobs.Get("pulled-1")
	if !found {
		t.Fatal("pulled job absent from the store")
	}
	if !view.Agent {
		t.Error("pulled agent job: Agent = false — handleJob's bearer gate will never fire for it")
	}
	// Poll it with no bearer: an agent job must answer 401.
	rec := do(t, s, http.MethodGet, "/fleet/jobs/pulled-1", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated poll of a PULLED agent job = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	// Feed metadata.
	for _, v := range jobs.Recent(10) {
		if v.ID == "pulled-1" {
			if v.Task != "agent" {
				t.Errorf("feed Task = %q, want agent", v.Task)
			}
			if v.Model != "agent-seat" {
				t.Errorf("feed Model = %q, want the agent seat", v.Model)
			}
		}
	}
}
