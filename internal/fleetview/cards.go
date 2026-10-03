package fleetview

// Per-card tiles (GPU routing P7). A node's health says, card by card, how much VRAM is used
// and how busy the card is (gpu_devices[]), and, lease by lease, which cards each live lease
// sits on (leases[]). The overview joins them so the operator reads "card 2: media lease 7,
// held-overdue, 1h30m left" instead of one gauge for the busiest card.
//
// This package does not import the delegator, so the join is its own and stays small. A lease
// names its cards by lower-cased GPU UUID; a lease that names none is the whole node and holds
// every card. A node one release behind publishes only the one lease block, which is read the
// way the delegator reads it: the whole node.

import "strings"

// freeUtilBelowPct is the utilisation under which a card is not doing work: the line the
// delegator's free-card reading and `gpu status` draw for "idle".
const freeUtilBelowPct = 15

// CardTiles joins the node's devices with its leases. leases is health leases[]; legacy is the
// singular lease block, consulted only when leases is empty (a node one release behind). It
// returns nil for a node that published no devices: nothing is invented.
func CardTiles(devices, leases []map[string]any, legacy map[string]any) []CardTile {
	if len(devices) == 0 {
		return nil
	}
	holders := make([]holding, 0, len(leases)+1)
	for _, l := range leases {
		holders = append(holders, holding{
			cards:  lowerStrs(strs(l, "devices")),
			holder: CardHolder{Epoch: uint64(num(l, "epoch")), Class: str(l, "class"), Verdict: verdictOf(l), Scope: scopeOf(l), RemainingSec: int(num(l, "remaining_sec")), Overdue: boolv(l, "overdue")},
		})
	}
	if len(leases) == 0 && boolv(legacy, "held") {
		holders = append(holders, holding{holder: CardHolder{
			Class: str(legacy, "class"), Verdict: legacyVerdict(legacy), Scope: "whole-node",
			RemainingSec: int(num(legacy, "remaining_sec")), Overdue: boolv(legacy, "overdue"),
		}})
	}
	tiles := make([]CardTile, 0, len(devices))
	for _, d := range devices {
		total, free := num(d, "vram_total_gb"), num(d, "vram_free_gb")
		t := CardTile{
			Index: int(num(d, "index")), UUID: str(d, "uuid"), Name: str(d, "name"),
			VramUsedGB: total - free, VramTotalGB: total,
			UtilPct: int(num(d, "util_pct")), UtilKnown: boolv(d, "util_known"),
			Display: boolv(d, "display_active"),
		}
		id := strings.ToLower(strings.TrimSpace(t.UUID))
		for _, h := range holders {
			if len(h.cards) == 0 || containsString(h.cards, id) {
				held := h.holder
				t.Holder = &held
				break
			}
		}
		t.Free = t.Holder == nil && !t.Display && t.UtilKnown && t.UtilPct < freeUtilBelowPct
		tiles = append(tiles, t)
	}
	return tiles
}

// holding is one lease and the cards it sits on (empty = the whole node).
type holding struct {
	cards  []string
	holder CardHolder
}

// verdictOf is the node's own word for a lease, "held" when it published none.
func verdictOf(l map[string]any) string {
	if v := str(l, "verdict"); v != "" {
		return v
	}
	return "held"
}

// scopeOf says where a lease's cards came from; a lease that says nothing and names no cards is
// the whole node.
func scopeOf(l map[string]any) string {
	if s := str(l, "scope"); s != "" {
		return s
	}
	if len(strs(l, "devices")) == 0 {
		return "whole-node"
	}
	return "declared"
}

// legacyVerdict words the one lease block of an older node.
func legacyVerdict(l map[string]any) string {
	switch {
	case boolv(l, "stalled"):
		return "held-stalled"
	case boolv(l, "orphaned"):
		return "held-orphaned"
	case boolv(l, "overdue"):
		return "held-overdue"
	}
	return "held"
}

func lowerStrs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
