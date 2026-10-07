package gpualloc

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// displayCfg is a composite box with the pair on cards 0 and 2 and the display layer's router
// seat on card 1: the twins are named only in its model_map, and the seat declares what it puts
// on the display card as display_footprint_gib (it has no footprint_gib of its own).
func displayCfg() config.Config {
	return config.Config{Layers: []config.LayerSpec{
		{Name: "pair", Seats: []config.LayerSeat{{Role: "agent", Model: "agent-pool", Device: "0,2", FootprintGiB: 15}}},
		{Name: "display", DisplayDevice: "1", DisplayFloorGiB: 4, Guards: []string{"display_floor", "presence"},
			Seats: []config.LayerSeat{{Role: "router", Device: "1", DisplayFootprintGiB: 6.2,
				ModelMap: map[string]string{"workhorse": "gemma-4-e4b-display", "triage": "gemma-4-e2b-display"}}}},
	}}
}

// A loaded twin sits on the display card although no seat names it as its model: the allocator has
// to see it there, costed at the footprint the layer declares for the card, or the display card
// reads as an empty one that is free to take.
func TestResidentFromCountsALoadedModelMapTwinOnItsCard(t *testing.T) {
	cards := threeCards()
	id := cards[1].LeaseID()

	got := ResidentFrom(displayCfg(), cards, map[string]bool{"gemma-4-e4b-display": true})
	if r := got[id]; len(r.Seats) != 1 || r.Seats[0] != "gemma-4-e4b-display" || r.CostGiB != 6.2 {
		t.Fatalf("card 1 = %+v, want the e4b twin costing the declared 6.2", r)
	}

	// Names compare case-insensitively, as a seat's own model does.
	cfg := displayCfg()
	got = ResidentFrom(cfg, cards, map[string]bool{"gemma-4-e4b-display": true})
	if len(got[id].Seats) != 1 {
		t.Fatalf("a lower-cased /running id must match the declared twin: %+v", got)
	}

	// Nothing loaded, nothing resident: the display card is empty again.
	if r, ok := ResidentFrom(displayCfg(), cards, map[string]bool{})[id]; ok {
		t.Fatalf("no twin is loaded, card 1 must have no resident: %+v", r)
	}
	// A model that is not one of the twins does not land on card 1.
	if r, ok := ResidentFrom(displayCfg(), cards, map[string]bool{"gemma-4-26b": true})[id]; ok {
		t.Fatalf("an unrelated model is not on the display card: %+v", r)
	}

	// Both twins loaded (two clients, one each): both are named and BOTH are costed, because the cost is
	// what taking the card evicts and each twin holds its own footprint on the card (about 6.2 GiB each).
	// It was costed once per seat, which priced two loaded twins as one and ranked the card as cheaper
	// to take than it is.
	got = ResidentFrom(displayCfg(), cards, map[string]bool{"gemma-4-e4b-display": true, "gemma-4-e2b-display": true})
	if r := got[id]; len(r.Seats) != 2 || r.CostGiB < 12.39 || r.CostGiB > 12.41 {
		t.Fatalf("card 1 = %+v, want both twins named and costed at 6.2 each (12.4)", r)
	}
}

// A seat that declares a footprint_gib keeps it: the display footprint is only the fallback for a
// seat that has none, and the pair's seat on cards 0 and 2 is counted exactly as before.
func TestResidentFromKeepsTheSeatFootprintAndFallsBackToTheDisplayOne(t *testing.T) {
	cards := threeCards()
	got := ResidentFrom(displayCfg(), cards, map[string]bool{"agent-pool": true})
	if r := got[cards[0].LeaseID()]; len(r.Seats) != 1 || r.CostGiB != 15 {
		t.Fatalf("card 0 = %+v, want the pair seat at its 15 GiB footprint", r)
	}
	if _, ok := got[cards[1].LeaseID()]; ok {
		t.Fatal("the pair seat is not on the display card")
	}

	cfg := displayCfg()
	cfg.Layers[1].Seats[0].FootprintGiB = 5
	got = ResidentFrom(cfg, cards, map[string]bool{"gemma-4-e2b-display": true})
	if r := got[cards[1].LeaseID()]; r.CostGiB != 5 {
		t.Fatalf("a declared footprint_gib wins over display_footprint_gib: %+v", r)
	}
}

// BuildInput hands the allocator the floor the declared layers keep, beside AllowDisplay, so both
// of its callers (gpu reserve --cards and the media admission path) hold the opened display card
// to it without each carrying its own copy of the number.
func TestBuildInputCarriesTheLayersDesktopFloor(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	in, err := BuildInput(context.Background(), m, displayCfg(), Need{}, hostDeps(cards, true))
	if err != nil {
		t.Fatal(err)
	}
	if !in.AllowDisplay || in.DisplayFloorGiB != 4 {
		t.Fatalf("AllowDisplay=%v DisplayFloorGiB=%v, want true and 4", in.AllowDisplay, in.DisplayFloorGiB)
	}
	// A box that declares no display_floor layer keeps the behaviour it had: no floor.
	in, err = BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(cards, true))
	if err != nil || in.DisplayFloorGiB != 0 {
		t.Fatalf("a plain box carries no floor: %v err %v", in.DisplayFloorGiB, err)
	}
}

