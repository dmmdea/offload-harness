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

// TestClaimLoopBackpressureUsesResolvedQueueLimit is review round 1 addendum
// item 7's test: the claim loop's back-off check must use the RESOLVED
// fleet_max_queue_depth (cfg.FleetQueueLimit(), what the push path's own
// admission gate compares QueueDepth() against), not the raw config field.
// left at its default (0 = unset), the raw field made `limit > 0` false
// forever, so the loop never paced itself no matter how deep the backlog got.
func TestClaimLoopBackpressureUsesResolvedQueueLimit(t *testing.T) {
	cfg := imageCfg() // FleetMaxQueueDepth / FleetMaxConcurrentJobs both left at their zero value (unset)
	if got := cfg.FleetQueueLimit(); got != 8 {
		t.Fatalf("sanity: FleetQueueLimit() = %d, want 8 (2x the default 4 workers)", got)
	}
	if !claimBackpressureActive(cfg, 8) {
		t.Fatal("claimBackpressureActive(depth=8) = false, want true: the node is AT its RESOLVED default depth (8) even though the raw fleet_max_queue_depth field is 0 (unset)")
	}
	if claimBackpressureActive(cfg, 7) {
		t.Fatal("claimBackpressureActive(depth=7) = true, want false: one below the resolved limit")
	}
	// Control: an explicit depth still wins outright, same as FleetQueueLimit().
	cfg.FleetMaxQueueDepth = 20
	if claimBackpressureActive(cfg, 8) {
		t.Fatal("claimBackpressureActive(depth=8) = true with an explicit limit of 20, want false")
	}
	if !claimBackpressureActive(cfg, 20) {
		t.Fatal("claimBackpressureActive(depth=20) = false with an explicit limit of 20, want true")
	}
	// Control: a negative value means unlimited — never backpressure.
	cfg.FleetMaxQueueDepth = -1
	if claimBackpressureActive(cfg, 1000) {
		t.Fatal("claimBackpressureActive with fleet_max_queue_depth=-1 (unlimited) = true, want false at any depth")
	}
}

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
