package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Register C-58 (operator 2026-09-18: "nvidia pair showing the qube doing
// lenovo work"): a route=local run whose config endpoint is ANOTHER box's
// engine is that box's work. The ledger row names that host as the node, the
// placement reason says so, and the PAIR card is scheduled on that member —
// not on this box's identity.
func TestLocalRunAgainstAnotherBoxsEngineIsAttributedToThatBox(t *testing.T) {
	pairAppDir(t)
	// The endpoint's host is a cluster member with its own PAIR uuid.
	members := filepath.Join(os.Getenv("OFFLOAD_PAIR_APPDIR"), "cluster", "members.json")
	if err := os.WriteFile(members, []byte(`[{"name":"node-a","nodeUuid":"self-uuid"},{"name":"node-b","nodeUuid":"lenovo-uuid"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.Endpoint = "http://node-b:18797"
	cfg.AgentModel = "a2-pool"
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "a2-pool"}, nil
	})
	res, sum, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || len(res) != 1 {
		t.Fatalf("succeeded=%d results=%d", sum.Succeeded, len(res))
	}
	if res[0].Node != "node-b" {
		t.Fatalf("node = %q, want the endpoint's host node-b (the box whose engine ran)", res[0].Node)
	}
	if !strings.Contains(res[0].PlacementReason, "route=local forced") || !strings.Contains(res[0].PlacementReason, "node-b") {
		t.Fatalf("placement reason must keep the route and name the engine's box: %q", res[0].PlacementReason)
	}
	deadline := 50
	for len(c.snapshot()) < 2 && deadline > 0 {
		deadline--
		waitABit()
	}
	frames := c.snapshot()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2: %v", len(frames), frames)
	}
	a, b := pairInfo(frames[0]), pairInfo(frames[1])
	if a["scheduledOn"] != "lenovo-uuid" || b["scheduledOn"] != "lenovo-uuid" {
		t.Fatalf("the PAIR card must be scheduled on the engine's box, got %v / %v", a["scheduledOn"], b["scheduledOn"])
	}
}

// The same run against this box's own engine keeps the old attribution.
func TestLocalRunAgainstOwnEngineIsAttributedToThisBox(t *testing.T) {
	pairAppDir(t)
	cfg := testCfg(t)
	cfg.Endpoint = "http://127.0.0.1:11434"
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "local-seat"}, nil
	})
	res, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hn, _ := os.Hostname()
	if len(res) != 1 || res[0].Node != hn {
		t.Fatalf("node = %q, want this box (%s)", res[0].Node, hn)
	}
	if strings.Contains(res[0].PlacementReason, "attributed there") {
		t.Fatalf("a loopback endpoint must not be described as another box: %q", res[0].PlacementReason)
	}
}
