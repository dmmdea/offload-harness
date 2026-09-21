package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// seedRulesDoc is the slice of profiles.json the house seed rules read.
type seedRulesDoc struct {
	Profiles map[string]struct {
		Backend      string                     `json:"backend"`
		ResidentTier string                     `json:"resident_tier"`
		Include26B   bool                       `json:"include_26b"`
		MoE26B       string                     `json:"moe_26b"`
		VLLMSeat     *struct{}                  `json:"vllm_seat"`
		ConfigSeed   map[string]json.RawMessage `json:"config_seed"`
	} `json:"profiles"`
}

func loadSeedRulesDoc(t *testing.T) seedRulesDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc seedRulesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("profiles.json has no profiles — every gate below went blind")
	}
	return doc
}

func sortedTiers(doc seedRulesDoc) []string {
	names := make([]string, 0, len(doc.Profiles))
	for n := range doc.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestNoGPUTierParksTheMoEExpertsInRAM enforces the operator's hard rule of 2026-09-10 02:00
// ("THE THREE CARDS DO THE INFERENCE — RAM IS OVERFLOW ONLY"): a --cpu-moe that parks most expert
// layers in RAM is forbidden; only a PARTIAL spill of a model that does not fit its card is
// sanctioned. `moe_26b: "cpu_moe"` renders --cpu-moe — every expert in RAM — so no tier that has a
// GPU may seed it. The `cpu` tier is exempt only because it has no card to overflow FROM.
//
// It exists because the rule was written, recorded, and then not applied to the seeds: four tiers
// kept "cpu_moe", and the Aorus served the 26B that way live until 2026-09-21.
func TestNoGPUTierParksTheMoEExpertsInRAM(t *testing.T) {
	doc := loadSeedRulesDoc(t)
	for _, name := range sortedTiers(doc) {
		p := doc.Profiles[name]
		if p.Backend == "cpu" {
			continue
		}
		if p.Include26B && p.MoE26B == "cpu_moe" {
			t.Errorf("%s (backend %q) seeds moe_26b \"cpu_moe\": that parks every expert in RAM on a box with a GPU. "+
				"Fit it on the card (\"gpu\") or drop it (include_26b false, moe_26b \"drop\")", name, p.Backend)
		}
	}
}

// TestEveryAgentSeatIsChosenNotDerived forbids a tier from getting its agent planner seat by the
// silent resident_tier fallback (internal/tierseed/tierseed.go, "derive only when resident_tier
// DIFFERS from the row's effective workhorse"). An agent seat is a measured choice: it must be
// named in config_seed.agent_model, or bound by a vllm_seat.
//
// The fallback is how amd-gcn shipped gemma4-e2b as its agent seat — the one model measured to FAIL
// the agent contract on that very box (18.6 s, 2026-09-20), while the seat that PASSED three times
// (qwen3.5-4b-agent) existed only as a hand edit on the node, so every fresh install lost it.
func TestEveryAgentSeatIsChosenNotDerived(t *testing.T) {
	doc := loadSeedRulesDoc(t)
	const defaultWorkhorse = "offload-e4b"
	for _, name := range sortedTiers(doc) {
		p := doc.Profiles[name]
		if _, explicit := p.ConfigSeed["agent_model"]; explicit || p.VLLMSeat != nil || p.ResidentTier == "" {
			continue
		}
		workhorse := defaultWorkhorse
		if raw, ok := p.ConfigSeed["model"]; ok {
			var m string
			if json.Unmarshal(raw, &m) == nil && m != "" {
				workhorse = m
			}
		}
		if p.ResidentTier != workhorse {
			t.Errorf("%s derives its agent seat from resident_tier %q: name the measured agent in "+
				"config_seed.agent_model instead", name, p.ResidentTier)
		}
	}
}

// TestAmdGcnSeedsTheSeatItWasMeasuredOn pins amd-gcn to what ran on its reference box (Binxarn,
// Ryzen 5 5625U / Vega 7, Vulkan), per plans/receipts/2026-09-20-binxarn-firmware-and-tune.md:
//
//   - the E4B is the hot model (resident == workhorse offload-e4b): Vulkan pp512 129.08 / tg128
//     11.93. The E2B "weakest path" the tier carried was a projection that was never replaced.
//   - the agent seat is qwen3.5-4b-agent: smoke PASS 44.7 s, a real contract PASS 188 s, a 32k
//     contract PASS 143 s. gemma4-e2b FAILED in 18.6 s and offload-e4b FAILED in 31 s.
//   - agent_seat_tok_s 10 is load-bearing: the first run without it deferred with 1 s of wall left,
//     because the wall was sized from a rate this iGPU does not reach.
func TestAmdGcnSeedsTheSeatItWasMeasuredOn(t *testing.T) {
	doc := loadSeedRulesDoc(t)
	p, ok := doc.Profiles["amd-gcn"]
	if !ok {
		t.Fatal("tier amd-gcn not found — this gate went blind")
	}
	if p.ResidentTier != "offload-e4b" {
		t.Errorf("amd-gcn resident_tier = %q, want \"offload-e4b\" (the E4B measured on binxarn)", p.ResidentTier)
	}
	want := map[string]any{
		"agent_model":         "qwen3.5-4b-agent",
		"agent_seat_tok_s":    float64(10),
		"agent_max_tokens":    float64(2048),
		"agent_timeout_sec":   float64(900),
		"fleet_agent_enabled": true,
	}
	for key, w := range want {
		raw, ok := p.ConfigSeed[key]
		if !ok {
			t.Errorf("amd-gcn config_seed lacks %q (want %v) — it is live on binxarn and a fresh install loses it", key, w)
			continue
		}
		var got any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("amd-gcn config_seed.%s: %v", key, err)
			continue
		}
		if got != w {
			t.Errorf("amd-gcn config_seed.%s = %v, want %v", key, got, w)
		}
	}
}
