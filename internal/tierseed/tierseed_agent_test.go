package tierseed

import "testing"

// The derivation contract (roast-reshaped agent seat, 2026-08-02):
// resident_tier seeds agent_model ONLY when it differs from the effective
// workhorse, and an explicit seed value — including a blank-out — always wins.
func TestAgentModelDerivation(t *testing.T) {
	opt := Options{Home: "/x"}

	t.Run("derives when resident_tier differs from the workhorse", func(t *testing.T) {
		out, err := Resolve(Profile{ResidentTier: "gemma4-26b-a4b"}, "t", opt)
		if err != nil {
			t.Fatal(err)
		}
		if got := out["agent_model"]; got != "gemma4-26b-a4b" {
			t.Fatalf("want derived agent_model=gemma4-26b-a4b, got %v", got)
		}
	})

	t.Run("stays ABSENT when resident_tier equals the workhorse", func(t *testing.T) {
		// Materializing agent_model=workhorse would fork the live fallback chain.
		out, err := Resolve(Profile{ResidentTier: "offload-e4b"}, "t", opt)
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			if _, present := out["agent_model"]; present {
				t.Fatalf("agent_model must not materialize when resident_tier==workhorse, got %v", out["agent_model"])
			}
		}
	})

	t.Run("respects a seeded workhorse in the comparison", func(t *testing.T) {
		out, err := Resolve(Profile{
			ResidentTier: "gemma4-e2b",
			ConfigSeed:   map[string]any{"model": "gemma4-e2b"},
		}, "t", opt)
		if err != nil {
			t.Fatal(err)
		}
		if _, present := out["agent_model"]; present {
			t.Fatal("agent_model must not materialize when resident_tier equals the SEEDED workhorse")
		}
	})

	t.Run("explicit seed wins over derivation", func(t *testing.T) {
		out, err := Resolve(Profile{
			ResidentTier: "gemma4-26b-a4b",
			ConfigSeed:   map[string]any{"agent_model": "offload-12b"},
		}, "t", opt)
		if err != nil {
			t.Fatal(err)
		}
		if got := out["agent_model"]; got != "offload-12b" {
			t.Fatalf("explicit config_seed.agent_model must win, got %v", got)
		}
	})

	t.Run("RAM-overlay blank-out beats derivation", func(t *testing.T) {
		// A RAM tier that drops the big model blanks the seat explicitly —
		// the derived value must not resurrect it.
		out, err := Resolve(Profile{
			ResidentTier:      "gemma4-26b-a4b",
			ConfigSeedMidHigh: map[string]any{"agent_model": ""},
		}, "t", Options{Home: "/x", RAMTier: "mid"})
		if err != nil {
			t.Fatal(err)
		}
		if got := out["agent_model"]; got != "" {
			t.Fatalf("blank-out must survive derivation, got %v", got)
		}
	})

	t.Run("text-only tier with no derivation still resolves to nil", func(t *testing.T) {
		out, err := Resolve(Profile{}, "t", opt)
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			t.Fatalf("empty profile must stay nil, got %v", out)
		}
	})
}
