package gpualloc

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// roomyHostMemory is a host with room for anything these tests ask of it.
var roomyHostMemory = gpuprobe.HostMemory{PhysicalGiB: 256, AvailableGiB: 200, CommitUsedGiB: 40, CommitLimitGiB: 400}

func scratchManager(t *testing.T) *gpulease.Manager {
	t.Helper()
	m, err := gpulease.OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	return m
}

func hostDeps(cards []gpuprobe.Card) Deps {
	return Deps{
		Cards:      func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return cards, "", nil },
		HostMemory: func() (gpuprobe.HostMemory, bool) { return roomyHostMemory, true },
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

	in, err := BuildInput(context.Background(), m, config.Config{}, Need{VRAMGiB: 4, RAMGiB: 8, Claimed: map[string]bool{cards[2].LeaseID(): true}}, hostDeps(cards))
	if err != nil {
		t.Fatal(err)
	}
	if !in.Claimed[cards[0].LeaseID()] || !in.Claimed[cards[2].LeaseID()] || in.Claimed[cards[1].LeaseID()] {
		t.Errorf("claimed = %v, want the leased card and the caller's, not the third", in.Claimed)
	}
	if in.WholeNodeHeld {
		t.Error("a card-scoped lease is not a whole-node lease")
	}
	if in.FootprintGiB != 4 || in.HostNeedGiB != 8 || !in.HostMemOK || in.HostMem != roomyHostMemory {
		t.Errorf("needs/host not carried: %+v", in)
	}
	if in.DisplayCapPct != 60 {
		t.Errorf("a config that sets no gpu_display_cap_pct carries the default cap of 60 percent, got %d", in.DisplayCapPct)
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
	in, err := BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(threeCards()))
	if err != nil || !in.WholeNodeHeld {
		t.Fatalf("err=%v WholeNodeHeld=%v, want a whole-node lease read as held", err, in.WholeNodeHeld)
	}
}

func TestBuildInputNamesAFailedCardTable(t *testing.T) {
	m := scratchManager(t)
	d := hostDeps(nil)
	d.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("nvidia-smi: not on PATH")
	}
	_, err := BuildInput(context.Background(), m, config.Config{}, Need{}, d)
	if err == nil || !strings.Contains(err.Error(), "the card table") || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v, want the card-table failure named", err)
	}
}

// The display card is open up to the operator's cap at all times: BuildInput carries gpu_display_cap_pct
// and reads no presence, so the answer is the same whatever operator_presence says.
func TestBuildInputOpensTheDisplayCardAtTheCapWhateverThePresence(t *testing.T) {
	m := scratchManager(t)
	zero, thirtyFive := 0, 35
	for _, tc := range []struct {
		name     string
		presence string
		cap      *int
		want     int
	}{
		{"no presence set, no cap set", "", nil, 60},
		{"present", "present", nil, 60},
		{"away", "away", nil, 60},
		{"auto", "auto", nil, 60},
		{"a configured share", "present", &thirtyFive, 35},
		{"the operator needs the card", "away", &zero, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{OperatorPresence: tc.presence, GPUDisplayCapPct: tc.cap}
			// The pair seat is loaded on the two non-display cards, so the display card is the one with
			// nothing to evict and sorts first whenever the cap lets it in.
			cards := threeCards()
			deps := hostDeps(cards)
			deps.Resident = func(context.Context, config.Config, []gpuprobe.Card) map[string]gpulease.ResidentInfo {
				seat := gpulease.ResidentInfo{Seats: []string{"agent-pool"}, CostGiB: 15}
				return map[string]gpulease.ResidentInfo{cards[0].LeaseID(): seat, cards[2].LeaseID(): seat}
			}
			in, err := BuildInput(context.Background(), m, cfg, Need{VRAMGiB: 2}, deps)
			if err != nil {
				t.Fatal(err)
			}
			if in.DisplayCapPct != tc.want {
				t.Fatalf("DisplayCapPct = %d, want %d", in.DisplayCapPct, tc.want)
			}
			a, err := gulease2Allocate(in)
			if err != nil {
				t.Fatal(err)
			}
			display := threeCards()[1].LeaseID()
			if taken := len(a.Devices) == 1 && a.Devices[0] == display; taken != (tc.want > 0) {
				t.Fatalf("display card taken = %v with a cap of %d (a 2 GiB job on 2 GiB in use of 16, the pair seat on the other two): %v", taken, tc.want, a.Devices)
			}
		})
	}
}

