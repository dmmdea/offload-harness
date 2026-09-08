package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A reasoning seat must DECLARE its completion budget. There is deliberately no floor:
// the right value is a property of the seat, measured, and a one-size number is wrong in
// BOTH directions.
//
// Measured on the A2 (2026-09-08), same eight contracts, blind-judged, paired on the
// packets both arms answered:
//
//	Qwen3.5-9B   1024 -> 4096:  +0.62  (17 wins, 1 tie, 0 losses) - filler findings 6 -> 0
//	Qwen3.5-4B   1024 -> 4096:  -0.88  ( 6 wins, 0 ties, 15 losses) - filler findings 0 -> 3
//
// Too little budget truncates a thinking-heavy model into degenerate output; too much lets
// a small one ramble. The 4B's own tier was briefly seeded at 4,096 on the strength of a
// single-call cliff measured on the 9B, which is exactly the reasoning this comment now
// exists to prevent: the cliff was real and the inference from it was not.
//
// The loop already covers the starvation emergency - internal/agent/loop.go raises the
// budget 4x (cap 8,192) and re-runs the step - but that raise is ONCE PER RUN and only
// fires when a step returned neither content nor a tool call. It is a safety net, not a
// budget, and a seat that relies on it pays a wasted step every run.

// A tier that seats a REASONING model must seed a completion budget CHOSEN for that seat,
// rather than inheriting the loop default by accident. The test enforces the declaration,
// not a value - see the block above for why a value would be wrong.
//
// The defect this exists to prevent, measured end to end on 2026-09-08: `ampere-16`
// declared a vllm_seat with `reasoning_parser: qwen3` and seeded no `agent_max_tokens`, so
// the node ran every delegation at 1,024 by default. Nothing errored, and the budget was
// never a considered choice. It turned out to matter: on the same eight contracts, blind
// judged, moving 1024 -> 4096 was worth +0.62 to a 9B and -0.88 to the 4B that tier
// actually seats.
//
// Same failure shape as TestAgentWindowMatchesWhatTheAgentSeatServes on the context axis:
// a tier silently inheriting a number instead of stating one, where nothing errors and
// only output quality moves.
func TestAReasoningSeatSeedsItsCompletionBudget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *struct {
				ID              string `json:"id"`
				ReasoningParser string `json:"reasoning_parser"`
			} `json:"vllm_seat"`
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}

	checked := 0
	for tier, p := range doc.Profiles {
		if p.VLLMSeat == nil || p.VLLMSeat.ReasoningParser == "" {
			continue
		}
		checked++
		rawTok, ok := p.ConfigSeed["agent_max_tokens"]
		if !ok {
			t.Errorf("tier %q seats %q with reasoning_parser %q but its config_seed sets no agent_max_tokens: "+
				"every install of this tier then inherits the loop's 1,024 default by accident rather than by "+
				"measurement. Seed a value measured for THIS seat - there is no universal right answer "+
				"(the 9B gained +0.62 going 1024->4096 while the 4B lost 0.88 on the same move).",
				tier, p.VLLMSeat.ID, p.VLLMSeat.ReasoningParser)
			continue
		}
		var tok int
		if err := json.Unmarshal(rawTok, &tok); err != nil {
			t.Errorf("tier %q: agent_max_tokens is not a number: %s", tier, rawTok)
			continue
		}
		if tok <= 0 {
			t.Errorf("tier %q seats a reasoning model and seeds agent_max_tokens = %d, which is not a budget", tier, tok)
		}
	}

	// An empty sweep would make this test vacuously green forever — exactly how a guard
	// stops guarding when a field is renamed or the seats move. Assert it found work.
	if checked == 0 {
		t.Fatal("no tier declares a vllm_seat with a reasoning_parser — this guard matched nothing " +
			"and would pass regardless of the defect it exists to catch; fix the field names above")
	}
	t.Logf("checked %d reasoning seat(s) for a seeded completion budget", checked)
}
