package config

import (
	"os"
	"path/filepath"
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

// A running process never reloads its config, and the operator's "I need the card" switch (gpu_display_cap_pct 0)
// has to reach work that is already queued for hours and a long-lived server. LiveGPUDisplayCap follows the file
// the Config was read from; the snapshot GPUDisplayCap does not.
func TestTheDisplayCapSwitchFollowsTheFileTheConfigWasReadFrom(t *testing.T) {
	path := writeCfg(t, `{"gpu_display_cap_pct": 60}`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.LiveGPUDisplayCap(); got != 60 {
		t.Fatalf("nothing has changed yet: the live cap is the file's 60, got %d", got)
	}
	rewrite(`{"gpu_display_cap_pct": 0}`) // the operator says he needs the card
	if got := c.LiveGPUDisplayCap(); got != 0 {
		t.Fatalf("the file now says 0 and the process holding the Config must see it, got %d", got)
	}
	if got := c.GPUDisplayCap(); got != 60 {
		t.Fatalf("the snapshot is the value at load, got %d", got)
	}
	rewrite(`{"gpu_display_cap_pct": 35}`)
	if got := c.LiveGPUDisplayCap(); got != 35 {
		t.Fatalf("a new share is followed too, got %d", got)
	}
	rewrite(`{"gpu_display_cap_pct": 35, "model": `) // an editor part-way through a save
	if got := c.LiveGPUDisplayCap(); got != 35 {
		t.Fatalf("a file that cannot be read keeps the last value read from it (35), not the load-time 60: got %d", got)
	}
	rewrite(`{"gpu_display_cap_pct": 600}`) // the typo Load refuses by name
	if got := c.LiveGPUDisplayCap(); got != 0 {
		t.Fatalf("a value that is not a share reads as closed, got %d", got)
	}
	rewrite(`{"model": "x"}`) // the key removed: unset is the default, not the old value
	if got := c.LiveGPUDisplayCap(); got != 60 {
		t.Fatalf("the key removed reads as the default 60, got %d", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rewrite(`{"gpu_display_cap_pct": 20}`)
	if got := c.LiveGPUDisplayCap(); got != 20 {
		t.Fatalf("a file replaced under the same name is followed, got %d", got)
	}
}

// A Config that was not read from a file has nothing to follow: it answers with what it holds, and a file the
// Config named but that is gone before any read falls back to the load-time value rather than to the default.
func TestTheDisplayCapOfAConfigWithNoFileIsItsOwnValue(t *testing.T) {
	zero := 0
	if got := (Config{GPUDisplayCapPct: &zero}).LiveGPUDisplayCap(); got != 0 {
		t.Errorf("a hand-built Config with the switch at 0 reads 0, got %d", got)
	}
	if got := (Config{}).LiveGPUDisplayCap(); got != 60 {
		t.Errorf("a hand-built Config with nothing set reads the default, got %d", got)
	}
	thirty := 30
	gone := Config{GPUDisplayCapPct: &thirty}.WithSource(filepath.Join(t.TempDir(), "never-written.json"))
	if got := gone.LiveGPUDisplayCap(); got != 30 {
		t.Errorf("a source that cannot be read at all falls back to the value the Config holds (30), got %d", got)
	}
	if got := Default().LiveGPUDisplayCap(); got != 60 {
		t.Errorf("the built-in defaults read 60, got %d", got)
	}
}