func gulease2Allocate(in gpulease.AllocInput) (gpulease.Allocation, error) {
	in.Min, in.Max = 1, 1
	return gpulease.Allocate(in)
}

// The reason this package exists: the allocator, fed by the shared builder, hands the card the monitor is
// attached to to an unnamed request only where its declared footprint keeps the card under the cap (60 percent
// of 16 GiB = 9.6 GiB, with 2 in use: a job of up to 7.6 GiB), and never to a request that declares none.
func TestPickAutoTakesTheDisplayCardAtTheDeskOnlyWithinTheCap(t *testing.T) {
	m := scratchManager(t)
	display := threeCards()[1].LeaseID()
	pick := func(cfg config.Config, vram float64, n int) []string {
		t.Helper()
		build := func() (gpulease.AllocInput, error) {
			return BuildInput(context.Background(), m, cfg, Need{VRAMGiB: vram}, hostDeps(threeCards()))
		}
		ids, free, err := PickAuto(Plan{Min: n, Max: n}, 0, build, io.Discard, func(time.Duration) { t.Error("must not wait") }, time.Now)
		if err != nil || !free {
			t.Fatalf("vram %v n %d: ids=%v free=%v err=%v", vram, n, ids, free, err)
		}
		return ids
	}
	has := func(ids []string) bool {
		for _, id := range ids {
			if id == display {
				return true
			}
		}
		return false
	}
	if ids := pick(config.Config{}, 6, 2); !has(ids) {
		t.Errorf("a 6 GiB job on 2 GiB in use of 16 is under the 60 percent cap and the display card has no seat to evict: want it in %v", ids)
	}
	if ids := pick(config.Config{}, 8, 2); has(ids) {
		t.Errorf("a 8 GiB job makes 10 GiB in use on the display card, over the 9.6 GiB cap: it must not be handed out, got %v", ids)
	}
	if ids := pick(config.Config{}, 0, 2); has(ids) {
		t.Errorf("a request that declares no footprint cannot be shown to stay under the cap: %v", ids)
	}
	zero := 0
	if ids := pick(config.Config{GPUDisplayCapPct: &zero}, 6, 2); has(ids) {
		t.Errorf("a cap of 0 is the operator's switch: the display card must stay out of %v", ids)
	}
	// Three cards asked for with the display card over the cap: the allocator cannot, and nothing waiting
	// for a lease would change that, so it is an error once the wait is spent.
	build := func() (gpulease.AllocInput, error) {
		return BuildInput(context.Background(), m, config.Config{}, Need{VRAMGiB: 8}, hostDeps(threeCards()))
	}
	_, _, err := PickAuto(Plan{Min: 3, Max: 3, Hint: "; HINT"}, 0, build, io.Discard, func(time.Duration) {}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "; HINT") || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %v, want a refusal carrying the caller's hint and naming the cap", err)
	}
}

func TestPickAutoQueuesOnTheClaimedCardsWhenNoneIsFree(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	build := func() (gpulease.AllocInput, error) {
		return BuildInput(context.Background(), m, config.Config{}, Need{Claimed: map[string]bool{cards[0].LeaseID(): true, cards[2].LeaseID(): true}}, hostDeps(cards))
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
		// The memory stack's embedder sits on a leased card here: placed there and still never listed.
		{Role: "embedding", Model: "embed", Device: "2"},
	}}}}
}

// The rule the pipeline relies on so that a render on card 2 does not empty the seats on card 0
// (register C-86, F15): the list a card-2 lease may unload is the roster minus the memory stack
// minus every model POSITIVELY placed on another card. A model placed on a leased card goes, and
// so does a model nobody placed: its card is unknown, the render needs its card, and that is the
// direction every doubt took before F15. The lines the lane prints name each group with its reason.
func TestUnloadModelsKeepsSeatsPlacedOnOtherCardsAndTheMemoryStack(t *testing.T) {
	cards := threeCards()
	roster := []string{"on-card-0", "on-card-2", "pair", "embed", "unpinned"}
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), scopedCfg(), roster, nil, []string{cards[2].LeaseID()}, hostDeps(cards), &warn)
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
	out := warn.String()
	for _, line := range []string{
		"unloaded, placed on a leased card by layers: on-card-2, pair",
		"left resident, placed elsewhere by layers: on-card-0",
		"unloaded, placement unknown (serving_config_path is unset, so the serving config places no model): unpinned",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the lane must say %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "embed") {
		t.Errorf("the memory stack is neither unloaded nor judged:\n%s", out)
	}
	if _, ok := UnloadModels(context.Background(), scopedCfg(), roster, nil, nil, hostDeps(cards), nil); ok {
		t.Error("a whole-node lease has no list: it holds every card, so the render lane keeps its own rule")
	}
}

