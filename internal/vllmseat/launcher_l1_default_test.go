package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherDefaultsL1StagingToTwoGBForUnmeasuredSeats (register B-02).
// 2 GB is the default for a seat with no measured value, in the launcher text and
// the spec default alike, so a fresh render and an unset env agree. Measured on
// the pair seat on 2026-09-18 against one NVMe store with 24 real contracts, the
// 8 / 4 / 2 GB arms restored the same context at 9.1 / 5.1 / 3.1 GiB of MP-server
// RSS. It is not enough for every seat: on 2026-09-21 a 2 GB cut starved the
// pair's staging under production load (the stores came up short by 34 blocks and
// nothing reached L2), so the pair profile seeds its measured 8 and the
// three-card flagship 16 (dualblackwell_seat_test.go pins both).
func TestSeatLauncherDefaultsL1StagingToTwoGBForUnmeasuredSeats(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `L1_GB="${SEAT_L1_GB:-2}"`) {
		t.Fatalf("seat_fg.sh no longer defaults SEAT_L1_GB to 2 GB, the default for a seat with no measured value")
	}
	if got := (CacheServer{}).EffectiveL1StagingGB(); got != 2 {
		t.Fatalf("CacheServer default L1 = %d, want 2", got)
	}
	if got := (CacheServer{L1StagingGB: 32}).EffectiveL1StagingGB(); got != 32 {
		t.Fatalf("an explicit L1 must win, got %d", got)
	}
}
