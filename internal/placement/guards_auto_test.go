package placement

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// display_device "auto" names no card: the display_floor guard asks the driver which card drives the
// monitor and guards that one (the same reading the card table and the allocator's 60 % cap use,
// gpuprobe.ScreenCardIndexes / Device.DrivesDisplay). A literal index and a UUID keep their old paths:
// they are the operator's declaration, cross-checked against the driver (guards_screen_test.go).

// autoLayerAndSeat is the fixture's display layer with display_device "auto" and its twin seat pinned to
// pin. Only the display_floor guard stays, so a case reads the floor and nothing else.
func autoLayerAndSeat(t *testing.T, pin string) (config.LayerSpec, config.LayerSeat) {
	t.Helper()
	l, s := displayLayerAndSeat(t)
	l.DisplayDevice = "auto"
	l.Guards = []string{"display_floor"}
	s.Device = pin
	return l, s
}

func TestAutoDisplayDeviceFollowsTheDriversMonitorCard(t *testing.T) {
	for _, monitor := range []string{"0", "1", "2"} {
		t.Run("monitor on "+monitor, func(t *testing.T) {
			l, s := autoLayerAndSeat(t, monitor)
			f := admitting()
			f.free[monitor] = 15 // 15 - 6.2 = 8.8 >= 4
			live := f.live()
			live.ScreenCards = screenAt(monitor)
			ok, reason, guard := LayerAdmissible(l, s, live, nil)
			if !ok {
				t.Fatalf("auto must follow the monitor to index %s and admit, got guard %q: %s", monitor, guard, reason)
			}
			want := "free 15.0 GiB − footprint 6.2 = 8.8 ≥ floor 4.0 on display device auto (index " + monitor + ")"
			if !strings.Contains(reason, want) {
				t.Errorf("the reading names the derived card and its arithmetic:\n got %q\nwant %q", reason, want)
			}
		})
	}
}

func TestAutoDisplayDeviceRefusesWhenTheDriverCannotNameOneCard(t *testing.T) {
	cases := []struct {
		name   string
		screen func() []string
		want   []string
	}{
		{"no reader", nil, []string{"display_device auto", "no reader is available"}},
		{"no card is flagged, or the reading cannot say", screenAt(), []string{"display_device auto", "does not say which card drives the monitor"}},
		{"two cards drive a monitor", screenAt("0", "2"), []string{"display_device auto", "indexes 0, 2", "does not choose between cards"}},
		{"every card drives one", screenAt("0", "1", "2"), []string{"display_device auto", "indexes 0, 1, 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, s := autoLayerAndSeat(t, "0")
			f := admitting()
			f.free["0"], f.free["1"], f.free["2"] = 15, 15, 15 // room everywhere: only the card is in doubt
			live := f.live()
			live.ScreenCards = tc.screen
			ok, reason, guard := LayerAdmissible(l, s, live, nil)
			if ok || guard != "display_floor" {
				t.Fatalf("auto with no single card to follow must refuse by display_floor, got ok=%v guard=%q %q", ok, guard, reason)
			}
			for _, w := range tc.want {
				if !strings.Contains(reason, w) {
					t.Errorf("the refusal must say %q, got %q", w, reason)
				}
			}
			if ok, reasons := ResidentVerdict(l, live); ok || !strings.Contains(strings.Join(reasons, " "), "display_device auto") {
				t.Errorf("a loaded twin is held to the same reading, got ok=%v %v", ok, reasons)
			}
		})
	}
}