// A box that declares no layers and has no serving config has no placement for any model, so the
// answer is the one it always was: every model off the memory stack goes. The lane says why.
func TestUnloadModelsOnABoxThatPlacesNothingUnloadsEveryModelButTheStack(t *testing.T) {
	cards := threeCards()
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), config.Config{MemoryStack: []string{"embed"}}, []string{"a", "b", "embed"}, nil, []string{cards[2].LeaseID()}, hostDeps(cards), &warn)
	if !ok || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("list = %v ok=%v, want a and b", got, ok)
	}
	for _, want := range []string{"unloaded, placement unknown", "serving_config_path is unset", "a, b"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("the lane must say %q:\n%s", want, warn.String())
		}
	}
}

// A lease that names every card the box has holds the whole node whatever it is called: every model
// is on a leased card, so the list is every model off the memory stack.
func TestUnloadModelsOnALeaseThatHoldsEveryCardListsEveryModelButTheStack(t *testing.T) {
	cards := threeCards()
	all := []string{cards[0].LeaseID(), cards[1].LeaseID(), cards[2].LeaseID()}
	got, ok := UnloadModels(context.Background(), scopedCfg(), []string{"on-card-0", "b", "embed"}, nil, all, hostDeps(cards), nil)
	if !ok || len(got) != 2 || got[0] != "on-card-0" || got[1] != "b" {
		t.Fatalf("three cards of three: list = %v ok=%v, want on-card-0 and b (even the seat layers put on card 0; the memory stack never leaves)", got, ok)
	}
	one := cards[:1]
	got, ok = UnloadModels(context.Background(), config.Config{MemoryStack: []string{"embed"}}, []string{"a", "b", "embed"}, nil, []string{one[0].LeaseID()}, hostDeps(one), nil)
	if !ok || len(got) != 2 {
		t.Fatalf("the only card of a one-card box: list = %v ok=%v, want a and b", got, ok)
	}
	// Two of three is a card lease: the seat layers put on card 0 is placed on a leased card (0 and 1 are
	// held), and the seat on card 2 is placed elsewhere.
	got, ok = UnloadModels(context.Background(), scopedCfg(), []string{"on-card-0", "on-card-2", "embed"}, nil, all[:2], hostDeps(cards), nil)
	if !ok || len(got) != 1 || got[0] != "on-card-0" {
		t.Fatalf("two cards of three: list = %v ok=%v, want on-card-0 only", got, ok)
	}
}

// A roster that could not be read is not an empty roster. Nothing says what is resident, so a list built
// from the config alone is never complete: with layers only, a resident model neither the layers nor the
// serving config name would never be judged and would stay on the leased card, and on a box that
// declares nothing the list would be empty and the lane would unload nothing. The answer is no list
// (ok false) whatever the config declares: the render lane keeps its own rule, every model off the
// memory stack, and the lane says why. A roster handed along with the error is ignored.
func TestUnloadModelsWithAnUnreadableRosterMakesNoListAndSaysWhy(t *testing.T) {
	cards := threeCards()
	for name, cfg := range map[string]config.Config{
		"no layers and no serving config": {MemoryStack: []string{"embed"}},
		"layers only":                     scopedCfg(),
		"layers and a serving config":     servingCfg(t),
	} {
		for _, roster := range [][]string{nil, {"on-card-0", "stray-model", "embed"}} {
			var warn bytes.Buffer
			got, ok := UnloadModels(context.Background(), cfg, roster, errors.New("roster refused"), []string{cards[2].LeaseID()}, hostDeps(cards), &warn)
			if ok || got != nil {
				t.Errorf("%s, roster %v: list = %v ok=%v, want no list: the lane keeps its own rule", name, roster, got, ok)
			}
			if env := UnloadEnv(got, ok); env != "" {
				t.Errorf("%s, roster %v: env = %q, want the variable unset, never the none sentinel", name, roster, env)
			}
			for _, want := range []string{"roster could not be read", "roster refused", "own rule"} {
				if !strings.Contains(warn.String(), want) {
					t.Errorf("%s, roster %v: the lane must say %q:\n%s", name, roster, want, warn.String())
				}
			}
			if strings.Contains(warn.String(), "unloaded, ") || strings.Contains(warn.String(), "left resident, ") {
				t.Errorf("%s, roster %v: nothing is judged when the roster cannot be read:\n%s", name, roster, warn.String())
			}
		}
	}
}

