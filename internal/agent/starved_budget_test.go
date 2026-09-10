package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The empty-final protocol (0.115.8, register D-41…D-44). A THINKING seat that
// spends its whole completion budget in the think block returns finish_reason
// "length", content "" and the unclosed block under `reasoning`. Until 0.115.8
// the loop raised the budget 4x and re-ran, nudged with a user turn, and then
// accepted a SECOND empty as "done" — 1x + 4x + 4x the budget for zero visible
// output, published as a result (2026-09-10, 20,526 tokens on the Qube 27B).
//
// Now the step is re-issued ONCE, with thinking off at the final budget and
// no nudge turn; a second empty ends the run on a NAMED stop.

func starvedTurn(reasoningTokens, completionTokens int) Completion {
	return Completion{
		Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "length",
		Reasoning: strings.Repeat("Thinking Process: analyze the request. ", 20), ReasoningKey: "reasoning",
		Serve: &ServeStats{UsageCompletionTokens: completionTokens, UsageReasoningTokens: reasoningTokens},
	}
}

// TestReasoningStarvedStepIsReissuedOnceWithThinkingOffAtTheFinalBudget: the
// re-issue is the SAME request (no nudge turn appended), rendered in
// non-thinking mode, at 4x the step budget — and it does not spend a step.
func TestReasoningStarvedStepIsReissuedOnceWithThinkingOffAtTheFinalBudget(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		starvedTurn(1024, 1024),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop", ThinkingOff: true},
	}}
	l := NewLoop(client, full, 3).WithMaxTokens(1024)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "the answer" || res.StopReason != "done" {
		t.Fatalf("output %q stop %q", res.Output, res.StopReason)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want 2 chat calls (starved + re-issued), got %d", len(client.seen))
	}
	if client.seenNoThink[0] || !client.seenNoThink[1] {
		t.Fatalf("thinking-off per call = %v, want [false true]", client.seenNoThink)
	}
	if client.seenMax[0] != 1024 || client.seenMax[1] != 4096 {
		t.Fatalf("max_tokens per call = %v, want [1024 4096] (the final budget is 4x the step budget)", client.seenMax)
	}
	if len(client.seen[1]) != len(client.seen[0]) {
		t.Fatalf("the re-issue must send the SAME transcript (no nudge turn): %d vs %d messages", len(client.seen[1]), len(client.seen[0]))
	}
	if l.maxTokens != 1024 {
		t.Fatalf("the step budget must not be raised for the rest of the run, got %d", l.maxTokens)
	}
	if res.Steps != 1 {
		t.Fatalf("the re-issue must not consume a step: steps=%d", res.Steps)
	}
	if len(res.Calls) != 2 || res.Calls[0].ReasoningTokens != 1024 || res.Calls[0].FinishReason != "length" || !res.Calls[1].ThinkingOff || res.Calls[1].MaxTokens != 4096 {
		t.Fatalf("call records = %+v", res.Calls)
	}
}

// TestReasoningStarvedTwiceEndsOnANamedStopWithNoOutput: a second empty is
// terminal — exactly two calls, an empty Output, StopReason reasoning_starved
// and a StopNote carrying the arithmetic. No third generation, no nudge.
func TestReasoningStarvedTwiceEndsOnANamedStopWithNoOutput(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		starvedTurn(4096, 4096),
		starvedTurn(8192, 8192),
		{Msg: Msg{Role: "assistant", Content: "never asked"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 4).WithMaxTokens(4096)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want exactly 2 chat calls (starved, re-issued), got %d", len(client.seen))
	}
	if res.Output != "" || res.StopReason != StopReasoningStarved {
		t.Fatalf("output %q stop %q, want empty + %q", res.Output, res.StopReason, StopReasoningStarved)
	}
	if !strings.Contains(res.StopNote, "8192 of 8192") || !strings.Contains(res.StopNote, `"reasoning"`) {
		t.Fatalf("stop note %q must carry the reasoning/completion arithmetic and the wire key", res.StopNote)
	}
	if client.seenMax[1] != 8192 {
		t.Fatalf("final budget = %d, want the 8192 cap", client.seenMax[1])
	}
	if res.TokensOut != 4096+8192 {
		t.Fatalf("tokens_out = %d, want both generations counted", res.TokensOut)
	}
}

