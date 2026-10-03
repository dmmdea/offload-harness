package gpualloc

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Synthetic ids only: repeated-nibble heads, the shape the leak gate treats as placeholders.
const (
	uA = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	uB = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
	uC = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
)

// threeCards is the 3-card box: card 1 is the one the monitor is attached to (display_active
// reads Disabled everywhere with the screen asleep, so only display_attached marks it).
func threeCards() []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: uA, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: uB, Name: "NVIDIA GeForce RTX 5070 Ti", TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: uC, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	cards, _ := gpuprobe.BuildCards(devs, "")
	return cards
}

func scratchManager(t *testing.T) *gpulease.Manager {
	t.Helper()
	m, err := gpulease.OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	return m
}

func hostDeps(cards []gpuprobe.Card, away bool) Deps {
	return Deps{
		Cards:       func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return cards, "", nil },
		HostFreeRAM: func() (float64, bool) { return 64, true },
		Presence:    func(config.Config) (bool, bool) { return true, away },
	}
}

func TestBuildInputReadsLeasesAndTheCallersOwnClaims(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "held", Devices: []string{cards[0].LeaseID()}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	in, err := BuildInput(context.Background(), m, config.Config{}, Need{VRAMGiB: 4, RAMGiB: 8, Claimed: map[string]bool{cards[2].LeaseID(): true}}, hostDeps(cards, false))
	if err != nil {
		t.Fatal(err)
	}
	if !in.Claimed[cards[0].LeaseID()] || !in.Claimed[cards[2].LeaseID()] || in.Claimed[cards[1].LeaseID()] {
		t.Errorf("claimed = %v, want the leased card and the caller's, not the third", in.Claimed)
	}
	if in.WholeNodeHeld {
		t.Error("a card-scoped lease is not a whole-node lease")
	}
	if in.FootprintGiB != 4 || in.HostNeedGiB != 8 || !in.HostFreeOK || in.HostFreeGiB != 64 {
		t.Errorf("needs/host not carried: %+v", in)
	}
	if in.AllowDisplay {
		t.Error("the display card is not allocatable while the operator is at the desk")
	}
	if len(in.Cards) != 3 || !in.Cards[1].Display {
		t.Errorf("the attached card must be the display card in the table: %+v", in.Cards)
	}
}

func TestBuildInputAWholeNodeLeaseClaimsEveryCard(t *testing.T) {
	m := scratchManager(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "whole", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	in, err := BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(threeCards(), false))
	if err != nil || !in.WholeNodeHeld {
		t.Fatalf("err=%v WholeNodeHeld=%v, want a whole-node lease read as held", err, in.WholeNodeHeld)
	}
}

func TestBuildInputNamesAFailedCardTable(t *testing.T) {
	m := scratchManager(t)
	d := hostDeps(nil, false)
	d.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("nvidia-smi: not on PATH")
	}
	_, err := BuildInput(context.Background(), m, config.Config{}, Need{}, d)
	if err == nil || !strings.Contains(err.Error(), "the card table") || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v, want the card-table failure named", err)
	}
}

func TestBuildInputOperatorAwayAllowsTheDisplayCard(t *testing.T) {
	m := scratchManager(t)
	in, err := BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(threeCards(), true))
	if err != nil || !in.AllowDisplay {
		t.Fatalf("err=%v AllowDisplay=%v, want the display card allowed when the operator is known to be away", err, in.AllowDisplay)
	}
	known := hostDeps(threeCards(), true)
	known.Presence = func(config.Config) (bool, bool) { return false, true }
	in, _ = BuildInput(context.Background(), m, config.Config{}, Need{}, known)
	if in.AllowDisplay {
		t.Error("an unknown presence must never allow the display card")
	}
}