// A roster that WAS read and is empty means nothing is resident: the card lease still has a list (the
// none sentinel on a box that places nothing), never the unset variable, and a seat placed on another
// card is never on it.
func TestUnloadModelsWithAnEmptyRosterThatWasReadStillHasAList(t *testing.T) {
	cards := threeCards()
	for _, roster := range [][]string{nil, {}} {
		got, ok := UnloadModels(context.Background(), config.Config{MemoryStack: []string{"embed"}}, roster, nil, []string{cards[2].LeaseID()}, hostDeps(cards), nil)
		if !ok || UnloadEnv(got, ok) != "GPU_LEASE_UNLOAD_MODELS=-" {
			t.Errorf("roster %v, nothing placed: list = %v ok=%v, want the none sentinel", roster, got, ok)
		}
		got, ok = UnloadModels(context.Background(), scopedCfg(), roster, nil, []string{cards[2].LeaseID()}, hostDeps(cards), nil)
		if !ok {
			t.Fatalf("roster %v: a card lease with a read roster has a list", roster)
		}
		for _, id := range got {
			if id == "on-card-0" || id == "embed" {
				t.Errorf("roster %v: %q must not be unloaded by a card-2 lease: list %v", roster, id, got)
			}
		}
	}
}

// With no card table nothing is placed, so every model goes, as before F15, and the lane says why
// once, with the error.
func TestUnloadModelsWithoutACardTableUnloadsEveryModelAndSaysWhy(t *testing.T) {
	d := hostDeps(nil)
	d.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("no smi")
	}
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), scopedCfg(), []string{"on-card-0", "on-card-2", "embed"}, nil, []string{"gpu-cccc3333-dddd-eeee-ffff-000000000000"}, d, &warn)
	if !ok || len(got) != 2 {
		t.Fatalf("list = %v ok=%v, want both seats (the doubt unloads), the memory stack still kept", got, ok)
	}
	for _, want := range []string{"card table could not be read", "no smi", "unloaded, placement unknown"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("the doubt must be said (%q missing): %q", want, warn.String())
		}
	}
	if n := strings.Count(warn.String(), "no smi"); n != 1 {
		t.Errorf("the reason is said once for the group, not once per model (%d times): %q", n, warn.String())
	}
}

