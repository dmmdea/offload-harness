package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F82, the hold bound (the incident of 2026-10-14): a lease that unloaded the seat and then waits for the host to have its
// RAM holds the cards for the whole wait. gpu_hostram_hold_max_sec bounds that hold. Unset is a chosen 600 seconds and 0 is a
// value ("no bound, the whole --wait, as before"), so the key is a pointer and the accessor tells the three apart.

func intp(v int) *int { return &v }

func TestGPUHostRAMHoldMaxDefaultsToTenMinutesAndZeroIsAValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  *int
		want time.Duration
	}{
		{"unset is the chosen default", nil, 600 * time.Second},
		{"a configured bound", intp(90), 90 * time.Second},
		{"zero means no bound, not the default", intp(0), 0},
		{"a negative value reads as the default, never as no bound", intp(-5), 600 * time.Second},
	} {
		c := Default()
		c.GPUHostRAMHoldMaxSec = tc.set
		if got := c.GPUHostRAMHoldMax(); got != tc.want {
			t.Errorf("%s: GPUHostRAMHoldMax() = %s, want %s", tc.name, got, tc.want)
		}
	}
	if d := Default(); d.GPUHostRAMHoldMaxSec != nil {
		t.Errorf("the built-in config leaves the key unset so the default applies, got %v", *d.GPUHostRAMHoldMaxSec)
	}
}

// The key survives a round trip through the file: an explicit 0 stays 0 (and is not mistaken for unset), an absent key stays
// nil, and a negative value is refused by name, the way gpu_display_cap_pct refuses a value that is not a share.
func TestLoadReadsTheHoldBoundAndRefusesANegativeOne(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	write := func(extra string) {
		t.Helper()
		body := `{"state_dir": "` + filepath.ToSlash(dir) + `"` + extra + `}`
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`, "gpu_hostram_hold_max_sec": 0`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.GPUHostRAMHoldMaxSec == nil || c.GPUHostRAMHoldMax() != 0 {
		t.Errorf("an explicit 0 is the unbounded wait: key %v, bound %s", c.GPUHostRAMHoldMaxSec, c.GPUHostRAMHoldMax())
	}
	write(`, "gpu_hostram_hold_max_sec": 45`)
	if c, err = Load(p); err != nil || c.GPUHostRAMHoldMax() != 45*time.Second {
		t.Errorf("a configured bound loads: %v %s", err, c.GPUHostRAMHoldMax())
	}
	write("")
	if c, err = Load(p); err != nil || c.GPUHostRAMHoldMaxSec != nil || c.GPUHostRAMHoldMax() != 600*time.Second {
		t.Errorf("an absent key is the default: %v key=%v bound=%s", err, c.GPUHostRAMHoldMaxSec, c.GPUHostRAMHoldMax())
	}
	write(`, "gpu_hostram_hold_max_sec": -1`)
	c, err = Load(p)
	if err == nil || !strings.Contains(err.Error(), "gpu_hostram_hold_max_sec") {
		t.Errorf("a negative bound is refused by name, got %v", err)
	}
	if c.GPUHostRAMHoldMax() != 600*time.Second {
		t.Errorf("a one-shot verb that goes on with the Config anyway must read a negative bound as the default, got %s", c.GPUHostRAMHoldMax())
	}
}