func TestAutoDisplayFloorIsJudgedOnTheDerivedCardsFreeVRAM(t *testing.T) {
	// The monitor is on 0 and card 0 has 3.0 GiB free: 3.0 - 6.2 < the floor. Card 1, which the
	// seeded layer used to name, has plenty, and must not be what the guard reads.
	l, s := autoLayerAndSeat(t, "0")
	f := admitting()
	f.free["0"], f.free["1"] = 3.0, 15
	live := f.live()
	live.ScreenCards = screenAt("0")
	ok, reason, guard := LayerAdmissible(l, s, live, nil)
	if ok || guard != "display_floor" {
		t.Fatalf("3.0 GiB free on the monitor's card must refuse by display_floor, got ok=%v guard=%q %q", ok, guard, reason)
	}
	for _, w := range []string{"free 3.0 GiB", "floor 4.0", "display device auto (index 0)"} {
		if !strings.Contains(reason, w) {
			t.Errorf("the refusal must carry %q, got %q", w, reason)
		}
	}

	// The monitor on 1: the same free numbers now admit, because the card read is 1's.
	l, s = autoLayerAndSeat(t, "1")
	live = f.live()
	live.ScreenCards = screenAt("1")
	if ok, reason, _ := LayerAdmissible(l, s, live, nil); !ok {
		t.Fatalf("with the monitor on 1 (15.0 GiB free) the layer admits, got %q", reason)
	}
}

func TestAutoDisplayRefusesASeatPinnedOffTheDerivedCard(t *testing.T) {
	// A seat that declares display_footprint_gib is meant to sit on the display card. With the monitor
	// on 0 and the twin still pinned to 1, a floor judged on card 0 would admit a twin that loads on
	// card 1, beside whatever the compute card is running. The pin must follow the monitor.
	l, s := autoLayerAndSeat(t, "1")
	f := admitting()
	f.free["0"], f.free["1"] = 15, 15
	live := f.live()
	live.ScreenCards = screenAt("0")
	ok, reason, guard := LayerAdmissible(l, s, live, nil)
	if ok || guard != "display_floor" {
		t.Fatalf("a twin pinned to 1 with the monitor on 0 must refuse by display_floor, got ok=%v guard=%q %q", ok, guard, reason)
	}
	for _, w := range []string{"seat router declares display_footprint_gib 6.2", "pin 1", "index 0", "re-pin"} {
		if !strings.Contains(reason, w) {
			t.Errorf("the refusal must say %q, got %q", w, reason)
		}
	}
	// A seat that carries a model is named by it.
	named := config.LayerSeat{Role: "agent", Model: "gemma-4-e4b-display", Device: "1", DisplayFootprintGiB: 5}
	live.ScreenCards = screenAt("0")
	if ok, reason, _ := LayerAdmissible(l, named, live, nil); ok || !strings.Contains(reason, "seat gemma-4-e4b-display declares display_footprint_gib 5.0") {
		t.Fatalf("a seat with a model is named by it, got ok=%v %q", ok, reason)
	}

	// The monitor on 1 is the seeded arrangement: admitted.
	live.ScreenCards = screenAt("1")
	if ok, reason, _ := LayerAdmissible(l, s, live, nil); !ok {
		t.Fatalf("the pin includes the monitor's card: admitted, got %q", reason)
	}

	// The rule keys on the declared footprint. A seat that declares none is off the display card by
	// its own account and is judged free - 0, as a seat off a literal card always was.
	heavy := config.LayerSeat{Role: "agent", Model: "m", Device: "1", CtxTokens: 1000}
	live.ScreenCards = screenAt("0")
	ok, reason, _ = LayerAdmissible(l, heavy, live, nil)
	if !ok || !strings.Contains(reason, "footprint 0.0") {
		t.Fatalf("a seat with no display footprint pinned off the card is judged free - 0, got ok=%v %q", ok, reason)
	}

	// A pin that spans the derived card is on it: judged with its footprint.
	span := config.LayerSeat{Role: "long", Model: "m", Device: "0,1", CtxTokens: 1000, DisplayFootprintGiB: 10.5}
	ok, reason, _ = LayerAdmissible(l, span, live, nil)
	if !ok || !strings.Contains(reason, "footprint 10.5") {
		t.Fatalf("a pin including the monitor's card is judged with its footprint, got ok=%v %q", ok, reason)
	}

	// A UUID pin nothing can resolve counts as on the card (seatOnIndex), so the coherence rule does
	// not refuse it and the arithmetic uses its footprint.
	uuidPinned := config.LayerSeat{Role: "router", Device: "GPU-zzzz0000", DisplayFootprintGiB: 6.2}
	ok, reason, _ = LayerAdmissible(l, uuidPinned, live, nil)
	if !ok || !strings.Contains(reason, "footprint 6.2") {
		t.Fatalf("an unresolvable pin may be on the card: footprint applies, got ok=%v %q", ok, reason)
	}
}

