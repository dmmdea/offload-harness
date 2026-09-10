package agent

import (
	"context"
	"strings"
	"testing"
)

// countFinalTurns counts the forced-final user turns in a transcript.
func countFinalTurns(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && m.Content == FinalAnswerTurn {
			n++
		}
	}
	return n
}

func oneTool(execs *int) []Tool {
	return []Tool{{ToolSpec: ToolSpec{Name: "search_files"}, Exec: func(_ context.Context, _ string) (string, error) {
		*execs++
		return "hit", nil
	}}}
}

func searchCall(id, q string) Completion {
	return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "search_files", `{"pattern":"`+q+`"}`)}}, FinishReason: "tool_calls"}
}

// TestForcedFinalStepOffersNoToolsAndAsksForTheAnswer (0.115.19, D-89): the
// last step of a multi-step run offers no tools, opens with the answer-now
// turn, runs at the final budget with thinking off, and its answer is "done"
// with the forced-final note — the 2026-09-10 27B run ended "budget" with
// nothing because the last step still offered every tool.
func TestForcedFinalStepOffersNoToolsAndAsksForTheAnswer(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		searchCall("c1", "a"), searchCall("c2", "b"),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, oneTool(&execs), 3).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "done" || res.Output != "the answer" || !strings.Contains(res.StopNote, "forced final answer") {
		t.Fatalf("stop=%q output=%q note=%q; want done + the answer + the forced-final note", res.StopReason, res.Output, res.StopNote)
	}
	if len(client.seenSpecs[0]) == 0 || len(client.seenSpecs[1]) == 0 {
		t.Fatal("tools must stay offered before the last step")
	}
	if len(client.seenSpecs[2]) != 0 {
		t.Fatalf("the last step offered %v; want no tools", specNames(client.seenSpecs[2]))
	}
	last := client.seen[2][len(client.seen[2])-1]
	if last.Role != "user" || last.Content != FinalAnswerTurn {
		t.Fatalf("the last step must open with the answer-now turn, got %+v", last)
	}
	if client.seenMax[2] != finalMaxTokens(1024) || !client.seenNoThink[2] {
		t.Fatalf("last step max=%d noThink=%v; want the final budget %d with thinking off", client.seenMax[2], client.seenNoThink[2], finalMaxTokens(1024))
	}
	if len(res.Calls) != 3 || !res.Calls[2].ForcedFinal || res.Calls[1].ForcedFinal {
		t.Fatalf("calls[] must mark only the forced final step: %+v", res.Calls)
	}
}

// A tool call returned on the forced final step is never executed: the run
// ends "budget" with the evidence in stop_note.
func TestForcedFinalToolCallIsNotExecuted(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), searchCall("c2", "b")}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if execs != 1 || res.StopReason != "budget" || res.Steps != 2 || !strings.Contains(res.StopNote, "1 tool call") {
		t.Fatalf("execs=%d stop=%q steps=%d note=%q; want 1 execution, budget, 2 steps, the tool-call note", execs, res.StopReason, res.Steps, res.StopNote)
	}
}

// A tool call written as TEXT on the tool-less final step (no parser runs on a
// request without tools) is a budget stop with a note — not the seat
// tool-call-parser error an unparsed marker means on any other step.
func TestForcedFinalToolCallWrittenAsTextIsABudgetStop(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		searchCall("c1", "a"),
		{Msg: Msg{Role: "assistant", Content: "<tool_call>\n<function=search_files>\n</function>\n</tool_call>"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatalf("a tool call written as text on the forced final must not be the parser error: %v", err)
	}
	if res.StopReason != "budget" || !strings.Contains(res.StopNote, "as text") {
		t.Fatalf("stop=%q note=%q; want budget with the as-text note", res.StopReason, res.StopNote)
	}
}

// A one-step run keeps its tools: its only step may legitimately be the call.
func TestOneStepRunKeepsItsTools(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a")}}
	res, err := NewLoop(client, oneTool(&execs), 1).Run(context.Background(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.seenSpecs[0]) == 0 || execs != 1 || res.StopReason != "budget" || countFinalTurns(client.seen[0]) != 0 {
		t.Fatalf("specs=%v execs=%d stop=%q; a one-step run must offer and execute its tool like before", specNames(client.seenSpecs[0]), execs, res.StopReason)
	}
}

// WithoutForcedFinal restores the pre-0.115.19 last step.
func TestWithoutForcedFinalOffersToolsOnTheLastStep(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), searchCall("c2", "b")}}
	res, err := NewLoop(client, oneTool(&execs), 2).WithoutForcedFinal().Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.seenSpecs[1]) == 0 || execs != 2 || res.StopReason != "budget" {
		t.Fatalf("specs=%v execs=%d stop=%q; WithoutForcedFinal must keep the last step's tools", specNames(client.seenSpecs[1]), execs, res.StopReason)
	}
}

// A seat pinned to ThinkingOn keeps thinking on the forced final step.
func TestForcedFinalUnderThinkingOnKeepsThinking(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		searchCall("c1", "a"),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, oneTool(&execs), 2)
	loop.thinking = ThinkingOn
	if _, err := loop.Run(context.Background(), "extract"); err != nil {
		t.Fatal(err)
	}
	if client.seenNoThink[1] {
		t.Fatal("ThinkingOn must not be switched off on the forced final step")
	}
}

// An empty completion on the forced final is re-issued (the existing rule) and
// the answer-now turn is added once, not once per attempt.
func TestForcedFinalAnswerTurnAddedOnceAcrossAReissue(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		searchCall("c1", "a"),
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "the answer" || countFinalTurns(client.seen[2]) != 1 || len(client.seenSpecs[2]) != 0 {
		t.Fatalf("output=%q final turns=%d specs=%v; want the answer, one final turn, no tools on the re-issue", res.Output, countFinalTurns(client.seen[2]), specNames(client.seenSpecs[2]))
	}
}
