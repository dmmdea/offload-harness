package placement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The floor protects the card the config NAMES. When the driver says the monitor is on a different
// card (the board reorders on a power loss, a cable moves), the floor would guard the wrong card
// while the desktop's own card is loaded, so the guard refuses and says which cards disagree.

func displayLayerAndSeat(t *testing.T) (config.LayerSpec, config.LayerSeat) {
	t.Helper()
	cfg := config.CompositeFixture()
	l, ok := cfg.Layer("display")
	if !ok {
		t.Fatal("fixture has no display layer")
	}
	s, ok := cfg.LayerSeat("display", "router")
	if !ok {
		t.Fatal("fixture display layer has no router seat")
	}
	return l, s
}

func screenAt(indexes ...string) func() []string {
	return func() []string { return indexes }
}

// screenCase is one reading of the driver's screen cards against a layer that declares card 1.
type screenCase struct {
	name   string
	screen func() []string // nil = the reader is absent
	admit  bool
}

// named is the cards the case's reader names, as the refusal prints them.
func (c screenCase) named() string { return strings.Join(c.screen(), ", ") }

func TestDisplayFloorCrossChecksTheCardTheDriverSaysDrivesTheMonitor(t *testing.T) {
	l, s := displayLayerAndSeat(t) // display_device "1", floor 4, footprint 6.2
	f := admitting()               // 15 GiB free on "1": 15 - 6.2 = 8.8 >= 4

	cases := []screenCase{
		{"no reader behaves as before the check existed", nil, true},
		{"the driver agrees", screenAt("1"), true},
		{"the reading cannot single a card out", screenAt(), true},
		{"a second monitor on another card does not contradict the declared one", screenAt("0", "1"), true},
		{"the monitor is on index 0", screenAt("0"), false},
		{"the monitor is on 0 and 2, never on the declared card", screenAt("0", "2"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := f.live()
			live.ScreenCards = tc.screen
			ok, reason, guard := LayerAdmissible(l, s, live, nil)
			if ok != tc.admit {
				t.Fatalf("admissible = %v, want %v (guard %q, reason %q)", ok, tc.admit, guard, reason)
			}
			if tc.admit {
				return
			}
			if guard != "display_floor" {
				t.Errorf("the refusal belongs to the display_floor guard, got %q", guard)
			}
			if !strings.Contains(reason, "index 1") || !strings.Contains(reason, "monitor on index "+tc.named()) {
				t.Errorf("the refusal must name BOTH cards, got %q", reason)
			}
		})
	}
}

// A UUID-pinned display_device (this box pins every seat by UUID) is resolved to its index first and
// then compared, so the pin is as protected as an index one.
func TestDisplayFloorCrossChecksAUUIDPinToo(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	l.DisplayDevice = "GPU-8888bbbb"
	f := admitting()
	f.index = map[string]string{"GPU-8888bbbb": "1"}
	f.free["GPU-8888bbbb"] = 15

	live := f.live()
	live.ScreenCards = screenAt("1")
	if ok, reason, _ := LayerAdmissible(l, s, live, nil); !ok {
		t.Fatalf("a UUID pin that resolves to the monitor's index must admit, got %q", reason)
	}
	live.ScreenCards = screenAt("2")
	ok, reason, guard := LayerAdmissible(l, s, live, nil)
	if ok || guard != "display_floor" {
		t.Fatalf("a UUID pin resolving to a card that is not the monitor's must refuse by guard name, got ok=%v guard=%q %q", ok, guard, reason)
	}
	if !strings.Contains(reason, "GPU-8888bbbb") || !strings.Contains(reason, "index 1") || !strings.Contains(reason, "monitor on index 2") {
		t.Errorf("the refusal names the pin, its index and the monitor's card, got %q", reason)
	}
}