// TestStarvationClassifierNamesTheShape pins Completion.Starvation on the
// four shapes the probe and the corpus produced.
func TestStarvationClassifierNamesTheShape(t *testing.T) {
	cases := []struct {
		name string
		c    Completion
		kind string
		ok   bool
	}{
		{"vllm-length-reasoning-tokens", starvedTurn(200, 200), StopReasoningStarved, true},
		{"length-reasoning-text-no-usage", Completion{FinishReason: "length", Reasoning: "thinking…"}, StopReasoningStarved, true},
		{"length-nothing-reported", Completion{FinishReason: "length"}, StopReasoningStarved, true},
		{"stop-bare-think-block", Completion{FinishReason: "stop", Reasoning: "I have nothing to add."}, StopEmpty, true},
		{"stop-empty", Completion{FinishReason: "stop"}, StopEmpty, true},
		{"answer", Completion{FinishReason: "stop", Msg: Msg{Content: "42"}}, "", false},
		{"tool-call", Completion{FinishReason: "tool_calls", Msg: Msg{ToolCalls: []ToolCall{tc("c", "f", "{}")}}}, "", false},
		{"mostly-answer-under-share", Completion{FinishReason: "length", Msg: Msg{Content: "partial"}, Serve: &ServeStats{UsageCompletionTokens: 100, UsageReasoningTokens: 95}}, "", false},
	}
	for _, c := range cases {
		kind, basis, ok := c.c.Starvation()
		if ok != c.ok || kind != c.kind {
			t.Errorf("%s: kind=%q ok=%v (basis %q), want kind=%q ok=%v", c.name, kind, ok, basis, c.kind, c.ok)
		}
		if ok && basis == "" {
			t.Errorf("%s: an empty completion must carry a basis", c.name)
		}
	}
}

// TestThinkingOffModeRendersEveryCallWithoutThinking: `agent_thinking: off`
// sends the non-thinking kwarg on step 1 already, not only on the re-issue.
func TestThinkingOffModeRendersEveryCallWithoutThinking(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{"path":"."}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3).WithThinking(ThinkingOff)
	if _, err := l.Run(context.Background(), "digest"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, off := range client.seenNoThink {
		if !off {
			t.Fatalf("call %d rendered WITH thinking under ThinkingOff", i)
		}
	}
}

// TestThinkingOnModeNeverSendsTheKwarg: `agent_thinking: on` keeps the retry
// (at the final budget) but never asks for a non-thinking render.
func TestThinkingOnModeNeverSendsTheKwarg(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		starvedTurn(1024, 1024),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3).WithMaxTokens(1024).WithThinking(ThinkingOn)
	res, err := l.Run(context.Background(), "digest")
	if err != nil || res.Output != "the answer" {
		t.Fatalf("res %+v err %v", res.Output, err)
	}
	if client.seenNoThink[0] || client.seenNoThink[1] {
		t.Fatalf("thinking-off per call = %v, want [false false] under ThinkingOn", client.seenNoThink)
	}
	if client.seenMax[1] != 4096 {
		t.Fatalf("the re-issue must still use the final budget: %v", client.seenMax)
	}
}

// TestResponseShapeSummarizesTheSeatsAnswerShape pins the D-45 record.
func TestResponseShapeSummarizesTheSeatsAnswerShape(t *testing.T) {
	if got := ResponseShape(nil); got != "" {
		t.Fatalf("no calls must record nothing, got %q", got)
	}
	calls := []CallRecord{{ToolCalls: 1}, {ReasoningKey: "reasoning", ReasoningTokens: 200}, {ThinkingOff: true}}
	got := ResponseShape(calls)
	for _, want := range []string{"reasoning_key=reasoning", "reasoning_tokens=reported", "tool_calls_parsed=1", "completions=3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("response shape %q lacks %q", got, want)
		}
	}
	if got := ResponseShape([]CallRecord{{ReasoningKey: "reasoning_content"}}); !strings.Contains(got, "reasoning_key=reasoning_content") || !strings.Contains(got, "reasoning_tokens=unreported") {
		t.Fatalf("llama.cpp shape = %q", got)
	}
}