func TestAutoDisplayStillRefusesAnUndeclaredFootprintOnTheDerivedCard(t *testing.T) {
	l, s := autoLayerAndSeat(t, "0")
	s.DisplayFootprintGiB = 0
	f := admitting()
	f.free["0"] = 15
	live := f.live()
	live.ScreenCards = screenAt("0")
	ok, reason, guard := LayerAdmissible(l, s, live, nil)
	if ok || guard != "display_floor" || !strings.Contains(reason, "display footprint undeclared") ||
		!strings.Contains(reason, "includes display device auto → index 0") {
		t.Fatalf("a seat on the derived card owes its footprint (never free - 0), got ok=%v guard=%q %q", ok, guard, reason)
	}
}

// The literal-index and UUID paths are the operator's declaration and keep their exact words: this runs
// them in one config next to an auto layer and pins the strings, so the new branch cannot swallow them.
func TestAutoDisplayLeavesLiteralIndexAndUUIDBehaviourAlone(t *testing.T) {
	auto, autoSeat := autoLayerAndSeat(t, "0")

	// A literal "1" and a monitor on 1: the old reading, word for word.
	lit, litSeat := displayLayerAndSeat(t)
	f := admitting()
	f.free["0"] = 15
	f.index = map[string]string{"GPU-8888bbbb": "1"}
	f.free["GPU-8888bbbb"] = 15
	live := f.live()
	live.ScreenCards = screenAt("1")
	ok, reason, _ := LayerAdmissible(lit, litSeat, live, nil)
	if !ok || !strings.Contains(reason, "display_floor: free 15.0 GiB − footprint 6.2 = 8.8 ≥ floor 4.0 on display device 1 (index 1)") {
		t.Fatalf("literal index, monitor on 1: got ok=%v %q", ok, reason)
	}
	// The same config, the monitor on 0: the literal layer refuses with its old sentence; the auto layer
	// follows the monitor and admits.
	live.ScreenCards = screenAt("0")
	ok, reason, guard := LayerAdmissible(lit, litSeat, live, nil)
	const old = "display_floor: display_device 1 is CUDA index 1, but the driver reports the monitor on index 0 (display_active / display_attached) — refused: the floor would guard a card that is not the desktop's until display_device names the one that drives the screen"
	if ok || guard != "display_floor" || reason != old {
		t.Fatalf("literal index, monitor on 0 must keep its sentence:\n got %q\nwant %q", reason, old)
	}
	if ok, reason, _ := LayerAdmissible(auto, autoSeat, live, nil); !ok {
		t.Fatalf("the auto layer next to it follows the monitor to 0, got %q", reason)
	}

	// The seat-pin rule is the sentinel's alone. On a declared card a seat that declares a footprint but
	// is pinned elsewhere is judged free - 0, as it always was.
	offCard := litSeat
	offCard.Device = "0"
	live.ScreenCards = screenAt("1")
	ok, reason, _ = LayerAdmissible(lit, offCard, live, nil)
	if !ok || !strings.Contains(reason, "display_floor: free 15.0 GiB − footprint 0.0 = 15.0 ≥ floor 4.0 on display device 1 (index 1)") {
		t.Fatalf("literal index, seat pinned off the card: got ok=%v %q", ok, reason)
	}

	// A UUID pin resolves through the probe and prints as itself.
	uuid, uuidSeat := displayLayerAndSeat(t)
	uuid.DisplayDevice = "GPU-8888bbbb"
	live.ScreenCards = screenAt("1")
	ok, reason, _ = LayerAdmissible(uuid, uuidSeat, live, nil)
	if !ok || !strings.Contains(reason, "on display device GPU-8888bbbb (index 1)") {
		t.Fatalf("UUID pin: got ok=%v %q", ok, reason)
	}

	// A typo is not the sentinel: it is a UUID prefix nothing matches, and it fails closed.
	typo, typoSeat := displayLayerAndSeat(t)
	typo.DisplayDevice = "atuo"
	ok, reason, guard = LayerAdmissible(typo, typoSeat, live, nil)
	if ok || guard != "display_floor" || !strings.Contains(reason, "display device atuo cannot be resolved to a CUDA index") {
		t.Fatalf("a typo must refuse by name, got ok=%v guard=%q %q", ok, guard, reason)
	}
}

