// seatless_test.go: a box with no agent seat (a delegation client: `install client` writes agent_model
// and model empty) is never a placement. Before this, an idle local seat won route=auto and subtask 0
// took route=spread's local slot whether or not the box had a seat, so a client's agent_delegate (auto)
// and offload_research (spread) deferred "no agent seat resolvable" with the fleet idle beside them
// (live repro 2026-10-03, a client config against two fleet nodes).

package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// seatlessCfg is testCfg with no agent seat to resolve: no agent_model, no model.
func seatlessCfg(t *testing.T) config.Config {
	cfg := testCfg(t)
	cfg.AgentModel = ""
	cfg.Model = ""
	return cfg
}

func TestASeatlessBoxSendsAutoAndSpreadToTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	for _, route := range []string{"auto", "spread"} {
		t.Run(route, func(t *testing.T) {
			_, url := acceptingNode(t, "node-a", "zorblax from node-a", nil)
			contracts := []core.AgentContract{remoteContract(), remoteContract()}
			results, sum, err := Run(context.Background(), seatlessCfg(t), neverLocal(t), contracts, route, []string{url})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum.Succeeded != len(contracts) {
				t.Fatalf("summary = %+v, want every contract run on the fleet", sum)
			}
			for i, r := range results {
				if r.Node != "node-a" {
					t.Errorf("subtask %d ran on %q (%s), want node-a: a box with no agent seat is never a placement", i, r.Node, r.PlacementReason)
				}
			}
		})
	}
}

func TestASeatlessBoxWithNoEligibleRemoteSaysWhy(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := acceptingNode(t, "node-off", "never", func(f *fakeNode) { f.agentEnabled = false })
	// The production local run on a seatless box defers by config (pipeline.runAgentContract); this
	// stands in for it so the test sees the placement, not a crash.
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, DeferClass: core.DeferClassConfig,
			Reason: "no agent seat resolvable (agent_model and model both empty)"}, nil
	}
	results, _, _ := Run(context.Background(), seatlessCfg(t), local, []core.AgentContract{remoteContract()}, "auto", []string{url})
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if !results[0].Result.Deferred || !strings.Contains(results[0].PlacementReason, "has no agent seat") {
		t.Fatalf("result = deferred %v, reason %q: want a defer whose placement reason says this box has no agent seat", results[0].Result.Deferred, results[0].PlacementReason)
	}
}

func TestASeatlessDealNeverGivesTheLocalSlot(t *testing.T) {
	r := &runner{cfg: seatlessCfg(t), route: "spread"}
	a, b := eligibleRemote(), eligibleRemote()
	a.NodeID, b.NodeID = "node-a", "node-b"
	r.spreadViews, r.spreadBases = []NodeView{a, b}, []string{"http://node-a", "http://node-b"}
	seatless := NodeView{NodeID: "client", Local: true}
	contracts := []core.AgentContract{remoteContract(), remoteContract(), remoteContract(), remoteContract()}
	for i, s := range r.dealSpread(contracts, seatless) {
		if s.view.Local {
			t.Errorf("spread subtask %d dealt the local slot of a seatless box (%s)", i, s.reason)
		}
	}
	auto := r.dealAutoRemote(contracts, seatless, []NodeView{a, b}, []string{"http://node-a", "http://node-b"}, false, nil)
	for i, s := range auto {
		if s.view.Local {
			t.Errorf("auto subtask %d dealt the idle local slot of a seatless box (%s)", i, s.reason)
		}
	}
	// The same box with a seat keeps spread's guarantee: subtask 0 stays local.
	seated := seatless
	seated.AgentSeat = "local-seat"
	if s := r.dealSpread(contracts[:1], seated); !s[0].view.Local {
		t.Errorf("a seated box lost spread's subtask-0 local slot: %s", s[0].reason)
	}
}
