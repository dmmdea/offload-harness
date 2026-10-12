package config

import (
	"strings"
	"testing"
)

// gpu_display_cap_pct (A-RAM-15): the share of the display card's VRAM jobs may use. Unset is 60, the operator's
// rule of 2026-10-11, and 0 is the "I need the card" switch that closes it, so the two must be different values.
func TestGPUDisplayCapPctDefaultsTo60AndRejectsOutOfRange(t *testing.T) {
	if got := Default().GPUDisplayCap(); got != 60 {
		t.Fatalf("the built-in default is 60 percent, got %d", got)
	}
	if got := (Config{}).GPUDisplayCap(); got != 60 {
		t.Fatalf("a Config that never read a file keeps the same default, got %d", got)
	}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"unset", `{"model":"x"}`, 60},
		{"null is unset", `{"gpu_display_cap_pct": null}`, 60},
		{"zero closes the card, it is not the default", `{"gpu_display_cap_pct": 0}`, 0},
		{"a share", `{"gpu_display_cap_pct": 35}`, 35},
		{"the whole card", `{"gpu_display_cap_pct": 100}`, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeCfg(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.GPUDisplayCap(); got != tc.want {
				t.Fatalf("GPUDisplayCap() = %d, want %d", got, tc.want)
			}
		})
	}
	for _, body := range []string{`{"gpu_display_cap_pct": -1}`, `{"gpu_display_cap_pct": 101}`, `{"gpu_display_cap_pct": 1000}`} {
		_, err := Load(writeCfg(t, body))
		if err == nil || !strings.Contains(err.Error(), "gpu_display_cap_pct") {
			t.Errorf("%s: want a load error that names gpu_display_cap_pct, got %v", body, err)
		}
	}
}
