package displaywatch

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The watcher puts the display layer's guards to a loaded twin on every check. With display_device
// "auto" the card it reads is the one the driver reports for the monitor, so the unload follows the
// monitor too, and the card the seed used to name is not what decides.

func autoWatchedCfg() config.Config {
	cfg := watchedCfg()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].DisplayDevice = "auto"
			cfg.Layers[i].Seats[0].Device = "0"
		}
	}
	return cfg
}

// watcherWithMonitor builds a watcher whose device sample puts the monitor on the given card (-1 =
// no card flagged) with the given free VRAM on cards 0 and 1.
func watcherWithMonitor(t *testing.T, b *box, monitor int, free0, free1 float64) *Watcher {
	t.Helper()
	d := b.deps("")
	d.Devices = func() ([]gpuprobe.Device, bool) {
		devs := []gpuprobe.Device{
			{Index: 0, UUID: "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", FreeGiB: free0},
			{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", FreeGiB: free1},
			{Index: 2, UUID: "GPU-cccc3333-dddd-eeee-ffff-000000000000", FreeGiB: 1},
		}
		for i := range devs {
			devs[i].DisplayAttached = i == monitor
		}
		return devs, true
	}
	w := New(autoWatchedCfg(), d)
	if w == nil {
		t.Fatal("an awake display layer with desktop guards wants a watcher")
	}
	return w
}

func TestTheWatcherUnloadsATwinWhenTheMonitorsCardFallsUnderTheFloor(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B)
	out := watcherWithMonitor(t, b, 0, 3.5, 15).Check(context.Background())
	if len(b.unloaded) != 1 || b.unloaded[0] != twinE4B {
		t.Fatalf("3.5 GiB free on the monitor's card (0) under a 4 GiB floor must unload the twin, got %v", b.unloaded)
	}
	joined := strings.Join(out.Reasons, " ")
	if !strings.Contains(joined, "floor") || !strings.Contains(joined, "(index 0)") {
		t.Fatalf("the reason names the floor and the derived card: %q", joined)
	}
}

func TestTheWatcherKeepsATwinWhoseMonitorCardHoldsTheFloor(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B)
	// Card 1 is almost full, but the monitor is on 0: nothing on card 1 is the desktop's to protect.
	out := watcherWithMonitor(t, b, 0, 6, 0.5).Check(context.Background())
	if len(b.unloaded) != 0 || len(out.Unloaded) != 0 {
		t.Fatalf("the floor holds on the monitor's card: nothing may be unloaded, got %v", b.unloaded)
	}
	if len(out.Loaded) != 1 {
		t.Fatalf("the loaded twin is reported as watched: %+v", out)
	}
}

// The monitor is not on the card the seed names: the watch follows it there.
func TestTheWatcherFollowsTheMonitorToAnotherCard(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B)
	out := watcherWithMonitor(t, b, 1, 15, 2).Check(context.Background())
	if len(b.unloaded) != 1 || !strings.Contains(strings.Join(out.Reasons, " "), "(index 1)") {
		t.Fatalf("the monitor on 1 with 2.0 GiB free there must unload, naming index 1: unloaded=%v reasons=%v", b.unloaded, out.Reasons)
	}
}

// Fail closed: with no single card to follow the floor cannot be shown to hold, so the twin leaves.
func TestTheWatcherUnloadsATwinWhenTheDriverNamesNoSingleCard(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B)
	out := watcherWithMonitor(t, b, -1, 15, 15).Check(context.Background()) // plenty free everywhere, no card flagged
	if len(b.unloaded) != 1 || !strings.Contains(strings.Join(out.Reasons, " "), "display_device auto") {
		t.Fatalf("an auto layer with no monitor card to read must unload and say so: unloaded=%v reasons=%v", b.unloaded, out.Reasons)
	}
}