// The reason this package exists: the allocator, fed by the shared builder, never hands the
// card the monitor is attached to to an unnamed request while the operator is at the desk.
func TestPickAutoNeverTakesTheAttachedCardAtTheDesk(t *testing.T) {
	m := scratchManager(t)
	build := func() (gpulease.AllocInput, error) {
		return BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(threeCards(), false))
	}
	var out bytes.Buffer
	ids, free, err := PickAuto(Plan{Min: 2, Max: 2}, time.Minute, build, &out, func(time.Duration) { t.Error("must not wait") }, time.Now)
	if err != nil || !free || len(ids) != 2 {
		t.Fatalf("ids=%v free=%v err=%v, want two free cards", ids, free, err)
	}
	for _, id := range ids {
		if id == threeCards()[1].LeaseID() {
			t.Fatalf("the attached (display) card %s was handed out: %v", id, ids)
		}
	}
	// Three cards asked for with the display excluded: the allocator cannot, and nothing waiting
	// for a lease would change that, so it is an error once the wait is spent.
	_, _, err = PickAuto(Plan{Min: 3, Max: 3, Hint: "; HINT"}, 0, build, &out, func(time.Duration) {}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "; HINT") {
		t.Fatalf("err = %v, want a refusal carrying the caller's hint", err)
	}
}

func TestPickAutoQueuesOnTheClaimedCardsWhenNoneIsFree(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	build := func() (gpulease.AllocInput, error) {
		return BuildInput(context.Background(), m, config.Config{}, Need{Claimed: map[string]bool{cards[0].LeaseID(): true, cards[2].LeaseID(): true}}, hostDeps(cards, false))
	}
	var out bytes.Buffer
	ids, free, err := PickAuto(Plan{Min: 1, Max: 1}, time.Minute, build, &out, func(time.Duration) { t.Error("a place in line is not a wait for cards to qualify") }, time.Now)
	if err != nil || free || len(ids) != 1 {
		t.Fatalf("ids=%v free=%v err=%v, want a queue target", ids, free, err)
	}
	if ids[0] != cards[0].LeaseID() && ids[0] != cards[2].LeaseID() {
		t.Errorf("queued on %s, want one of the two claimed non-display cards", ids[0])
	}
}

func TestResidentFromMapsLoadedSeatsToTheirCards(t *testing.T) {
	cards := threeCards()
	cfg := config.Config{Layers: []config.LayerSpec{{Name: "single", Seats: []config.LayerSeat{
		{Role: "agent", Model: "seat-a", Device: "0", FootprintGiB: 9},
		{Role: "vision", Model: "seat-v", Device: "2", FootprintGiB: 5},
		{Role: "stt", Model: "seat-cold", Device: "2", FootprintGiB: 3},
		{Role: "ocr", Model: "seat-pair", Device: "0,2", FootprintGiB: 2},
	}}}}
	got := ResidentFrom(cfg, cards, map[string]bool{"seat-a": true, "seat-v": true, "seat-pair": true})
	if r := got[cards[0].LeaseID()]; len(r.Seats) != 2 || r.CostGiB != 11 {
		t.Errorf("card 0 = %+v, want seat-a and seat-pair costing 11", r)
	}
	if r := got[cards[2].LeaseID()]; len(r.Seats) != 2 || r.CostGiB != 7 {
		t.Errorf("card 2 = %+v, want seat-v and seat-pair costing 7 (the unloaded seat-cold does not count)", r)
	}
	if _, ok := got[cards[1].LeaseID()]; ok {
		t.Error("nothing sits on card 1")
	}
}

func scopedCfg() config.Config {
	return config.Config{MemoryStack: []string{"embed"}, Layers: []config.LayerSpec{{Name: "single", Seats: []config.LayerSeat{
		{Role: "agent", Model: "on-card-0", Device: "0"},
		{Role: "vision", Model: "on-card-2", Device: "2"},
		{Role: "ocr", Model: "pair", Device: "0,2"},
	}}}}
}

// The rule the pipeline relies on so that a render on card 2 does not empty the seats on card 0
// (register C-86): the list a card-2 lease may unload is the roster minus the memory stack
// minus the seats pinned only to other cards.
func TestUnloadModelsKeepsSeatsOnOtherCardsAndTheMemoryStack(t *testing.T) {
	cards := threeCards()
	roster := []string{"on-card-0", "on-card-2", "pair", "embed", "unpinned"}
	got, ok := UnloadModels(context.Background(), scopedCfg(), roster, []string{cards[2].LeaseID()}, hostDeps(cards, false), nil)
	if !ok {
		t.Fatal("a card lease has a list")
	}
	want := map[string]bool{"on-card-2": true, "pair": true, "unpinned": true}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want exactly %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("%q must not be unloaded by a card-2 lease: list %v", id, got)
		}
	}
	if _, ok := UnloadModels(context.Background(), scopedCfg(), roster, nil, hostDeps(cards, false), nil); ok {
		t.Error("a whole-node lease has no list: the render lane keeps its own rule")
	}
	if _, ok := UnloadModels(context.Background(), scopedCfg(), nil, []string{cards[2].LeaseID()}, hostDeps(cards, false), nil); ok {
		t.Error("an empty roster has no list")
	}
}

