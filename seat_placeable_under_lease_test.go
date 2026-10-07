package main

import (
	"sort"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// The C-86 estimate, measured (plan P4 acceptance): how many of the three-card tier's declared
// seats stay placeable while a lease holds one card. The count is computed through the code the
// consumers use (the shipped table resolved the way a node seeds it, each seat's pin resolved
// against the card table, gpulease.Info.Touches), so it moves when the table or the rule does.
// docs/systems/gpu-lease.md quotes these numbers.
func TestSeatsThatStayPlaceableUnderACardLease(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["blackwell-3x16"]
	if !ok {
		t.Fatal("no blackwell-3x16 tier")
	}
	seed, err := tierseed.Resolve(p, "blackwell-3x16", tierseed.Options{Home: "/opt/offload", GOOS: "linux", VLLMSeatActive: true})
	if err != nil {
		t.Fatal(err)
	}
	layers, _ := seed["layers"].([]config.LayerSpec)
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000", Name: "T", TotalGiB: 16, FreeGiB: 15},
		{Index: 1, UUID: "GPU-bbbb0000", Name: "T", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")

	type seat struct{ layer, role, pin string }
	var seats []seat
	free := map[string][]seat{} // held card id -> seats that stay placeable
	held := []string{"gpu-aaaa0000", "gpu-bbbb0000", "gpu-cccc0000"}
	for _, l := range layers {
		for _, s := range l.Seats {
			seats = append(seats, seat{l.Name, s.Role, s.Device})
			ids, ok := gpulease.ResolvePins(s.DeviceList(), cards)
			if !ok {
				t.Fatalf("seat %s/%s pin %q does not resolve against a 3-card table", l.Name, s.Role, s.Device)
			}
			for _, h := range held {
				lease := gpulease.Info{Held: true, Epoch: 1, Class: gpulease.ClassMedia, Devices: []string{h}}
				if !lease.Touches(ids) {
					free[h] = append(free[h], seat{l.Name, s.Role, s.Device})
				}
			}
		}
	}
	name := func(ss []seat) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.layer+"/"+s.role)
		}
		sort.Strings(out)
		return out
	}
	t.Logf("declared seats: %d; placeable under a card-0 lease: %v; card-1 (display): %v; card-2: %v",
		len(seats), name(free["gpu-aaaa0000"]), name(free["gpu-bbbb0000"]), name(free["gpu-cccc0000"]))

	// Eight since 2026-10-04: the three-card layer (its one agent seat pinned to every card)
	// left the tier when the harness moved back to the pair; it declared nine while it stood.
	if len(seats) != 8 {
		t.Fatalf("the tier declares %d seats, the docs quote 8: re-measure and update docs/systems/gpu-lease.md", len(seats))
	}
	for card, want := range map[string]int{"gpu-aaaa0000": 3, "gpu-bbbb0000": 7, "gpu-cccc0000": 3} {
		if got := len(free[card]); got != want {
			t.Errorf("seats placeable while %s is held = %d (%v), want %d", card, got, name(free[card]), want)
		}
	}
	// Under a card-2 lease the live seats left are the single layer's router and agent on card 0
	// (the third is the dormant display layer's twin on the display card).
	wantCard2 := []string{"display/router", "single/agent", "single/router"}
	if got := name(free["gpu-cccc0000"]); len(got) != 3 || got[0] != wantCard2[0] || got[1] != wantCard2[1] || got[2] != wantCard2[2] {
		t.Errorf("under a card-2 lease the placeable seats are %v, want %v", got, wantCard2)
	}
}