// The check is a refusal before any arithmetic: a card with all the free VRAM in the world is still
// the wrong card to guard.
func TestTheScreenCardRefusalComesBeforeTheFloorArithmetic(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	f := admitting()
	f.free["1"] = 16
	live := f.live()
	live.ScreenCards = screenAt("0")
	if ok, reason, _ := LayerAdmissible(l, s, live, nil); ok || !strings.Contains(reason, "monitor on index 0") {
		t.Fatalf("plenty of free VRAM must not hide a mismatch, got ok=%v %q", ok, reason)
	}
}

// A remote row's verdict stands in only where a reader is nil, and the display card's number was
// read where the card is; the reader the node builds from its own sampler carries the screen cards.
func TestLiveFromReadingsCarriesTheScreenCardsOfItsOwnSample(t *testing.T) {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa1111", FreeGiB: 14},
		{Index: 1, UUID: "GPU-bbbb2222", FreeGiB: 12, DisplayAttached: true},
		{Index: 2, UUID: "GPU-cccc3333", FreeGiB: 14},
	}
	live := LiveFromReadings(devs, nil, nil)
	if live.ScreenCards == nil {
		t.Fatal("a node with a device sample must carry the screen-card reader")
	}
	if got := live.ScreenCards(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("ScreenCards = %v, want [1]", got)
	}
	if LiveFromReadings(nil, nil, nil).ScreenCards != nil {
		t.Fatal("no device sample, no reader: the guard then fails closed on its other inputs")
	}
}

// The node publishes its layer verdicts from LiveFromReadings, so a delegator is told the truth too:
// the display layer reads inadmissible, with the mismatch as the reason, when the monitor has moved.
func TestANodePublishesAMismatchedDisplayCardAsInadmissible(t *testing.T) {
	cfg := config.CompositeFixture()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = false
		}
	}
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa1111", FreeGiB: 14, DisplayAttached: true}, // the monitor has moved to 0
		{Index: 1, UUID: "GPU-bbbb2222", FreeGiB: 15},
		{Index: 2, UUID: "GPU-cccc3333", FreeGiB: 14},
	}
	host := 80.0
	pres := Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"}
	rows := RowsFromConfig(cfg, LiveFromReadings(devs, &host, &pres))
	for _, r := range rows {
		if r.Name != "display" {
			continue
		}
		if r.Admissible || !strings.Contains(r.Reason, "monitor on index 0") {
			t.Fatalf("the display row must be inadmissible and say why, got admissible=%v %q", r.Admissible, r.Reason)
		}
		return
	}
	t.Fatal("no display row")
}

// The Snapshot every local caller reads through serves the screen cards from the same memoised probe
// as the free-VRAM numbers: one nvidia-smi per ttl, not one per question.
func TestSnapshotServesTheScreenCardsFromItsMemoisedProbe(t *testing.T) {
	cfg := config.CompositeFixture()
	now := time.Unix(1_700_000_000, 0)
	reads := 0
	s := NewSnapshot(cfg, 2*time.Second)
	s.now = func() time.Time { return now }
	s.readGPU = func(context.Context) ([]gpuprobe.Device, error) {
		reads++
		return []gpuprobe.Device{{Index: 0, UUID: "GPU-aaaa1111", FreeGiB: 2}, {Index: 1, UUID: "GPU-bbbb2222", FreeGiB: 15, DisplayAttached: true}}, nil
	}
	live := s.Live()
	if live.ScreenCards == nil {
		t.Fatal("the snapshot's Live must carry the screen-card reader")
	}
	for i := 0; i < 5; i++ {
		if got := live.ScreenCards(); len(got) != 1 || got[0] != "1" {
			t.Fatalf("ScreenCards = %v, want [1]", got)
		}
		if _, ok := live.DeviceFree("1"); !ok {
			t.Fatal("free VRAM must be read from the same probe")
		}
	}
	if reads != 1 {
		t.Fatalf("one probe per ttl serves both questions, got %d reads", reads)
	}
	s.readGPU = func(context.Context) ([]gpuprobe.Device, error) { return nil, errors.New("nvidia-smi: driver reset") }
	now = now.Add(time.Minute)
	if got := live.ScreenCards(); got != nil {
		t.Fatalf("a failed probe says nothing about the screen, got %v", got)
	}
}
