package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// display_device names the card the display_floor guard protects. "auto" names none: the guard asks the
// driver which card drives the monitor (internal/placement). These tests pin the config side: what the
// sentinel is, that it loads, and that the forms the runtime already accepts keep loading.

func TestIsAutoDisplayDevice(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"auto", true},
		{"AUTO", true},
		{"Auto", true},
		{" auto ", true},
		{"\tauto\n", true},
		{"", false},
		{"atuo", false},
		{"autos", false},
		{"auto0", false},
		{"au to", false},
		{"0", false},
		{"1", false},
		{"GPU-8888bbbb", false},
		{"GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000", false},
		{"GPU-auto", false},
	} {
		if got := IsAutoDisplayDevice(tc.in); got != tc.want {
			t.Errorf("IsAutoDisplayDevice(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if DisplayDeviceAuto != "auto" {
		t.Errorf("the sentinel is the string the seed and the docs spell, got %q", DisplayDeviceAuto)
	}
}

// ValidateLayers does not shape-check display_device, on purpose: a load error strips EVERY layer from the
// config Load hands back (stripComposite), and the runtime accepts any prefix of a GPU UUID (gpuprobe.IndexOf
// matches the lower-cased "gpu-..." id), dashes included, so a stricter rule here would turn a working
// UUID-pinned box into one with no composite tier at all. A typo such as "atuo" is therefore not a load
// error; it fails closed at admission (internal/placement), where the display layer refuses by name.
func TestDisplayDeviceIsNotShapeCheckedAtLoad(t *testing.T) {
	for _, dev := range []string{
		"1",
		"0",
		"auto",
		"AUTO",
		" auto ",
		"GPU-8888bbbb",
		"GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000",
		"GPU-abc",
		"gpu-8888bbbb",
		"atuo", // not the sentinel and not a card: refused where it is used, never at load
	} {
		c := CompositeFixture()
		c.Layers[3].DisplayDevice = dev
		if err := c.ValidateLayers(); err != nil {
			t.Errorf("display_device %q must validate (the runtime resolves it or fails closed), got %v", dev, err)
		}
	}
}

// "auto" is a device for the pairing rules: a floor and the guard still need a display_device, and the
// sentinel fills that slot.
func TestAutoLayerStillNeedsFloorAndGuard(t *testing.T) {
	c := CompositeFixture()
	c.Layers[3].DisplayDevice = "auto"
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("an auto display layer with its floor and guard is valid: %v", err)
	}
	c.Layers[3].DisplayFloorGiB = 0
	if err := c.ValidateLayers(); err == nil || !strings.Contains(err.Error(), "guard display_floor needs display_device and display_floor_gib") {
		t.Fatalf("auto with no floor behind the display_floor guard must be refused, got %v", err)
	}
	c = CompositeFixture()
	c.Layers[3].DisplayDevice = ""
	if err := c.ValidateLayers(); err == nil || !strings.Contains(err.Error(), "display_floor_gib needs display_device") {
		t.Fatalf("no display_device (auto is spelled, not implied) must still be refused, got %v", err)
	}
}

// With "auto" config cannot say which card the seat's pin must avoid (the driver says, at admission), so
// the load-time footprint rule that applies to a plain CUDA index does not run; admission enforces the same
// rule on the card it derives (internal/placement TestAutoDisplayStillRefusesAnUndeclaredFootprintOnTheDerivedCard).
func TestAutoLeavesTheUndeclaredFootprintToAdmission(t *testing.T) {
	c := CompositeFixture()
	c.Layers[3].DisplayDevice = "auto"
	c.Layers[3].Seats[0].DisplayFootprintGiB = 0
	if err := c.ValidateLayers(); err != nil {
		t.Fatalf("config cannot resolve auto to a card, so it cannot judge the footprint at load: %v", err)
	}
	c.Layers[3].DisplayDevice = "1"
	if err := c.ValidateLayers(); err == nil || !strings.Contains(err.Error(), "display_footprint_gib is undeclared") {
		t.Fatalf("a plain index still owes the footprint at load, got %v", err)
	}
}

// A dashed full UUID (the form a box that pins by UUID carries) and the sentinel both survive the door
// the binary uses, with every layer intact.
func TestUUIDAndAutoDisplayDevicesLoadWithTheirLayersIntact(t *testing.T) {
	for _, dev := range []string{"GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000", "auto"} {
		c := CompositeFixture()
		c.Layers[3].DisplayDevice = dev
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Load(p)
		if err != nil {
			t.Fatalf("display_device %q must load: %v", dev, err)
		}
		if !got.Composite() || len(got.Layers) != 4 {
			t.Fatalf("display_device %q: the composite keys must survive Load, got %d layers", dev, len(got.Layers))
		}
		l, ok := got.Layer("display")
		if !ok || l.DisplayDevice != dev || l.DisplayFloorGiB != 4 {
			t.Fatalf("display_device %q must round-trip through Load, got %+v", dev, l)
		}
	}
}

// The floor the card allocator holds an opened display card to is read from display_floor_gib and the
// guard, never from the device string, so it cannot depend on which card (or "auto") the layer names.
func TestDisplayFloorGiBDoesNotDependOnTheDeviceString(t *testing.T) {
	for _, dev := range []string{"1", "0", "auto", "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000"} {
		c := CompositeFixture()
		for i := range c.Layers {
			if c.Layers[i].DisplayDevice != "" {
				c.Layers[i].DisplayDevice = dev
			}
		}
		if got := c.DisplayFloorGiB(); got != 4 {
			t.Errorf("display_device %q: DisplayFloorGiB = %v, want 4", dev, got)
		}
	}
}
