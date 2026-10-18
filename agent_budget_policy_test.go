package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// budgetPolicyLoopDefaultChosen is the one value of a tier's `agent_budget_policy`
// marker. It says: this tier's agent completion budget is the loop default (1,024) BY
// CHOICE, not by accident, and the figure is CHOSEN, not measured. It is replaced by a
// seeded `agent_max_tokens` when a seat window measures one for the seat. Operator
// decision D8, accepted 2026-10-18: a marker is a declaration, which is what the guard in
// agent_budget_test.go enforces; it is not a measurement, and the field's own
// documentation in profiles.json says so.
const budgetPolicyLoopDefaultChosen = "loop-default-chosen"

// budgetTier is the part of one profiles.json tier the budget guard reads.
type budgetTier struct {
	VLLMSeat *struct {
		ReasoningParser string `json:"reasoning_parser"`
	} `json:"vllm_seat"`
	ExtraVLLMSeats []struct {
		ReasoningParser string `json:"reasoning_parser"`
	} `json:"extra_vllm_seats"`
	ConfigSeed        map[string]json.RawMessage `json:"config_seed"`
	ConfigSeedLowUp   map[string]json.RawMessage `json:"config_seed_ram_low_up"`
	ConfigSeedMidHigh map[string]json.RawMessage `json:"config_seed_ram_mid_high"`
	AgentBudgetPolicy string                     `json:"agent_budget_policy"`
}

// seatsAnAgentModel reports whether the tier binds an agent model by name, in its seed or
// in either RAM overlay. A tier that binds none (the cpu tier, rdna3, the NPU tier) has no
// agent seat whose completion budget could be inherited by accident.
func (p budgetTier) seatsAnAgentModel() bool {
	for _, seed := range []map[string]json.RawMessage{p.ConfigSeed, p.ConfigSeedLowUp, p.ConfigSeedMidHigh} {
		raw, ok := seed["agent_model"]
		if !ok {
			continue
		}
		var name string
		if json.Unmarshal(raw, &name) == nil && name != "" {
			return true
		}
	}
	return false
}

// declaresAReasoningParser reports whether any vLLM seat of the tier serves with a
// reasoning parser: the seats whose thinking is spent inside the completion budget.
func (p budgetTier) declaresAReasoningParser() bool {
	if p.VLLMSeat != nil && p.VLLMSeat.ReasoningParser != "" {
		return true
	}
	for _, s := range p.ExtraVLLMSeats {
		if s.ReasoningParser != "" {
			return true
		}
	}
	return false
}

// budgetProblems says what is wrong with a tier's declaration of its agent completion
// budget; none when it keeps every rule. A tier that seats an agent model must state its
// budget in exactly one way:
//
//   - a seeded `agent_max_tokens` (a number, measured or chosen for the seat);
//   - `agent_thinking: "off"` in its seed (the exemption: no think block to starve, so the
//     loop default is the right size for a plain answer); or
//   - `agent_budget_policy: "loop-default-chosen"` on the tier (the marker: the loop's
//     1,024 is kept on purpose; it is NOT a measurement).
//
// A tier whose vLLM seat declares a reasoning_parser has no marker and no exemption: the
// A2 measurement in agent_budget_test.go is about those seats, and the guard that existed
// before this one already required a number there. The marker is also refused where it
// contradicts a number, repeats the exemption, or sits on a tier with no agent seat,
// because a marker that can be set anywhere states nothing.
func budgetProblems(tier string, p budgetTier) []string {
	var out []string
	var tokens int
	hasTokens := false
	if raw, ok := p.ConfigSeed["agent_max_tokens"]; ok {
		hasTokens = true
		if err := json.Unmarshal(raw, &tokens); err != nil {
			out = append(out, fmt.Sprintf("tier %q: agent_max_tokens is not a number: %s", tier, raw))
			hasTokens = false
		} else if tokens <= 0 {
			out = append(out, fmt.Sprintf("tier %q seeds agent_max_tokens = %d, which is not a budget", tier, tokens))
			hasTokens = false
		}
	}
	thinkingOff := false
	if raw, ok := p.ConfigSeed["agent_thinking"]; ok {
		var v string
		thinkingOff = json.Unmarshal(raw, &v) == nil && v == "off"
	}
	marked := p.AgentBudgetPolicy != ""
	markerOK := p.AgentBudgetPolicy == budgetPolicyLoopDefaultChosen

	if marked && !markerOK {
		out = append(out, fmt.Sprintf("tier %q: agent_budget_policy %q is not %q, the only value there is", tier, p.AgentBudgetPolicy, budgetPolicyLoopDefaultChosen))
	}
	if marked && !p.seatsAnAgentModel() {
		out = append(out, fmt.Sprintf("tier %q sets agent_budget_policy but binds no agent model: the marker describes an agent seat's budget", tier))
	}
	if marked && hasTokens {
		out = append(out, fmt.Sprintf("tier %q sets agent_budget_policy %q and also seeds agent_max_tokens = %d: the marker says no number was chosen; drop it", tier, p.AgentBudgetPolicy, tokens))
	}
	if marked && thinkingOff {
		out = append(out, fmt.Sprintf("tier %q sets agent_budget_policy and seeds agent_thinking off: the exemption already states its budget; drop the marker", tier))
	}
	if p.declaresAReasoningParser() {
		if !hasTokens {
			out = append(out, fmt.Sprintf("tier %q serves a reasoning model (a vllm_seat reasoning_parser) and seeds no agent_max_tokens: "+
				"a thinking seat spends its reasoning inside the completion budget, so the number must be one chosen for THIS seat "+
				"(no marker, no exemption)", tier))
		}
		return out
	}
	if p.seatsAnAgentModel() && !hasTokens && !thinkingOff && !markerOK {
		out = append(out, fmt.Sprintf("tier %q binds an agent model and states no completion budget: every install of it inherits the loop's "+
			"1,024 by accident. Seed agent_max_tokens for the seat, seed agent_thinking \"off\" if it has no think block, "+
			"or set agent_budget_policy %q to keep the default on purpose (chosen, not measured).", tier, budgetPolicyLoopDefaultChosen))
	}
	return out
}

