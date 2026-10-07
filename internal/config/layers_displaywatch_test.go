package config

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// display_watch_sec follows operator_idle_sec's convention (0 = the default) and adds the explicit
// switch a periodic check that can unload a seat needs: negative turns it off.
func TestDisplayWatchIntervalDefaultsAndSwitchesOff(t *testing.T) {
	for _, tc := range []struct {
		sec  int
		want time.Duration
	}{
		{0, 10 * time.Second},
		{1, time.Second},
		{30, 30 * time.Second},
		{300, 300 * time.Second},
		{-1, 0},
		{-3600, 0},
	} {
		c := Config{DisplayWatchSec: tc.sec}
		if got := c.DisplayWatchInterval(); got != tc.want {
			t.Errorf("display_watch_sec %d: interval %v, want %v", tc.sec, got, tc.want)
		}
	}
}

func TestDisplayWatchSecIsReadFromItsJSONKey(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"display_watch_sec": 7}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.DisplayWatchSec != 7 || c.DisplayWatchInterval() != 7*time.Second {
		t.Fatalf("display_watch_sec did not decode: %+v", c)
	}
}

// A check slower than llama-swap's own 300 s idle ttl guards nothing the ttl does not, so the config
// that names one is refused by name at load, on every box (the key is not composite-only).
func TestValidateLayersRefusesADisplayWatchSlowerThanTheIdleTTL(t *testing.T) {
	c := Config{DisplayWatchSec: 301}
	err := c.ValidateLayers()
	if err == nil || !strings.Contains(err.Error(), "display_watch_sec") {
		t.Fatalf("301 s must be refused by name, got %v", err)
	}
	for _, sec := range []int{-1, 0, 1, 10, 300} {
		if err := (Config{DisplayWatchSec: sec}).ValidateLayers(); err != nil {
			t.Errorf("display_watch_sec %d must validate, got %v", sec, err)
		}
	}
}

// display_watch_sec is seconds, and seconds * time.Second overflows int64 for a value above about 9.2e9
// seconds: the product wraps NEGATIVE. Compared after the multiplication that read as "off" and passed
// validation, so an absurd value switched the post-admission check off with no complaint. It is compared
// as an integer first now, so it is refused by name, and the interval clamps instead of wrapping.
func TestValidateLayersRefusesADisplayWatchThatOverflowsADuration(t *testing.T) {
	overflowing := []int{math.MaxInt}
	// The smallest value whose product with time.Second wraps (not representable as an int on a 32-bit
	// build, where nothing can overflow a Duration through an int anyway).
	justOver := int64(math.MaxInt64)/int64(time.Second) + 1
	if int64(int(justOver)) == justOver {
		overflowing = append(overflowing, int(justOver), int(justOver)*2)
		if wrapped := time.Duration(int(justOver)) * time.Second; wrapped >= 0 {
			t.Fatalf("setup: %d s was expected to wrap negative as a Duration, got %v", justOver, wrapped)
		}
	}
	for _, sec := range overflowing {
		err := (Config{DisplayWatchSec: sec}).ValidateLayers()
		if err == nil || !strings.Contains(err.Error(), "display_watch_sec") {
			t.Errorf("display_watch_sec %d overflows a Duration and must be refused by name, got %v", sec, err)
		}
		if got := (Config{DisplayWatchSec: sec}).DisplayWatchInterval(); got != 300*time.Second {
			t.Errorf("display_watch_sec %d unvalidated must clamp to 300 s, not wrap to %v (negative reads as off)", sec, got)
		}
	}
	// The boundary stays where it was, and negative stays the explicit switch.
	for _, sec := range []int{math.MinInt, -1, 0, 1, 300} {
		if err := (Config{DisplayWatchSec: sec}).ValidateLayers(); err != nil {
			t.Errorf("display_watch_sec %d must validate, got %v", sec, err)
		}
	}
	if err := (Config{DisplayWatchSec: 301}).ValidateLayers(); err == nil {
		t.Error("301 stays refused")
	}
}
