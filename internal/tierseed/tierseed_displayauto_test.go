package tierseed

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The display layer's guard protects the card that drives the monitor. A table that names that card by a
// literal index ships a layer that is only right until the monitor moves, and then refuses forever (the
// guard cross-checks a declaration against the driver, never follows it). The shipped table says "auto" so
// the guard reads the driver's own answer; a tier that names a card again is a decision to make on purpose.
func TestShippedDisplayFloorLayersFollowTheMonitor(t *testing.T) {
	profiles, err := Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["blackwell-3x16"]
	if !ok {
		t.Fatal("blackwell-3x16 is not in the table: this gate went blind")
	}
	guarded := 0
	for _, l := range p.Layers {
		for _, g := range l.Guards {
			if g != "display_floor" {
				continue
			}
			guarded++
			if !config.IsAutoDisplayDevice(l.DisplayDevice) {
				t.Errorf("layer %q is guarded by display_floor and names display_device %q: the shipped table must say %q so the guard follows the card the driver reports for the monitor",
					l.Name, l.DisplayDevice, config.DisplayDeviceAuto)
			}
		}
	}
	if guarded == 0 {
		t.Fatal("blackwell-3x16 declares no display_floor-guarded layer: this gate went blind")
	}
}

// The seeded table must keep validating as a whole: the sentinel is a device for every pairing rule.
func TestSeededAutoDisplayLayerValidates(t *testing.T) {
	profiles, err := Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := Resolve(profiles["blackwell-3x16"], "blackwell-3x16", Options{GOOS: "windows", Home: "C:/x", RAMTier: "high", VLLMSeatActive: true})
	if err != nil {
		t.Fatal(err)
	}
	layers, ok := seed["layers"].([]config.LayerSpec)
	if !ok {
		t.Fatalf("layers = %#v", seed["layers"])
	}
	c := config.Config{TierProfile: "blackwell-3x16", Tiers: seed["tiers"].([]string), Layers: layers}
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("the seeded layers must validate: %v", err)
	}
	var display *config.LayerSpec
	for i := range layers {
		if layers[i].Name == "display" {
			display = &layers[i]
		}
	}
	if display == nil || !display.Dormant || display.DisplayFloorGiB <= 0 {
		t.Fatalf("the display layer ships dormant with its floor: %+v", display)
	}
}
