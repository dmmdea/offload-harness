package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// W0.1: the hard-rule seed gates have synthetic RED fixtures.
//
// seed_rules_test.go reads the real profiles.json, so a gate that went blind (a skip that swallowed
// everything, a branch that never fires) passes for as long as the real seeds are clean, and nothing
// ever shows the rule CAN fail. These tests hand the same rule bodies a document that must be refused,
// and one that must not, so each clause of each rule is pinned by a fixture that exists only to pin it.

// seedDoc decodes a profiles document from JSON text, through the same decode path as the real file.
func seedDoc(t *testing.T, js string) seedRulesDoc {
	t.Helper()
	var doc seedRulesDoc
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\n%s", err, js)
	}
	return doc
}

func TestSeedRuleCPUMoERefusesASyntheticGPUTier(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"clean-gpu":  {"backend":"cuda","include_26b":true,"moe_26b":"gpu"},
		"cpu-moe-gpu":{"backend":"cuda","include_26b":true,"moe_26b":"cpu_moe"}
	}}`)
	got := checkNoGPUTierParksTheMoE(doc)
	if len(got) != 1 {
		t.Fatalf("want exactly one finding (the cuda tier that seeds cpu_moe), got %d: %q", len(got), got)
	}
	for _, want := range []string{"cpu-moe-gpu", "cuda", "cpu_moe"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the finding must name %q, got: %s", want, got[0])
		}
	}
	if strings.Contains(got[0], "clean-gpu") {
		t.Errorf("the finding names a tier that is clean: %s", got[0])
	}
}

// The cpu tier has no card to overflow FROM, so it may seed the every-expert placement.
func TestSeedRuleCPUMoEExemptsTheCPUTier(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{"cpu":{"backend":"cpu","include_26b":true,"moe_26b":"cpu_moe"}}}`)
	if got := checkNoGPUTierParksTheMoE(doc); len(got) != 0 {
		t.Fatalf("the cpu tier is exempt, got: %q", got)
	}
}

// Only the combination parks every expert in RAM: a tier that drops the 26B, or places it on the card,
// or spills only a measured part, is not what the rule forbids.
func TestSeedRuleCPUMoELeavesTheOtherPlacementsAlone(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"drops-the-26b":   {"backend":"cuda","include_26b":false,"moe_26b":"cpu_moe"},
		"on-the-card":     {"backend":"cuda","include_26b":true,"moe_26b":"gpu"},
		"partial-spill":   {"backend":"vulkan","include_26b":true,"moe_26b":"n_cpu_moe"},
		"dropped":         {"backend":"cuda","include_26b":true,"moe_26b":"drop"}
	}}`)
	if got := checkNoGPUTierParksTheMoE(doc); len(got) != 0 {
		t.Fatalf("none of these parks every expert in RAM, got: %q", got)
	}
}

func TestSeedRuleRefusesADerivedAgentSeat(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"derives": {"backend":"cuda","resident_tier":"some-resident-model"}
	}}`)
	got := checkAgentSeatsAreChosen(doc)
	if len(got) != 1 {
		t.Fatalf("want exactly one finding, got %d: %q", len(got), got)
	}
	for _, want := range []string{"derives", "some-resident-model", "agent_model"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the finding must name %q, got: %s", want, got[0])
		}
	}
}

// A tier whose agent seat is a vLLM seat names it there; the resident_tier fallback is not in play.
func TestSeedRuleAcceptsAnAgentSeatNamedByAVLLMSeat(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"vllm": {"backend":"cuda","resident_tier":"some-resident-model","vllm_seat":{}}
	}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 0 {
		t.Fatalf("a vllm_seat names the agent seat, got: %q", got)
	}
}

// Added in review round 1: no other fixture has an explicit agent_model beside a differing
// resident_tier, so deleting the `explicit` clause survived the first four.
func TestSeedRuleAcceptsAnExplicitAgentSeatOnADerivingTier(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"explicit": {"backend":"cuda","resident_tier":"some-resident-model","config_seed":{"agent_model":"a-measured-agent"}}
	}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 0 {
		t.Fatalf("config_seed.agent_model chooses the seat, got: %q", got)
	}
}

// The workhorse the rule compares against is config_seed.model when the tier sets one, else the
// harness default (the same fallback tierseed.Resolve compares against).
func TestSeedRuleReadsTheWorkhorseFromConfigSeedModel(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"resident-is-workhorse": {"backend":"cuda","resident_tier":"the-workhorse","config_seed":{"model":"the-workhorse"}}
	}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 0 {
		t.Fatalf("resident_tier equal to config_seed.model derives nothing, got: %q", got)
	}
	// And the other direction: a config_seed.model that is NOT the resident_tier is a derivation.
	doc = seedDoc(t, `{"profiles":{
		"resident-differs": {"backend":"cuda","resident_tier":"the-resident","config_seed":{"model":"the-workhorse"}}
	}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 1 || !strings.Contains(got[0], "resident-differs") {
		t.Fatalf("resident_tier different from config_seed.model derives the seat, got: %q", got)
	}
}

func TestSeedRuleAgentSeatFallsBackToTheHarnessDefaultWorkhorse(t *testing.T) {
	def := config.Default().Model
	doc := seedDoc(t, `{"profiles":{"default-workhorse":{"backend":"cuda","resident_tier":`+jsonString(def)+`}}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 0 {
		t.Fatalf("resident_tier equal to the harness default workhorse derives nothing, got: %q", got)
	}
}

// A tier that declares no resident_tier derives nothing.
func TestSeedRuleAcceptsATierWithNoResidentTier(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{"no-resident":{"backend":"cuda"}}}`)
	if got := checkAgentSeatsAreChosen(doc); len(got) != 0 {
		t.Fatalf("no resident_tier, nothing derived, got: %q", got)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
