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
)

// Per-card tiles (plan P7, register C-86). A node used to read as one gauge: the busiest card.
// With card-scoped leases the operator's question is which card holds what, and for how long,
// so the overview joins each card the node publishes (gpu_devices[]) with the lease that sits
// on it (leases[]), and says "free" only for a card nothing holds.

func threeCardHealth(extra map[string]any) map[string]any {
	h := map[string]any{
		"node_id": "n1", "vram_total_gb": 16.0, "vram_free_gb": 9.5, "gpu_util_pct": 40, "gpu_util_known": true,
		"agent_enabled": true, "agent_seat": "agent-pool", "agent_seat_resident": true, "queue_depth": 0,
		"gpu_devices": []map[string]any{
			{"index": 0, "uuid": "GPU-AAAA0000", "name": "Card A", "vram_total_gb": 16.0, "vram_free_gb": 14.0, "util_pct": 3, "util_known": true},
			{"index": 1, "uuid": "GPU-BBBB0000", "name": "Card B", "vram_total_gb": 16.0, "vram_free_gb": 15.0, "util_pct": 0, "util_known": true, "display_active": true},
			{"index": 2, "uuid": "GPU-CCCC0000", "name": "Card C", "vram_total_gb": 16.0, "vram_free_gb": 2.0, "util_pct": 97, "util_known": true},
		},
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func foldOne(t *testing.T, health map[string]any) Node {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(health) })
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := NewPoller(config.Config{}, []string{srv.URL}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Run(ctx)
	for {
		if ov := p.Snapshot(); len(ov.Nodes) == 1 && ov.Nodes[0].Reachable {
			return ov.Nodes[0]
		}
		select {
		case <-ctx.Done():
			t.Fatal("the node never folded")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func tileByUUID(t *testing.T, n Node, uuid string) CardTile {
	t.Helper()
	for _, c := range n.Cards {
		if strings.EqualFold(c.UUID, uuid) {
			return c
		}
	}
	t.Fatalf("no tile for %s in %+v", uuid, n.Cards)
	return CardTile{}
}

func TestFleetviewPerCardTiles(t *testing.T) {
	n := foldOne(t, threeCardHealth(map[string]any{
		"lease": map[string]any{"held": true, "class": "media", "busy": true, "remaining_sec": 5400},
		"leases": []map[string]any{
			{"epoch": 7, "class": "media", "devices": []string{"gpu-cccc0000"}, "scope": "declared", "remaining_sec": 5400, "busy": true, "verdict": "held"},
		},
	}))
	if len(n.Cards) != 3 {
		t.Fatalf("cards = %+v, want one tile per card the node publishes", n.Cards)
	}
	a, b, c := tileByUUID(t, n, "GPU-AAAA0000"), tileByUUID(t, n, "GPU-BBBB0000"), tileByUUID(t, n, "GPU-CCCC0000")
	if a.Holder != nil || !a.Free || a.Index != 0 || a.VramUsedGB != 2 || a.VramTotalGB != 16 || a.UtilPct != 3 {
		t.Fatalf("card A = %+v, want a free card with its VRAM and utilisation", a)
	}
	if b.Holder != nil || b.Free || !b.Display {
		t.Fatalf("card B = %+v, want the display card, never free for work", b)
	}
	if c.Holder == nil || c.Free || c.Holder.Epoch != 7 || c.Holder.Class != "media" || c.Holder.Verdict != "held" || c.Holder.RemainingSec != 5400 || c.Holder.Scope != "declared" {
		t.Fatalf("card C = %+v holder = %+v, want the media lease on it with its term", c, c.Holder)
	}
	if len(n.Leases) != 1 {
		t.Fatalf("the raw leases are kept for the page: %+v", n.Leases)
	}
}

// A lease that names no cards holds every card; a busy card with no lease is busy, not free.
func TestFleetviewTilesWholeNodeLeaseAndBusyCardWithoutALease(t *testing.T) {
	whole := foldOne(t, threeCardHealth(map[string]any{
		"leases": []map[string]any{{"epoch": 3, "class": "text", "scope": "whole-node", "verdict": "held-overdue", "overdue": true}},
	}))
	for _, c := range whole.Cards {
		if c.Display {
			continue
		}
		if c.Holder == nil || c.Holder.Epoch != 3 || c.Holder.Scope != "whole-node" || !c.Holder.Overdue || c.Free {
			t.Fatalf("card %s = %+v: a whole-node lease holds every card the operator can use", c.UUID, c)
		}
	}
	idle := foldOne(t, threeCardHealth(nil))
	c := tileByUUID(t, idle, "GPU-CCCC0000")
	if c.Holder != nil || c.Free {
		t.Fatalf("card C at 97%% with no lease = %+v: busy outside the harness, never free", c)
	}
}

// A node one release behind publishes one lease block: it reads as the whole node, which is how
// the delegator reads it too.
func TestFleetviewTilesOfAnOlderNodeReadTheOneLeaseBlockAsTheWholeNode(t *testing.T) {
	n := foldOne(t, threeCardHealth(map[string]any{
		"lease": map[string]any{"held": true, "class": "media", "busy": true, "overdue": true},
	}))
	a := tileByUUID(t, n, "GPU-AAAA0000")
	if a.Holder == nil || a.Holder.Scope != "whole-node" || a.Holder.Class != "media" || !a.Holder.Overdue || a.Free {
		t.Fatalf("card A = %+v: an older node's one lease block holds the whole node", a)
	}
}

// A node with no devices (or no lease) gets no tiles and no holders: nothing is invented.
func TestFleetviewTilesInventNothing(t *testing.T) {
	n := foldOne(t, map[string]any{"node_id": "bare", "queue_depth": 0})
	if len(n.Cards) != 0 || len(n.Leases) != 0 {
		t.Fatalf("cards = %+v leases = %+v from a node that published neither", n.Cards, n.Leases)
	}
}

// Snapshot hands out copies: a caller mutating the tiles must not race the poller.
func TestSnapshotDeepCopiesCardsAndLeases(t *testing.T) {
	n := &Node{
		Leases: []map[string]any{{"epoch": 1.0, "devices": []any{"gpu-aaaa0000"}}},
		Cards:  []CardTile{{UUID: "GPU-AAAA0000", Holder: &CardHolder{Epoch: 1, Class: "media"}}},
	}
	cp := copyNode(n)
	cp.Cards[0].Holder.Class = "mutated"
	cp.Cards[0].UUID = "mutated"
	cp.Leases[0]["epoch"] = 99.0
	if n.Cards[0].Holder.Class != "media" || n.Cards[0].UUID != "GPU-AAAA0000" || n.Leases[0]["epoch"] != 1.0 {
		t.Fatalf("the snapshot shares state with the poller: %+v %+v", n.Cards, n.Leases)
	}
}

// The terminal view prints the same tiles: one line per card with its holder.
func TestRenderTopShowsPerCardTiles(t *testing.T) {
	o := Overview{Nodes: []Node{{
		NodeID: "node-render", Reachable: true,
		Cards: []CardTile{
			{Index: 0, UUID: "GPU-AAAA0000", Name: "Card A", VramUsedGB: 2, VramTotalGB: 16, UtilPct: 3, UtilKnown: true, Free: true},
			{Index: 1, UUID: "GPU-BBBB0000", Name: "Card B", VramTotalGB: 16, UtilKnown: true, Display: true},
			{Index: 2, UUID: "GPU-CCCC0000", Name: "Card C", VramUsedGB: 14, VramTotalGB: 16, UtilPct: 97, UtilKnown: true,
				Holder: &CardHolder{Epoch: 7, Class: "media", Verdict: "held-overdue", Scope: "declared", Overdue: true}},
		},
	}}}
	out := RenderTop(o, 120)
	for _, want := range []string{"CARDS", "node-render", "card 0", "free", "card 1", "display", "card 2", "media lease 7", "held-overdue", "14.0/16.0", "97%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// The page carries the holder under each card's bar.
func TestPageShowsTheHolderUnderEachCard(t *testing.T) {
	p := NewPoller(config.Config{}, nil, time.Second, 5)
	rr := httptest.NewRecorder()
	NewHandler(p).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	for _, want := range []string{"n.cards", `class="holder`, "whole node"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q: the per-card holder line", want)
		}
	}
}

// A card that reported no utilisation is not known to be idle: never free.
func TestFleetviewTilesNeverCallAnUnmeasuredCardFree(t *testing.T) {
	n := foldOne(t, map[string]any{
		"node_id": "n", "queue_depth": 0,
		"gpu_devices": []map[string]any{{"index": 0, "uuid": "GPU-AAAA0000", "name": "A", "vram_total_gb": 16.0, "vram_free_gb": 16.0, "util_known": false}},
	})
	if a := tileByUUID(t, n, "GPU-AAAA0000"); a.Free || a.UtilKnown {
		t.Fatalf("card = %+v, want an unmeasured card to be neither known nor free", a)
	}
}

// A lease names its cards in whatever case a node wrote them; the join is on the lower-cased id.
func TestFleetviewTilesJoinOnTheLowerCasedUUID(t *testing.T) {
	n := foldOne(t, threeCardHealth(map[string]any{
		"leases": []map[string]any{{"epoch": 4, "class": "media", "devices": []string{"GPU-CCCC0000"}, "verdict": "held"}},
	}))
	if c := tileByUUID(t, n, "GPU-CCCC0000"); c.Holder == nil || c.Holder.Epoch != 4 {
		t.Fatalf("card C = %+v, want the lease whose device id differs only in case", c)
	}
	if a := tileByUUID(t, n, "GPU-AAAA0000"); a.Holder != nil {
		t.Fatalf("card A = %+v, want no holder", a)
	}
}

// The terminal view says when a lease holds the whole node, and how long it has left.
func TestRenderTopSaysWhenALeaseHoldsTheWholeNode(t *testing.T) {
	o := Overview{Nodes: []Node{{
		NodeID: "node-whole", Reachable: true,
		Cards: []CardTile{{Index: 0, UUID: "GPU-AAAA0000", Name: "Card A", VramTotalGB: 16, UtilKnown: true,
			Holder: &CardHolder{Epoch: 3, Class: "text", Verdict: "held", Scope: "whole-node", RemainingSec: 5400}}},
	}}}
	out := RenderTop(o, 120)
	for _, want := range []string{"text lease 3", "(whole node)", "1h30m left"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
