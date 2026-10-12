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

// A value Load refuses still reaches the callers that proceed on it: Load returns the Config beside the error, and
// the one-shot verbs (gpu reserve among them) discard the error. The validator exists so that a typo of 600 for 60
// does not hand the desktop's memory to a job, so the value that was refused must read as the card closed, never as
// the whole card.
func TestARefusedGPUDisplayCapReadsAsClosedForACallerThatProceedsOnTheValue(t *testing.T) {
	for _, body := range []string{`{"gpu_display_cap_pct": 600}`, `{"gpu_display_cap_pct": 101}`, `{"gpu_display_cap_pct": -1}`} {
		c, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("%s: setup: Load must refuse the value", body)
		}
		if got := c.GPUDisplayCap(); got != 0 {
			t.Errorf("%s: the Config Load returned beside its error reads a cap of %d, want 0 (closed): a caller that discards the error would hand the display card to a job", body, got)
		}
	}
	// By hand, on a Config that never went through Load, the same value is just as far from a share.
	over := 150
	if got := (Config{GPUDisplayCapPct: &over}).GPUDisplayCap(); got != 0 {
		t.Errorf("a hand-set 150 reads %d, want 0 (closed)", got)
	}
}