// The reason the floor exists, end to end through the shared builder and the picker: with the
// operator away the display card has 14 GiB free, and a job that would leave it under the layer's
// floor is placed on a pair card instead.
func TestPickAutoDoesNotHandOutTheDisplayCardBelowTheLayersFloor(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	// The pair seat is loaded on cards 0 and 2, so the display card is the one with no resident
	// seat: the card the order would put first.
	pairLoaded := func(context.Context, config.Config, []gpuprobe.Card) map[string]gpulease.ResidentInfo {
		return ResidentFrom(displayCfg(), cards, map[string]bool{"agent-pool": true})
	}
	for _, tc := range []struct {
		name      string
		vram      float64
		wantCard1 bool
	}{
		{"11 of 14 free leaves 3 under the floor", 11, false},
		{"6 of 14 free leaves 8", 6, true},
		{"no footprint declared cannot be shown to keep the floor", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := hostDeps(cards, true)
			deps.Resident = pairLoaded
			build := func() (gpulease.AllocInput, error) {
				return BuildInput(context.Background(), m, displayCfg(), Need{VRAMGiB: tc.vram}, deps)
			}
			ids, _, err := PickAuto(Plan{Min: 1, Max: 1}, time.Minute, build, io.Discard, func(time.Duration) {}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			got := len(ids) == 1 && ids[0] == cards[1].LeaseID()
			if got != tc.wantCard1 {
				t.Fatalf("display card handed out = %v, want %v (picked %v)", got, tc.wantCard1, ids)
			}
		})
	}
}

// An unreadable /running is not an empty one. For the ORDER of the cards the display card is the one
// that matters: a loaded twin makes it a card with something to evict, and read as empty it would sort
// first and be handed out while a twin may be on it. So the display layer's models are counted as
// loaded, each as a loaded twin is, when llama-swap cannot be read.
func TestResidentSeatsTreatsTheDisplayCardAsOccupiedWhenRunningCannotBeRead(t *testing.T) {
	cards := threeCards()
	id := cards[1].LeaseID()
	cfg := displayCfg()
	cfg.Endpoint = "http://127.0.0.1:1" // nothing listens
	for name, client := range map[string]*http.Client{"llama-swap client": nil, "shared client": {Timeout: time.Second}} {
		t.Run(name, func(t *testing.T) {
			got := ResidentSeats(context.Background(), cfg, cards, client)
			r, ok := got[id]
			if !ok || len(r.Seats) != 2 {
				t.Fatalf("display card = %+v (present %v), want both twins assumed loaded", r, ok)
			}
			if r.CostGiB < 12.39 || r.CostGiB > 12.41 {
				t.Errorf("each assumed twin is costed at its footprint: %v GiB, want 12.4", r.CostGiB)
			}
			if _, has := got[cards[0].LeaseID()]; has {
				t.Errorf("the pair seat is not assumed loaded on card 0: nothing about another layer is conservative to assume: %+v", got)
			}
		})
	}
}

// An endpoint the llama-swap client cannot even be built for is as unreadable as one nobody answers.
func TestResidentSeatsTreatsAnUnusableEndpointAsUnreadableToo(t *testing.T) {
	cards := threeCards()
	cfg := displayCfg()
	cfg.Endpoint = "http://[::1"
	got := ResidentSeats(context.Background(), cfg, cards, nil)
	if r := got[cards[1].LeaseID()]; len(r.Seats) != 2 {
		t.Fatalf("an endpoint that cannot be read leaves the display card occupied, got %+v", got)
	}
}

// A box with no display layer, or no endpoint to read, keeps the empty answer it always had.
func TestResidentSeatsIsStillEmptyWithoutADisplayLayerOrAnEndpoint(t *testing.T) {
	cards := threeCards()
	plain := scopedCfg()
	plain.Endpoint = "http://127.0.0.1:1"
	if got := ResidentSeats(context.Background(), plain, cards, nil); len(got) != 0 {
		t.Fatalf("no display layer, nothing to assume: %v", got)
	}
	noEndpoint := displayCfg()
	if got := ResidentSeats(context.Background(), noEndpoint, cards, nil); len(got) != 0 {
		t.Fatalf("no llama-swap endpoint means nothing can be loaded: %v", got)
	}
}

// Through the shared builder: with /running unreadable the allocator input carries the display card as
// occupied, and with the operator away the picker prefers the card with nothing on it.
func TestBuildInputOrdersTheDisplayCardLastWhenRunningCannotBeRead(t *testing.T) {
	m := scratchManager(t)
	cfg := displayCfg()
	cfg.Endpoint = "http://127.0.0.1:1"
	// The display card (index 1) gets the lowest lease id, so on a tie it sorts first: only occupancy
	// can put it last.
	devs := []gpuprobe.Device{
		{Index: 0, UUID: uA, Name: "T", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: "GPU-0000aaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "T", TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: uC, Name: "T", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	cards, _ := gpuprobe.BuildCards(devs, "")
	displayID := cards[1].LeaseID()
	in, err := BuildInput(context.Background(), m, cfg, Need{VRAMGiB: 2}, hostDeps(cards, true))
	if err != nil {
		t.Fatal(err)
	}
	if r := in.Resident[displayID]; len(r.Seats) == 0 {
		t.Fatalf("the display card must read as occupied in the allocator input: %+v", in.Resident)
	}
	in.Min, in.Max = 1, 1
	a, err := gpulease.Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Devices) != 1 || a.Devices[0] == displayID {
		t.Fatalf("a card with nothing on it must be taken before the display card that may hold a twin, got %v", a.Devices)
	}
}