// Locate places a model by layers first, then by its serving config pin; a doubt is not placed and
// reads as "on the leased cards" for Touches, and a model placed on another card does not touch.
func TestLeaseScopeLocateAndTouchesAgree(t *testing.T) {
	cards := threeCards()
	scope := NewLeaseScope(context.Background(), scopedCfg(), []string{cards[2].LeaseID()}, hostDeps(cards), nil)
	for _, tc := range []struct {
		model     string
		on, known bool
		by        string
		touches   bool
	}{
		{"on-card-2", true, true, "layers", true},
		{"pair", true, true, "layers", true},
		{"on-card-0", false, true, "layers", false},
		{"unpinned", false, false, "", true},
	} {
		l := scope.Locate(tc.model)
		if l.On != tc.on || l.Known != tc.known || l.By != tc.by {
			t.Errorf("Locate(%q) = %+v want on=%v known=%v by=%q", tc.model, l, tc.on, tc.known, tc.by)
		}
		if got := scope.Touches(tc.model); got != tc.touches {
			t.Errorf("Touches(%q) = %v, want %v", tc.model, got, tc.touches)
		}
	}
	whole := NewLeaseScope(context.Background(), scopedCfg(), nil, hostDeps(cards), nil)
	if l := whole.Locate("unpinned"); !l.On || !l.Known {
		t.Errorf("a whole-node lease holds every card: Locate = %+v", l)
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

// A host-RAM need no state of the host admits cannot be cured by waiting, so the allocator does not
// poll for it: it returns at once with the refusal, however long the caller was willing to wait, and
// it neither sleeps nor tells the line it is "waiting for that to change" (internal/gpulease/hostram.go).
func TestPickAutoDoesNotPollForAnImpossibleHostNeed(t *testing.T) {
	cards := threeCards()
	build := func() (gpulease.AllocInput, error) {
		return gpulease.AllocInput{
			Cards:     cards,
			HostMemOK: true, HostMem: gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 50, CommitUsedGiB: 20, CommitLimitGiB: 160},
			HostNeedGiB: 99, HostHeadroomGiB: 8, // 99 > 100 - 8: no state of this host admits it
		}, nil
	}
	var out bytes.Buffer
	sleeps := 0
	_, _, err := PickAuto(Plan{Min: 1, Max: 1}, time.Hour, build, &out, func(time.Duration) { sleeps++ }, time.Now)
	var none *gpulease.NoCardsError
	if !errors.As(err, &none) || !none.HostImpossible {
		t.Fatalf("want the impossible host refusal, got %v", err)
	}
	if sleeps != 0 || strings.Contains(out.String(), "waiting up to") {
		t.Fatalf("an impossible need must not be polled for: %d sleep(s), output %q", sleeps, out.String())
	}
	if strings.Contains(err.Error(), "--wait") {
		t.Fatalf("there is no --wait to suggest: %v", err)
	}

	// A host that is merely short IS waited for: the same request with a need that fits once the host frees.
	short := func() (gpulease.AllocInput, error) {
		in, _ := build()
		in.HostNeedGiB, in.HostMem.CommitUsedGiB = 30, 90
		return in, nil
	}
	sleeps = 0
	clock := time.Unix(1000, 0)
	_, _, err = PickAuto(Plan{Min: 1, Max: 1}, 10*time.Second, short, &out, func(d time.Duration) { sleeps++; clock = clock.Add(d) }, func() time.Time { return clock })
	if !errors.As(err, &none) || none.HostImpossible || sleeps == 0 {
		t.Fatalf("a shortage that waiting can cure is polled for (sleeps %d): %v", sleeps, err)
	}
}

// THE RUNAWAY OF 2026-10-10 (G3 of the P0 plan, step 2). A fix-round run of `go test -run TestPickAuto|TestQueuedClaims
// ./internal/gpualloc/` held 52 GiB private on the reference box and had to be killed. The cause was not a leak in
// the allocator: PickAuto's poll loop ends when `now()` passes the deadline, and the new test injected a sleep that
// does not sleep (it only recorded an error) with the REAL clock and a one-hour wait, against a PickAuto that still
// polled for the case under test. A poll loop whose sleep does not advance the clock it reads is a busy-spin, and
// every pass built a fresh AllocInput (maps, strings, a NoCardsError) and every pass appended to the test's log: for
// up to an hour. The loop is now also bounded by the number of polls the wait allows, whatever the clock does, so a
// caller that injects a stuck clock gets the same "fewer cards qualify" answer after at most wait/2s+2 passes instead
// of a spin; and this test is the bound.
func TestPickAutoPollsAreBoundedWhateverTheClockDoes(t *testing.T) {
	cards := threeCards() // the display card is the operator's: three cards can never qualify, and waiting does not change that
	builds := 0
	build := func() (gpulease.AllocInput, error) {
		builds++
		if builds > 5000 {
			t.Fatalf("PickAuto is still building inputs after %d passes: the poll loop is not bounded by the wait", builds)
		}
		return gpulease.AllocInput{Cards: cards, HostMemOK: true, HostMem: roomyHostMemory}, nil
	}
	frozen := time.Unix(1000, 0)
	sleeps := 0
	_, _, err := PickAuto(Plan{Min: 3, Max: 3}, time.Hour, build, io.Discard,
		func(time.Duration) { sleeps++ },   // a sleep that sleeps for nothing
		func() time.Time { return frozen }, // and a clock that never moves
	)
	var none *gpulease.NoCardsError
	if !errors.As(err, &none) {
		t.Fatalf("a stuck clock must end in the allocator's own answer, got %v", err)
	}
	if max := int(time.Hour/(2*time.Second)) + 2; builds > max {
		t.Fatalf("%d passes for a one-hour wait polled every 2s, want at most %d", builds, max)
	}
	if sleeps == 0 {
		t.Fatal("the loop never polled at all: this test no longer exercises the poll path")
	}
}
