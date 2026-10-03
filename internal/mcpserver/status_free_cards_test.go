// status_free_cards_test.go: GPU routing P1 — offload_status.fleet.nodes[] shows
// each node's free-card count from its own gpu_devices[], and flags an overdue
// lease, so an operator can see what placement ranked on without reading raw
// health.

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

func TestFleetViewShowsFreeCardsAndOverdueLease(t *testing.T) {
	serve := func(body map[string]any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(body)
		}))
	}
	dev := func(idx int, uuid string, util int, display bool) map[string]any {
		return map[string]any{"index": idx, "uuid": uuid, "name": "synthetic 16 GB", "vram_total_gb": 16, "vram_free_gb": 15, "util_pct": util, "util_known": true, "display_active": display}
	}
	withCards := serve(map[string]any{
		"node_id": "node-cards", "agent_enabled": true, "agent_seat": "offload-e4b", "agent_seat_resident": true, "agent_ctx_tokens": 8192,
		"lease": map[string]any{"held": true, "class": "media", "busy": true, "overdue": true},
		"gpu_devices": []any{
			dev(0, "GPU-1111aaaa-2222-3333-4444-555566667777", 97, false),
			dev(1, "GPU-2222bbbb-3333-4444-5555-666677778888", 2, false),
			dev(2, "GPU-3333cccc-4444-5555-6666-777788889999", 1, true), // the display card is never free
		},
	})
	defer withCards.Close()
	older := serve(map[string]any{"node_id": "node-old", "agent_enabled": true, "agent_seat": "offload-e4b", "agent_seat_resident": true, "agent_ctx_tokens": 8192})
	defer older.Close()

	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = []string{withCards.URL, older.URL}

	s := New(pipeline.New(cfg, nil, nil, nil))
	out := s.fleetView(context.Background(), cfg)
	nodes, ok := out["nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("fleet nodes = %v, want 2 entries", out["nodes"])
	}
	a, b := nodes[0].(map[string]any), nodes[1].(map[string]any)
	if a["free_cards"] != 1 || a["cards_total"] != 3 {
		t.Fatalf("node with cards: free_cards %v of %v, want 1 of 3 (one busy, one display)", a["free_cards"], a["cards_total"])
	}
	if a["gpu_lease_overdue"] != true {
		t.Fatalf("node with an overdue lease: gpu_lease_overdue = %v, want true", a["gpu_lease_overdue"])
	}
	if _, present := a["gpu_lease_busy"]; present {
		t.Fatalf("an overdue-only lease must not also read as the long-lease busy flag: %v", a)
	}
	// A node that publishes no devices has NO figure: absent, never 0.
	for _, key := range []string{"free_cards", "cards_total", "gpu_lease_overdue"} {
		if _, present := b[key]; present {
			t.Fatalf("older node row carries %s = %v; unknown must be absent, not zero", key, b[key])
		}
	}
}

// An overdue lease is still a held lease, so the one-word verdict says so
// instead of reading the node as loaded-idle or cold.
func TestNodeVerdictOverdueLeaseIsHeldIdle(t *testing.T) {
	loaded := true
	v := delegate.NodeView{LeaseOverdue: true, SeatLoaded: &loaded}
	if _, verdict := nodeVerdict(v); verdict != "held-idle" {
		t.Fatalf("overdue-lease verdict = %q, want held-idle", verdict)
	}
}