// Where no reader exists the messages still name the device, and "auto" reads as "auto" there too, not as
// a raw config string a reader cannot connect to the sentinel; the literal spelling is unchanged.
func TestAutoDisplayDeviceIsLabelledWhereNoReaderExists(t *testing.T) {
	auto, autoSeat := autoLayerAndSeat(t, "0")
	lit, litSeat := displayLayerAndSeat(t)
	lit.Guards = []string{"display_floor"}

	ok, reason, _ := LayerAdmissible(auto, autoSeat, Live{}, nil)
	if ok || reason != "display_floor: free VRAM on display device auto unreadable (no reader) — refused" {
		t.Fatalf("auto, no reader: got ok=%v %q", ok, reason)
	}
	ok, reason, _ = LayerAdmissible(lit, litSeat, Live{}, nil)
	if ok || reason != "display_floor: free VRAM on display device 1 unreadable (no reader) — refused" {
		t.Fatalf("literal, no reader: got ok=%v %q", ok, reason)
	}

	ok, reasons := ResidentVerdict(auto, Live{})
	if ok || strings.Join(reasons, "|") != "display_floor: free VRAM on display device auto unreadable (no reader) — refused" {
		t.Fatalf("auto, resident, no reader: got ok=%v %v", ok, reasons)
	}
	ok, reasons = ResidentVerdict(lit, Live{})
	if ok || strings.Join(reasons, "|") != "display_floor: free VRAM on display device 1 unreadable (no reader) — refused" {
		t.Fatalf("literal, resident, no reader: got ok=%v %v", ok, reasons)
	}

	// However the sentinel is spelled it prints as itself, in every message; a literal declaration prints
	// as the operator wrote it, whitespace and all, as it always did.
	shout, shoutSeat := autoLayerAndSeat(t, "0")
	shout.DisplayDevice = " AUTO "
	ok, reason, _ = LayerAdmissible(shout, shoutSeat, Live{}, nil)
	if ok || reason != "display_floor: free VRAM on display device auto unreadable (no reader) — refused" {
		t.Fatalf("\" AUTO \", no reader: got ok=%v %q", ok, reason)
	}
	ok, reasons = ResidentVerdict(shout, Live{})
	if ok || strings.Join(reasons, "|") != "display_floor: free VRAM on display device auto unreadable (no reader) — refused" {
		t.Fatalf("\" AUTO \", resident, no reader: got ok=%v %v", ok, reasons)
	}
	padded := lit
	padded.DisplayDevice = " 1 "
	ok, reason, _ = LayerAdmissible(padded, litSeat, Live{}, nil)
	if ok || reason != "display_floor: free VRAM on display device  1  unreadable (no reader) — refused" {
		t.Fatalf("\" 1 \", no reader: got ok=%v %q", ok, reason)
	}

	// A card the probe does not list: the label is the sentinel, the number is the derived card's.
	f := admitting()
	delete(f.free, "0")
	live := f.live()
	live.ScreenCards = screenAt("0")
	ok, reason, _ = LayerAdmissible(auto, autoSeat, live, nil)
	if ok || !strings.Contains(reason, "free VRAM on display device auto unreadable (card not in the probe)") {
		t.Fatalf("auto, card not in the probe: got ok=%v %q", ok, reason)
	}
}

// A delegator evaluating a remote node's rows holds no device reader: the node's own verdict answers
// before the sentinel is resolved, so a node on this build and a delegator on an older one still agree.
func TestAutoDisplayLayerIsAnsweredByTheNodesVerdictWhereTheDelegatorHasNoReader(t *testing.T) {
	l, s := autoLayerAndSeat(t, "0")
	yes, no := true, false
	if ok, reason, _ := LayerAdmissible(l, s, Live{}, &yes); !ok || !strings.Contains(reason, "node's own verdict") {
		t.Fatalf("the carried verdict stands in: got ok=%v %q", ok, reason)
	}
	if ok, _, guard := LayerAdmissible(l, s, Live{}, &no); ok || guard != "display_floor" {
		t.Fatalf("a refused verdict refuses by the guard, got ok=%v guard=%q", ok, guard)
	}
}

