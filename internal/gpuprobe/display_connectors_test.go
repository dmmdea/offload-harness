package gpuprobe

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The reading that failed for real on node-i (headless Linux, two A2, driver 615.71): both datacenter
// cards answer display_attached Yes with no connector at all. The sysfs tree below is the measured one:
// card0 and card1 (nvidia) have no cardN-<connector> entries, the iGPU's card2 has connectors.
func TestPhantomAttachedIgnoredWhenCardHasNoConnectors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a PCI bus id (0000:01:00.0) is not a legal directory name on Windows; the rule is Linux only")
	}
	root := t.TempDir()
	mk := func(bus, vendor string, dirs ...string) {
		dev := filepath.Join(root, "bus", "pci", "devices", bus)
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dev, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(dev, "drm", d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("0000:01:00.0", "0x10de", "card0")
	mk("0000:03:00.0", "0x10de", "card1")
	mk("0000:00:02.0", "0x8086", "card2", "card2/card2-DP-1", "card2/card2-HDMI-A-1")

	oldRoot, oldBus := sysfsRoot, smiBusIDs
	t.Cleanup(func() { sysfsRoot, smiBusIDs = oldRoot, oldBus; resetPhantomState() })
	resetPhantomState()
	sysfsRoot = root
	smiBusIDs = func() (string, error) {
		return "GPU-1111aaaa-0000-0000-0000-000000000001, 00000000:01:00.0\nGPU-2222bbbb-0000-0000-0000-000000000002, 00000000:03:00.0\n", nil
	}

	out := "0, GPU-1111aaaa-0000-0000-0000-000000000001, NVIDIA A2, 15356, 1115, 0, Disabled, Yes\n" +
		"1, GPU-2222bbbb-0000-0000-0000-000000000002, NVIDIA A2, 15356, 339, 0, Disabled, Yes\n"
	devs, err := ParseSmiMemoryDevices(maskPhantomAttached(out))
	if err != nil || len(devs) != 2 {
		t.Fatalf("parse: %v (%d devices)", err, len(devs))
	}
	for _, d := range devs {
		if d.DisplayAttached || d.DrivesDisplay() {
			t.Errorf("card %d: a card with no connectors must not drive a display, got attached=%v", d.Index, d.DisplayAttached)
		}
	}
	if got := ScreenCardUUIDs(devs); len(got) != 0 {
		t.Errorf("ScreenCardUUIDs = %v, want none", got)
	}

	// display_active stays authoritative, and a card WITH connectors keeps what the driver said.
	kept := strings.Replace(out, "0, Disabled, Yes\n1,", "0, Enabled, Yes\n1,", 1)
	devs, _ = ParseSmiMemoryDevices(maskPhantomAttached(kept))
	if !devs[0].DisplayActive || !devs[0].DrivesDisplay() {
		t.Errorf("display_active Enabled must stay authoritative, got %+v", devs[0])
	}
	smiBusIDs = func() (string, error) { return "GPU-1111aaaa-0000-0000-0000-000000000001, 00000000:00:02.0\n", nil }
	resetPhantomState()
	devs, _ = ParseSmiMemoryDevices(maskPhantomAttached(out))
	if !devs[0].DisplayAttached {
		t.Errorf("a card on a device with connectors must keep display_attached Yes")
	}
}
