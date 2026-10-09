package tierseed

import "testing"

// TestRAMLowUpIsExactlyLowMidHigh pins the predicate the overlay AND the serving render share
// (install_render.go spillSeatIncluded calls it): the 28 GB-and-up class. min, an unset tier and an
// unknown name are all false, so an unmeasured box neither takes the overlay nor renders the seat.
// Case and surrounding whitespace do not change the answer: `install render` lowercases and trims its
// --ram-tier before asking, so the seed side must read the same flag the same way ("LOW", " Mid ").
func TestRAMLowUpIsExactlyLowMidHighWhateverTheCaseOrPadding(t *testing.T) {
	for _, tier := range []string{"low", "mid", "high", "LOW", " low", "Mid ", "\tHIGH\n"} {
		if !RAMLowUp(tier) {
			t.Errorf("RAMLowUp(%q) = false, want true", tier)
		}
	}
	for _, tier := range []string{"", " ", "min", "MIN", "none", "huge", "medium", "lo w"} {
		if RAMLowUp(tier) {
			t.Errorf("RAMLowUp(%q) = true, want false", tier)
		}
	}
}

// TestResolveAppliesTheLowUpOverlayOnLowMidHighAndNeverOnMinOrUnknown: the base binding is what a
// `min` (or unknown) box keeps; low gets the low-and-up value; mid and high get the mid/high value
// where both layers set the key, and keep a low-and-up key the mid/high layer does not set. The tier
// name is read case-blind and trimmed for BOTH overlays, as the serving render reads it.
func TestResolveAppliesTheLowUpOverlayOnLowMidHighAndNeverOnMinOrUnknown(t *testing.T) {
	p := Profile{
		ConfigSeed:        map[string]any{"agent_model": "base-seat", "fleet_max_concurrent_jobs": 1},
		ConfigSeedLowUp:   map[string]any{"agent_model": "lowup-seat", "escalation_model": "lowup-only"},
		ConfigSeedMidHigh: map[string]any{"agent_model": "midhigh-seat"},
	}
	for _, tc := range []struct{ ram, agent, escalation string }{
		{"", "base-seat", ""},
		{"min", "base-seat", ""},
		{"huge", "base-seat", ""},
		{"MIN", "base-seat", ""},
		{"low", "lowup-seat", "lowup-only"},
		{"LOW", "lowup-seat", "lowup-only"},
		{" low ", "lowup-seat", "lowup-only"},
		{"mid", "midhigh-seat", "lowup-only"},
		{"Mid", "midhigh-seat", "lowup-only"},
		{"high", "midhigh-seat", "lowup-only"},
		{" HIGH", "midhigh-seat", "lowup-only"},
	} {
		out, err := Resolve(p, "t", Options{Home: "/x", RAMTier: tc.ram})
		if err != nil {
			t.Fatalf("ram %q: %v", tc.ram, err)
		}
		if got := out["agent_model"]; got != tc.agent {
			t.Errorf("ram %q: agent_model = %v, want %q", tc.ram, got, tc.agent)
		}
		if got, _ := out["escalation_model"].(string); got != tc.escalation {
			t.Errorf("ram %q: escalation_model = %q, want %q", tc.ram, got, tc.escalation)
		}
		if out["fleet_max_concurrent_jobs"] != 1 {
			t.Errorf("ram %q: an overlay must not drop a base key, got %v", tc.ram, out["fleet_max_concurrent_jobs"])
		}
	}
}

// TestAmpere6BindsTheSpillSeatOnLowAndUpAndTheSmallSeatOtherwise reads the SHIPPED table: the
// reference 6 GB node (32 GB, ram_tier low) must get the Qwen3.6-35B-A3B spill seat as its agent
// seat, and a box under 28 GB (min) must keep the Qwen3.5-4B. The bug this pins is the one a
// mid/high-only overlay would ship: the measured node classifies as `low`.
func TestAmpere6BindsTheSpillSeatOnLowAndUpAndTheSmallSeatOtherwise(t *testing.T) {
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["ampere-6"]
	if !ok {
		t.Fatal("no ampere-6 tier")
	}
	for _, goos := range []string{"linux", "windows"} {
		for ram, want := range map[string]string{
			"":     "qwen3.5-4b-agent",
			"min":  "qwen3.5-4b-agent",
			"low":  "qwen3.6-35b-a3b-agent",
			"mid":  "qwen3.6-35b-a3b-agent",
			"high": "qwen3.6-35b-a3b-agent",
		} {
			out, err := Resolve(p, "ampere-6", Options{Home: "/opt/offload", GOOS: goos, RAMTier: ram})
			if err != nil {
				t.Fatalf("%s ram %q: %v", goos, ram, err)
			}
			if got := out["agent_model"]; got != want {
				t.Errorf("%s ram %q: agent_model = %v, want %q", goos, ram, got, want)
			}
			// The single-slot rule rides every layer: the seat serves --parallel 1.
			if got := out["fleet_max_concurrent_jobs"]; got != float64(1) && got != 1 {
				t.Errorf("%s ram %q: fleet_max_concurrent_jobs = %v, want 1", goos, ram, got)
			}
		}
	}
}
