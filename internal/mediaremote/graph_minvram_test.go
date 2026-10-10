package mediaremote

// A bad min_vram_gb is refused by name on every route, not only on the ones that reach a node (found by review of the first
// S4 round: the typo vanished on a call that stayed on this machine).

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A bad min_vram_gb is the caller's mistake on any route: it is refused by name before the lane is asked or a node is read,
// so the typo is never silently dropped by a call that happens to stay on this machine.
func TestAMalformedMinVRAMIsRefusedOnEveryRoute(t *testing.T) {
	for _, route := range []string{"local", "auto", "remote"} {
		for _, bad := range []any{-1, "sixteen", true, "-4"} {
			n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
			cfg := graphClient(t, n)
			lr := &laneRunner{verdict: heldLane()}
			res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, map[string]any{"min_vram_gb": bad}), route, nil)
			if res.OK || !res.Deferred || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "min_vram_gb") {
				t.Errorf("route %s, min_vram_gb %v: want a contract refusal naming min_vram_gb, got %s", route, bad, brief(res))
			}
			if probes, runs := lr.counts(); probes != 0 || runs != 0 {
				t.Errorf("route %s, min_vram_gb %v: nothing is asked or run (probes %d runs %d)", route, bad, probes, runs)
			}
			untouched(t, "the node", n)
		}
	}
	// Well-formed values, 0 and a numeral in a string included, are not refused.
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, n)
	for _, good := range []any{0, "0", 8, "7.5", nil} {
		resetPlacer()
		lr := &laneRunner{}
		if res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, map[string]any{"min_vram_gb": good}), "local", nil); !res.OK {
			t.Errorf("min_vram_gb %v is well formed: %s", good, brief(res))
		}
	}
}