// TestFinalMaxTokensIsFourTimesCappedNeverBelow pins the final budget rule.
func TestFinalMaxTokensIsFourTimesCappedNeverBelow(t *testing.T) {
	for in, want := range map[int]int{0: 4096, 1024: 4096, 2048: 8192, 4096: 8192, 8192: 8192, 16384: 16384} {
		if got := finalMaxTokens(in); got != want {
			t.Errorf("finalMaxTokens(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestParseThinkingMode pins the closed vocabulary shared with core.ValidateThinking.
func TestParseThinkingMode(t *testing.T) {
	for in, want := range map[string]ThinkingMode{"": ThinkingAuto, "auto": ThinkingAuto, "OFF": ThinkingOff, "on": ThinkingOn} {
		got, err := ParseThinkingMode(in)
		if err != nil || got != want {
			t.Errorf("ParseThinkingMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"maybe", "true", "false", "1", "0"} {
		if _, err := ParseThinkingMode(bad); err == nil {
			t.Fatalf("%q must be refused: the vocabulary is exactly auto/on/off (core.ValidateThinking)", bad)
		}
	}
}

// TestReissueThatYieldsAToolCallLetsALaterEmptyFinalReissueAgain: the
// re-issue credit is per EPISODE (bounded per run), not one per run — an
// early empty step whose re-issue produced a tool call must not strip the
// real final answer of its re-issue.
func TestReissueThatYieldsAToolCallLetsALaterEmptyFinalReissueAgain(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		starvedTurn(1024, 1024), // step 1: empty
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{"path":"."}`)}}, FinishReason: "tool_calls"}, // re-issue: a tool call
		starvedTurn(1024, 1024), // step 2: empty again
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"}, // re-issue: the answer
	}}
	l := NewLoop(client, full, 6).WithMaxTokens(1024)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "the answer" || len(client.seen) != 4 {
		t.Fatalf("output %q calls %d", res.Output, len(client.seen))
	}
	if !client.seenNoThink[1] || !client.seenNoThink[3] || client.seenNoThink[2] {
		t.Fatalf("thinking-off per call = %v, want [false true false true]", client.seenNoThink)
	}
}

// TestReissuesAreBoundedPerRun: a third empty episode gets no re-issue.
func TestReissuesAreBoundedPerRun(t *testing.T) {
	full := mkTools("list_dir")
	toolTurn := func(id string) Completion {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "list_dir", `{"path":"`+id+`"}`)}}, FinishReason: "tool_calls"}
	}
	client := &fakeClient{script: []Completion{
		starvedTurn(1024, 1024), toolTurn("c1"),
		starvedTurn(1024, 1024), toolTurn("c2"),
		starvedTurn(1024, 1024), // third episode: no credit left
		{Msg: Msg{Role: "assistant", Content: "never asked"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 8).WithMaxTokens(1024)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 5 || res.StopReason != StopReasoningStarved {
		t.Fatalf("calls %d stop %q, want 5 calls and %q", len(client.seen), res.StopReason, StopReasoningStarved)
	}
}

// TestReissueDoesNotRecitePlanTwice: the plan-recitation block is keyed on
// the step index; the re-issue's step-- must not append the plan a second
// time to the re-issued transcript (review finding 1, 0.115.8).
func TestReissueDoesNotRecitePlanTwice(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "plan.md"), []byte("- step one\n- step two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := mkTools("list_dir")
	toolTurn := func(id string) Completion {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "list_dir", `{"path":"`+id+`"}`)}}, FinishReason: "tool_calls"}
	}
	client := &fakeClient{script: []Completion{
		toolTurn("c1"), toolTurn("c2"), toolTurn("c3"), // steps 1-3
		starvedTurn(1024, 1024), // step 4 (index 3 = recitation step): empty
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"}, // re-issue
	}}
	l := NewLoop(client, full, 8).WithMaxTokens(1024).WithWorktree(dir)
	if _, err := l.Run(context.Background(), "digest"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := client.seen[len(client.seen)-1]
	plans := 0
	for _, m := range last {
		if m.Role == "user" && strings.Contains(m.Content, "step two") {
			plans++
		}
	}
	if plans != 1 {
		t.Fatalf("the re-issued transcript carries the plan %d times, want exactly 1", plans)
	}
}

// TestTruncatedFinalIsFlaggedNotHidden: a NON-empty final cut on "length" is
// still the answer (the caller may use the partial) but the Result says so.
func TestTruncatedFinalIsFlaggedNotHidden(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "## Decisions\n- one\n- tw"}, FinishReason: "length"},
	}}
	res, err := NewLoop(client, full, 3).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OutputTruncated || res.Output == "" || res.StopReason != "done" {
		t.Fatalf("truncated=%v output=%q stop=%q", res.OutputTruncated, res.Output, res.StopReason)
	}
}