// The guard, widened (register D-24). TestAReasoningSeatSeedsItsCompletionBudget walks the
// tiers whose vLLM seat declares a reasoning_parser, and four more tiers that bind an agent
// model (blackwell-32, -48, -72 and ampere-6) inherited the loop's 1,024 without saying so:
// the same failure shape, one rung down. This walks every tier that binds an agent model.
func TestEveryAgentSeatingTierStatesItsCompletionBudget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]budgetTier `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	checked, marked := 0, 0
	for tier, p := range doc.Profiles {
		if p.seatsAnAgentModel() {
			checked++
		}
		if p.AgentBudgetPolicy != "" {
			marked++
		}
		for _, problem := range budgetProblems(tier, p) {
			t.Error(problem)
		}
	}
	// A sweep that found nothing would stay green after a rename of the fields it reads.
	if checked == 0 {
		t.Fatal("no tier binds an agent model: this guard matched nothing and would pass regardless of the defect it exists to catch")
	}
	t.Logf("checked %d agent-seating tier(s); %d carry the loop-default-chosen marker", checked, marked)
}

// Each rule of the guard is pinned on a synthetic tier, so the guard cannot go green by the
// profiles it happens to read: a rule loosened here fails a case below.
func TestBudgetGuardRules(t *testing.T) {
	seed := func(kv ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = json.RawMessage(kv[i+1])
		}
		return m
	}
	reasoning := &struct {
		ReasoningParser string `json:"reasoning_parser"`
	}{ReasoningParser: "qwen3"}
	cases := []struct {
		name string
		tier budgetTier
		want string // "" = clean; else a substring of the problem
	}{
		{"no agent seat, nothing to state", budgetTier{ConfigSeed: seed("imagegen_engine", `"sdcpp"`)}, ""},
		{"an agent model and nothing else", budgetTier{ConfigSeed: seed("agent_model", `"gemma4-26b-a4b"`)}, "states no completion budget"},
		{"an agent model in the low-RAM overlay only", budgetTier{ConfigSeedLowUp: seed("agent_model", `"qwen3.6-35b-a3b-agent"`)}, "states no completion budget"},
		{"a number", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_max_tokens", "2048")}, ""},
		{"a number that is not a budget", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_max_tokens", "0")}, "not a budget"},
		{"a number that is not a number", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_max_tokens", `"lots"`)}, "not a number"},
		{"thinking off is the exemption", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_thinking", `"off"`)}, ""},
		{"thinking auto is not", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_thinking", `"auto"`)}, "states no completion budget"},
		{"the marker", budgetTier{ConfigSeed: seed("agent_model", `"m"`), AgentBudgetPolicy: budgetPolicyLoopDefaultChosen}, ""},
		{"a marker with another word", budgetTier{ConfigSeed: seed("agent_model", `"m"`), AgentBudgetPolicy: "measured"}, "the only value there is"},
		{"a marker beside a number", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_max_tokens", "4096"), AgentBudgetPolicy: budgetPolicyLoopDefaultChosen}, "also seeds agent_max_tokens"},
		{"a marker beside the exemption", budgetTier{ConfigSeed: seed("agent_model", `"m"`, "agent_thinking", `"off"`), AgentBudgetPolicy: budgetPolicyLoopDefaultChosen}, "exemption already states"},
		{"a marker on a tier with no agent seat", budgetTier{ConfigSeed: seed("imagegen_engine", `"sdcpp"`), AgentBudgetPolicy: budgetPolicyLoopDefaultChosen}, "binds no agent model"},
		{"a reasoning seat with a number", budgetTier{VLLMSeat: reasoning, ConfigSeed: seed("agent_model", `"m"`, "agent_max_tokens", "4096")}, ""},
		{"a reasoning seat with none", budgetTier{VLLMSeat: reasoning, ConfigSeed: seed("agent_model", `"m"`)}, "reasoning model"},
		{"a reasoning seat is not exempted by thinking off", budgetTier{VLLMSeat: reasoning, ConfigSeed: seed("agent_model", `"m"`, "agent_thinking", `"off"`)}, "reasoning model"},
		{"a reasoning seat is not covered by the marker", budgetTier{VLLMSeat: reasoning, ConfigSeed: seed("agent_model", `"m"`), AgentBudgetPolicy: budgetPolicyLoopDefaultChosen}, "reasoning model"},
	}
	for _, tc := range cases {
		got := strings.Join(budgetProblems("t", tc.tier), " | ")
		if tc.want == "" && got != "" {
			t.Errorf("%s: problems = %q, want none", tc.name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems = %q, want one saying %q", tc.name, got, tc.want)
		}
	}
}
