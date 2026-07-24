//go:build windows

package fleetnode

import (
	"os"
	"testing"
)

// TestLiveWindowsProbes exercises the REAL WDDM sources on this machine —
// registry capacity, PDH adapter usage, RAM, both GenericWindowsProbe
// compositions, and the pdh-shared per-process tree on our own pid. Gated
// behind FLEET_LIVE_SMOKE=1 because the values are machine-dependent (CI-safe
// by default); the assertions are sanity bounds, not exact numbers. This is
// also the receipt probe for a new box: run
//
//	FLEET_LIVE_SMOKE=1 go test -run TestLiveWindowsProbes -v ./internal/fleetnode/
//
// and send the logged numbers back with the install receipts.
func TestLiveWindowsProbes(t *testing.T) {
	if os.Getenv("FLEET_LIVE_SMOKE") != "1" {
		t.Skip("set FLEET_LIVE_SMOKE=1 to exercise the live WDDM probes")
	}
	ded, err := DedicatedVramTotalGiB()
	if err != nil {
		t.Fatalf("DedicatedVramTotalGiB: %v", err)
	}
	t.Logf("DedicatedVramTotalGiB = %.2f", ded)
	if ded <= 0 || ded > 512 {
		t.Fatalf("implausible dedicated total %.2f GiB", ded)
	}
	ad, as, err := AdapterUsageGiB()
	if err != nil {
		t.Fatalf("AdapterUsageGiB: %v", err)
	}
	t.Logf("AdapterUsageGiB dedicated=%.2f shared=%.2f", ad, as)
	if ad < 0 || as < 0 {
		t.Fatalf("negative adapter usage (%.2f, %.2f)", ad, as)
	}
	ram, err := SystemRAMGiB()
	if err != nil {
		t.Fatalf("SystemRAMGiB: %v", err)
	}
	t.Logf("SystemRAMGiB = %.1f", ram)
	if ram < 1 || ram > 4096 {
		t.Fatalf("implausible RAM %.1f GiB", ram)
	}
	for _, uma := range []bool{false, true} {
		total, used, err := GenericWindowsProbe(uma)()
		if err != nil {
			t.Fatalf("GenericWindowsProbe(uma=%v): %v", uma, err)
		}
		t.Logf("GenericWindowsProbe(uma=%v) total=%.2f used=%.2f", uma, total, used)
		if total <= 0 {
			t.Fatalf("uma=%v: total must be > 0 (contract)", uma)
		}
		if uma && total <= ded {
			t.Fatalf("uma total %.2f must exceed the bare carve-out %.2f (adds the shared budget)", total, ded)
		}
	}
	self, err := TreeDedicatedPlusSharedGiB(os.Getpid())
	if err != nil {
		t.Fatalf("TreeDedicatedPlusSharedGiB(self): %v", err)
	}
	t.Logf("TreeDedicatedPlusSharedGiB(self) = %.3f", self)
	if self < 0 {
		t.Fatalf("negative per-process usage %.3f", self)
	}
}
