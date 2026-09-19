package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherDefaultsL1StagingToTheMeasuredTwoGB (register B-02, 2026-09-18).
// The L1 staging size was an unmeasured 8 GB seed since 0.113. Measured on the
// pair seat against one NVMe store with 24 real contracts: 8 / 4 / 2 GB arms
// restore the same context (every graded contract passes at every size, 0
// preemptions, tier-served tokens within the metric's noise) at 9.1 / 5.1 / 3.1
// GiB of MP-server RSS. The smallest at parity is the default, in the launcher
// text, the spec default and the pair profile alike, so a fresh render and an
// unset env agree.
func TestSeatLauncherDefaultsL1StagingToTheMeasuredTwoGB(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `L1_GB="${SEAT_L1_GB:-2}"`) {
		t.Fatalf("seat_fg.sh no longer defaults SEAT_L1_GB to the measured 2 GB")
	}
	if got := (CacheServer{}).EffectiveL1StagingGB(); got != 2 {
		t.Fatalf("CacheServer default L1 = %d, want 2", got)
	}
	if got := (CacheServer{L1StagingGB: 32}).EffectiveL1StagingGB(); got != 32 {
		t.Fatalf("an explicit L1 must win, got %d", got)
	}
}
