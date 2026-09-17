// w31_verdict_test.go: item 9 of PR-5 (W-31, register D-105's sibling) —
// offload_status.fleet.nodes[] gains in_flight and verdict from the 0.127
// health activity fields; the local seat entry gets the same two from
// probeLocalBusy's own primitive (seatload.Inflight).

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func TestNodeVerdictRunningMinusAdmittingIsBusy(t *testing.T) {
	loaded := true
	v := delegate.NodeView{JobsRunning: 2, JobsAdmitting: 1, SeatLoaded: &loaded}
	inFlight, verdict := nodeVerdict(v)
	if inFlight != 1 || verdict != "busy" {
		t.Fatalf("nodeVerdict = (%d, %q), want (1, \"busy\")", inFlight, verdict)
	}
}

func TestNodeVerdictLoadedIdle(t *testing.T) {
	loaded := true
	v := delegate.NodeView{JobsRunning: 0, JobsAdmitting: 0, SeatLoaded: &loaded}
	inFlight, verdict := nodeVerdict(v)
	if inFlight != 0 || verdict != "loaded-idle" {
		t.Fatalf("nodeVerdict = (%d, %q), want (0, \"loaded-idle\")", inFlight, verdict)
	}
}

func TestNodeVerdictCold(t *testing.T) {
	notLoaded := false
	v := delegate.NodeView{JobsRunning: 0, JobsAdmitting: 0, SeatLoaded: &notLoaded}
	inFlight, verdict := nodeVerdict(v)
	if inFlight != 0 || verdict != "cold" {
		t.Fatalf("nodeVerdict = (%d, %q), want (0, \"cold\")", inFlight, verdict)
	}
}

func TestNodeVerdictHeldIdleAndUnknown(t *testing.T) {
	held := delegate.NodeView{JobsRunning: 0, LeaseExclusive: true}
	if _, verdict := nodeVerdict(held); verdict != "held-idle" {
		t.Fatalf("held verdict = %q, want held-idle", verdict)
	}
	unknown := delegate.NodeView{JobsRunning: 0}
	if _, verdict := nodeVerdict(unknown); verdict != "unknown" {
		t.Fatalf("unpublished-seat verdict = %q, want unknown", verdict)
	}
}

// TestFleetViewPublishesInFlightAndVerdict drives the whole path end to end:
// a fake node publishes the 0.127 activity fields on /fleet/health, and
// fleetView's node row must carry in_flight/verdict derived from them.
func TestFleetViewPublishesInFlightAndVerdict(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-w31", "agent_enabled": true, "agent_seat": "offload-e4b",
			"agent_seat_resident": true, "agent_ctx_tokens": 8192, "queue_depth": 1,
			"jobs_running": 2, "jobs_admitting": 1, "seat_loaded": true,
		})
	}))
	defer node.Close()

	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = []string{node.URL}

	s := New(pipeline.New(cfg, nil, nil, nil))
	out := s.fleetView(context.Background(), cfg)
	nodes, ok := out["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("fleet nodes = %v, want 1 entry", out["nodes"])
	}
	n := nodes[0].(map[string]any)
	if n["in_flight"] != 1 {
		t.Fatalf("in_flight = %v, want 1 (jobs_running 2 - jobs_admitting 1)", n["in_flight"])
	}
	if n["verdict"] != "busy" {
		t.Fatalf("verdict = %v, want busy", n["verdict"])
	}
}