func TestUnloadModelsWithoutACardTableTreatsEverySeatAsOnTheLeasedCards(t *testing.T) {
	d := hostDeps(nil, false)
	d.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("no smi")
	}
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), scopedCfg(), []string{"on-card-0", "on-card-2", "embed"}, []string{"gpu-cccc3333-dddd-eeee-ffff-000000000000"}, d, &warn)
	if !ok || len(got) != 2 {
		t.Fatalf("list = %v ok=%v, want both seats (the doubt fences), the memory stack still kept", got, ok)
	}
	if !strings.Contains(warn.String(), "card table could not be read") {
		t.Errorf("the doubt must be said: %q", warn.String())
	}
}

func TestUnloadEnvRendersTheThreeStates(t *testing.T) {
	for _, tc := range []struct {
		models []string
		known  bool
		want   string
	}{
		{[]string{"a", "b"}, true, "GPU_LEASE_UNLOAD_MODELS=a,b"},
		{nil, true, "GPU_LEASE_UNLOAD_MODELS=-"},
		{nil, false, ""},
	} {
		if got := UnloadEnv(tc.models, tc.known); got != tc.want {
			t.Errorf("UnloadEnv(%v, %v) = %q, want %q", tc.models, tc.known, got, tc.want)
		}
	}
}

func TestEffectiveMemoryStackFallsBackToTheDefault(t *testing.T) {
	if got := EffectiveMemoryStack(config.Config{}); len(got) == 0 {
		t.Fatal("the default memory stack must not be empty")
	}
	if got := EffectiveMemoryStack(config.Config{MemoryStack: []string{"x"}}); len(got) != 1 || got[0] != "x" {
		t.Fatalf("a configured stack wins: %v", got)
	}
}

// llama-swap's /running, as a stub: a loaded seat, one on its way out, a stopped one.
func runningStub(t *testing.T) config.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"running":[{"model":"seat-a","state":"ready"},{"model":"seat-v","state":"shutdown"},{"model":"seat-cold","state":"stopped"}]}`))
	}))
	t.Cleanup(srv.Close)
	cfg := scopedCfg()
	cfg.Endpoint = srv.URL
	cfg.Layers = []config.LayerSpec{{Name: "single", Seats: []config.LayerSeat{
		{Role: "agent", Model: "seat-a", Device: "0", FootprintGiB: 9},
		{Role: "vision", Model: "seat-v", Device: "2", FootprintGiB: 5},
		{Role: "stt", Model: "seat-cold", Device: "2", FootprintGiB: 3},
	}}}
	return cfg
}

// A seat that is stopping or stopped holds nothing a new job would evict, on either way of
// reading /running (the reserve verb's shared client, or the llama-swap client).
func TestResidentSeatsCountsOnlyTheSeatsThatAreLoaded(t *testing.T) {
	cfg := runningStub(t)
	cards := threeCards()
	for name, client := range map[string]*http.Client{"llama-swap client": nil, "shared client": http.DefaultClient} {
		t.Run(name, func(t *testing.T) {
			got := ResidentSeats(context.Background(), cfg, cards, client)
			if r := got[cards[0].LeaseID()]; len(r.Seats) != 1 || r.Seats[0] != "seat-a" || r.CostGiB != 9 {
				t.Errorf("card 0 = %+v, want the loaded seat-a (9 GiB)", r)
			}
			if r, ok := got[cards[2].LeaseID()]; ok {
				t.Errorf("card 2 = %+v, want nothing: its seats are shutting down or stopped", r)
			}
		})
	}
}

func TestResidentSeatsIsEmptyWhenLlamaSwapCannotBeRead(t *testing.T) {
	cfg := scopedCfg()
	cfg.Endpoint = "http://127.0.0.1:1" // nothing listens
	if got := ResidentSeats(context.Background(), cfg, threeCards(), nil); len(got) != 0 {
		t.Fatalf("an unreadable /running contributes nothing, got %v", got)
	}
	if got := ResidentSeats(context.Background(), config.Config{}, threeCards(), nil); len(got) != 0 {
		t.Fatalf("no layers, nothing resident: %v", got)
	}
}