// The node publishes the verdict its own driver reading gives, so a delegator is told the truth: an auto
// layer whose twin sits on the monitor's card is admissible, and the same layer with the twin on the
// other card is not, and says which pin is stale.
func TestANodePublishesAnAutoDisplayLayerFollowingTheMonitor(t *testing.T) {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa1111", FreeGiB: 14, DisplayAttached: true}, // the monitor is on 0
		{Index: 1, UUID: "GPU-bbbb2222", FreeGiB: 15},
		{Index: 2, UUID: "GPU-cccc3333", FreeGiB: 14},
	}
	host := 80.0
	pres := Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"}
	for _, tc := range []struct {
		pin    string
		admit  bool
		wantIn string
	}{
		{"0", true, "display device auto (index 0)"},
		{"1", false, "pin 1"},
	} {
		cfg := config.CompositeFixture()
		for i := range cfg.Layers {
			if cfg.Layers[i].Name != "display" {
				continue
			}
			cfg.Layers[i].Dormant = false
			cfg.Layers[i].DisplayDevice = "auto"
			cfg.Layers[i].Guards = []string{"display_floor"} // presence needs the watcher, which a sample does not carry
			cfg.Layers[i].Seats[0].Device = tc.pin
		}
		found := false
		for _, r := range RowsFromConfig(cfg, LiveFromReadings(devs, &host, &pres)) {
			if r.Name != "display" {
				continue
			}
			found = true
			if r.Admissible != tc.admit || !strings.Contains(r.Reason, tc.wantIn) {
				t.Errorf("twin pinned to %s: admissible=%v reason %q, want admissible=%v and %q", tc.pin, r.Admissible, r.Reason, tc.admit, tc.wantIn)
			}
			if r.DisplayDevice != "auto" {
				t.Errorf("the row carries the declaration as written, got %q", r.DisplayDevice)
			}
		}
		if !found {
			t.Fatal("no display row")
		}
	}
}

// "Auto" derives its card from the driver's screen reading (ScreenCardIndexes), which does not carry the
// card table's own guards: BuildCards flags NO card as the display card on a box whose only card drives
// the monitor, or whose screen card has no UUID to key it by. The two disagree there, harmlessly for the
// 3-card reference box, and this pins it so a change to either is a decision: auto still names the card
// the driver reports, and the table's Display flag is not what it reads.
func TestAutoDisplayReadsTheScreenReadingEvenWhereTheCardTableFlagsNoCard(t *testing.T) {
	cases := []struct {
		name string
		devs []gpuprobe.Device
		card string
	}{
		{"a box whose only card drives the monitor", []gpuprobe.Device{
			{Index: 0, UUID: "GPU-aaaa1111", TotalGiB: 16, FreeGiB: 15, DisplayAttached: true},
		}, "0"},
		{"a screen card with no UUID to key it by", []gpuprobe.Device{
			{Index: 0, UUID: "GPU-aaaa1111", TotalGiB: 16, FreeGiB: 15},
			{Index: 1, UUID: "", TotalGiB: 16, FreeGiB: 15, DisplayAttached: true},
			{Index: 2, UUID: "GPU-cccc3333", TotalGiB: 16, FreeGiB: 15},
		}, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cards, _ := gpuprobe.BuildCards(tc.devs, "")
			for _, c := range cards {
				if c.Display {
					t.Fatalf("premise: the card table flags no display card here, but index %d is flagged", c.NvidiaIndex)
				}
			}
			l, s := autoLayerAndSeat(t, tc.card)
			host := 80.0
			pres := Presence{Mode: "away", Known: true, Away: true}
			ok, reason, _ := LayerAdmissible(l, s, LiveFromReadings(tc.devs, &host, &pres), nil)
			if !ok || !strings.Contains(reason, "display device auto (index "+tc.card+")") {
				t.Fatalf("auto follows the driver's screen reading to index %s, got ok=%v %q", tc.card, ok, reason)
			}
		})
	}
}
