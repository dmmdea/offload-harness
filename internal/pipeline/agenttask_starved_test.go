package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The node-side half of the empty-final protocol (0.115.8, register D-42):
// an EMPTY final answer is a defer, never a re-pack. Before this, the loop's
// "done" with Output "" reached repackStructured, which turned "" into a
// schema-valid all-empty object, which failed acceptance on the delegator,
// which retried on a seat with the wall's leftovers (2026-09-10: every empty
// structure in the corpus).

// starvedChat is the vLLM shape the 2026-09-10 probe recorded on both fleet
// seats: content null, the unclosed think block under `reasoning`, finish
// "length", reasoning_tokens == completion_tokens.
func starvedChat(int64) string {
	return `{"choices":[{"message":{"role":"assistant","content":null,"reasoning":"Thinking Process: 1. Analyze…"},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":13000,"completion_tokens":4096,"completion_tokens_details":{"reasoning_tokens":4096}}}`
}

func TestRunAgentTaskReasoningStarvedFinalDefersAsBudgetWithoutARepack(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      starvedChat, // every turn: whole budget in the think block
		repack:    func(int64) string { return `{"answer":""}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "empty final answer") {
		t.Fatalf("deferred/reason = %v/%q, want an empty-final defer", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q (the completion budget went to the think block)", wire.DeferClass, core.DeferClassBudget)
	}
	if wire.StopReason != "reasoning_starved" || wire.StopNote == "" {
		t.Fatalf("stop_reason/stop_note = %q/%q", wire.StopReason, wire.StopNote)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatal("an empty final must never spend a re-pack completion")
	}
	if len(wire.Structured) != 0 {
		t.Fatalf("structured = %s, want none — nothing was answered", wire.Structured)
	}
	// Exactly two planner completions: the step and its one thinking-off
	// re-issue — no 4x raise, no nudge (1x + 1x, not 1x + 4x + 4x).
	if len(wire.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (step + re-issue): %+v", len(wire.Calls), wire.Calls)
	}
	if wire.Calls[0].ReasoningTokens != 4096 || wire.Calls[0].FinishReason != "length" || !wire.Calls[1].ThinkingOff {
		t.Fatalf("call records = %+v", wire.Calls)
	}
	if !strings.Contains(wire.ResponseShape, "reasoning_key=reasoning") || !strings.Contains(wire.ResponseShape, "reasoning_tokens=reported") || !strings.Contains(wire.ResponseShape, "completions=2") {
		t.Fatalf("response_shape = %q, want the observed vLLM shape recorded (D-45)", wire.ResponseShape)
	}
	if wire.TokensOut != 8192 || wire.SeatTokensIn != 26000 {
		t.Fatalf("tokens_out/seat_tokens_in = %d/%d, want both generations ledgered on the deferred row", wire.TokensOut, wire.SeatTokensIn)
	}
}

func TestRunAgentTaskEmptyFinalOnStopDefersAsAbstention(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("") }, // closes with nothing, twice
		repack:    func(int64) string { return `{"answer":""}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention || wire.StopReason != "empty" {
		t.Fatalf("deferred/class/stop = %v/%q/%q, want an abstention on stop_reason empty (reason %q)", wire.Deferred, wire.DeferClass, wire.StopReason, wire.Reason)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatal("an empty final must never spend a re-pack completion")
	}
}

// TestRunAgentTaskContractThinkingOverridesTheBox: a contract's `thinking`
// is validated at the door and reaches the build; an unknown value is refused
// by name at decode, before any model call.
func TestRunAgentTaskContractThinkingIsValidatedAtTheDoor(t *testing.T) {
	c := testContract()
	c.Thinking = "maybe"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("Validate() = %v, want a thinking-vocabulary error", err)
	}
	c.Thinking = "off"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want off accepted", err)
	}
}
