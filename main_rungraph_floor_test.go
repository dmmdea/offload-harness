package main

import (
	"strings"
	"testing"
)

// run-graph takes --min-vram-gb (the least size, in GB, of the largest card of the fleet node that takes the graph) and
// --model-family (which the default floor keys on). Both reach the request as the delegator reads them; absent, neither
// key is there.
func TestRunGraphParamsCarryTheFleetFlags(t *testing.T) {
	p, err := runGraphParams([]string{"--graph-json", "{}", "--min-vram-gb", "16", "--model-family", " wan22 "})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := p["min_vram_gb"].(float64); !ok || v != 16 {
		t.Errorf("min_vram_gb = %#v, want 16", p["min_vram_gb"])
	}
	if p["model_family"] != "wan22" {
		t.Errorf("model_family = %#v", p["model_family"])
	}
	// 0 is a number the caller gave: it lifts the default floor.
	if p, err = runGraphParams([]string{"--graph-json", "{}", "--min-vram-gb", "0"}); err != nil {
		t.Fatal(err)
	} else if v, ok := p["min_vram_gb"]; !ok || v != float64(0) {
		t.Errorf("an explicit 0 must reach the request: %#v", p)
	}
	if p, err = runGraphParams([]string{"--graph-json", "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"min_vram_gb", "model_family"} {
		if _, has := p[k]; has {
			t.Errorf("%s must be absent when the flag was not given: %v", k, p)
		}
	}
}

func TestRunGraphMinVRAMMustBeANonNegativeNumber(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "NaN", "Inf"} {
		_, err := runGraphParams([]string{"--graph-json", "{}", "--min-vram-gb", bad})
		if err == nil || !strings.Contains(err.Error(), "--min-vram-gb") {
			t.Errorf("--min-vram-gb %q must be refused by name, got %v", bad, err)
		}
	}
}
