package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// reasoningBudgetFloor is the completion budget a REASONING seat needs before its
// answer survives its own thinking, and it is a measured number, not a guess.
//
// The agent loop's built-in default is 1,024 completion tokens, and reasoning counts
// against that budget. Measured on the Qube 27B seat (2026-09-04): 839 of those 1,024
// went to thinking and answers came back cut or empty, which is why that box's config
// was raised to 4,096. Measured again on the A2's Qwen3.5-9B (2026-09-08), where the
// same shape is total rather than partial:
//
//	finish_reason: length   content: None   reasoning_tokens: 1024 of 1024
//
// The entire budget spent thinking, zero tokens of answer, on a seat whose parsers and
// weights were both fine.
//
// The loop does mitigate — internal/agent/loop.go raises the budget 4x (cap 8,192) and
// re-runs the step — but that raise is ONCE PER RUN, and it only fires when the step
// returned neither content nor a tool call. It is a safety net, not a budget.
const reasoningBudgetFloor = 4096

// A tier that seats a REASONING model must also seed the completion budget that
// reasoning costs. The two are one decision and this test refuses to let them separate.
//
// The defect this exists to prevent, measured end to end on 2026-09-08: `ampere-16`
// declared a vllm_seat with `reasoning_parser: qwen3` but seeded no `agent_max_tokens`,
// so the node ran every delegation at the 1,024 default. Nothing errored. What it
// produced instead was a published seat-quality comparison in which a 4B "beat" the
// same-family 9B — because a 9B thinks more than a 4B, so a FIXED budget truncates the
// larger model harder. The measurement ranked how little each model thought, and the
// conclusion had to be withdrawn.
//
// That is the same failure shape TestAgentWindowMatchesWhatTheAgentSeatServes guards on
// the context axis: a tier quietly advertising less than its own seat needs, where
// nothing errors and only the output quality degrades.
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
				"every install of this tier runs the agent loop at the 1,024 default, where a thinking seat "+
				"spends the whole budget on <think> and returns empty content. Seed agent_max_tokens (>= %d).",
				tier, p.VLLMSeat.ID, p.VLLMSeat.ReasoningParser, reasoningBudgetFloor)
			continue
		}
		var tok int
		if err := json.Unmarshal(rawTok, &tok); err != nil {
			t.Errorf("tier %q: agent_max_tokens is not a number: %s", tier, rawTok)
			continue
		}
		if tok < reasoningBudgetFloor {
			t.Errorf("tier %q seats a reasoning model but seeds agent_max_tokens = %d, below the measured floor %d: "+
				"reasoning counts against this budget, so the seat's answers get truncated before they are written.",
				tier, tok, reasoningBudgetFloor)
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
