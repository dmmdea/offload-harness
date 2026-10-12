package agent

// The forced final step (D-89) offers no tools and asks for the answer. One seat family answers it with a tool call
// written as text ("<tool_call><function=...>") in a third of its forced finals (35 of 106 runs that reached one,
// against none of 241 on every other seat), because the transcript above is full of the tool-call turns the template
// keeps continuing. The run used to END there, with the seat's text discarded and the stop filed as `budget`
// ("a task too large for one contract: split it"), with most of its wall unspent (30 of 35 had more than half left).
//
// It is now re-asked ONCE, with a different prompt: the decode of that step is greedy, so an identical request would
// repeat itself. The offending text is quoted back, the seat is told nothing it writes as a call will run, and the
// poisoned turn stays out of the transcript. A second offence ends the run as before, now with both excerpts.

import (
	"context"
	"strings"
	"testing"
	"time"
)

const toolCallText = "<tool_call>\n<function=search_files>\n</function>\n</tool_call>"

func asTextCall() Completion {
	return Completion{Msg: Msg{Role: "assistant", Content: toolCallText}, FinishReason: "stop"}
}

func answerOf(s string) Completion {
	return Completion{Msg: Msg{Role: "assistant", Content: s}, FinishReason: "stop"}
}

// TestForcedFinalToolCallAsTextIsReaskedOnceWithAChangedPrompt: the seat writes a tool call on the tool-less final
// step; the loop asks again, with the offence quoted, and takes the answer that comes.
func TestForcedFinalToolCallAsTextIsReaskedOnceWithAChangedPrompt(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), asTextCall(), answerOf("the answer")}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "done" || res.Output != "the answer" || res.Steps != 2 {
		t.Fatalf("stop=%q output=%q steps=%d, want the re-asked answer as a finished run in 2 steps", res.StopReason, res.Output, res.Steps)
	}
	if execs != 1 {
		t.Fatalf("execs=%d: the tool call written on the final step must not run", execs)
	}
	if !strings.Contains(res.StopNote, "forced final answer") || !strings.Contains(res.StopNote, "re-asked") {
		t.Fatalf("note=%q, want the forced-final note saying the seat was re-asked", res.StopNote)
	}
	if client.calls != 3 {
		t.Fatalf("the seat was called %d times, want 3 (a tool step, the final step, one re-ask)", client.calls)
	}
	if len(client.seenSpecs[2]) != 0 {
		t.Fatalf("the re-ask offered %v, want no tools", specNames(client.seenSpecs[2]))
	}
	seen := client.seen[2]
	last := seen[len(seen)-1]
	if last.Role != "user" || last.Content == FinalAnswerTurn || !strings.Contains(last.Content, "<tool_call>") {
		t.Fatalf("the re-ask's last turn = %+v, want a user turn that is not the answer-now turn again and quotes the offending text", last)
	}
	for _, m := range seen {
		if m.Role == "assistant" && strings.Contains(m.Content, "<tool_call>") {
			t.Fatalf("the offending turn stayed in the transcript the re-ask was sent: %+v", m)
		}
	}
	if n := countFinalTurns(seen); n != 1 {
		t.Fatalf("the answer-now turn appears %d times in the re-ask, want once", n)
	}
	if client.seenMax[2] != client.seenMax[1] || !client.seenNoThink[2] {
		t.Fatalf("re-ask max=%d noThink=%v, final step max=%d: it must run at the same budget with thinking off", client.seenMax[2], client.seenNoThink[2], client.seenMax[1])
	}
	if len(res.Calls) != 3 || !res.Calls[1].ForcedFinal || !res.Calls[2].ForcedFinal {
		t.Fatalf("calls[] = %+v, want both final-step completions recorded as forced finals", res.Calls)
	}
}

// TestForcedFinalParsedToolCallThenAnswerIsRecovered: the same, for an engine that parsed the call into a tool call.
func TestForcedFinalParsedToolCallThenAnswerIsRecovered(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), searchCall("c2", "b"), answerOf("the answer")}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "done" || res.Output != "the answer" || execs != 1 {
		t.Fatalf("stop=%q output=%q execs=%d, want the answer and the parsed final-step call not executed", res.StopReason, res.Output, execs)
	}
	last := client.seen[2][len(client.seen[2])-1]
	if !strings.Contains(last.Content, "search_files") {
		t.Fatalf("the re-ask %q does not name the tool the seat called", last.Content)
	}
}

// TestForcedFinalSecondToolCallIsTheBudgetStopWithBothExcerpts: bounded at one re-ask; the stop keeps the class it
// had and the evidence now names what the seat wrote both times.
func TestForcedFinalSecondToolCallIsTheBudgetStopWithBothExcerpts(t *testing.T) {
	execs := 0
	second := Completion{Msg: Msg{Role: "assistant", Content: "Sure.\n<function=list_dir>\n</function>"}, FinishReason: "stop"}
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), asTextCall(), second, answerOf("never asked for")}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "budget" || res.Output != "" {
		t.Fatalf("stop=%q output=%q, want the budget stop with no output", res.StopReason, res.Output)
	}
	if client.calls != 3 {
		t.Fatalf("the seat was called %d times, want 3: one re-ask and no more", client.calls)
	}
	for _, want := range []string{"forced final step", "as text", "re-asked", "<tool_call>", "<function=list_dir>"} {
		if !strings.Contains(res.StopNote, want) {
			t.Errorf("note %q does not contain %q", res.StopNote, want)
		}
	}
}

// TestForcedFinalReaskIsGatedByTheWall: with less wall than one more turn needs there is no re-ask, exactly as the
// list-cap re-issue is gated, so the re-ask can never create the overrun it exists to avoid.
func TestForcedFinalReaskIsGatedByTheWall(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), asTextCall(), answerOf("too late")}}
	loop := NewLoop(client, oneTool(&execs), 2)
	loop.listCapMinTurn = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := loop.Run(ctx, "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "budget" || client.calls != 2 {
		t.Fatalf("stop=%q calls=%d, want the budget stop after 2 calls: no wall for a re-ask", res.StopReason, client.calls)
	}
}

// TestForcedFinalAnswerThatIsNotAToolCallIsNotReasked: an ordinary answer on the final step is taken as it always was.
func TestForcedFinalAnswerThatIsNotAToolCallIsNotReasked(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{searchCall("c1", "a"), answerOf("the answer"), answerOf("never asked for")}}
	res, err := NewLoop(client, oneTool(&execs), 2).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "done" || client.calls != 2 || strings.Contains(res.StopNote, "re-asked") {
		t.Fatalf("stop=%q calls=%d note=%q, want the answer taken at once with no re-ask", res.StopReason, client.calls, res.StopNote)
	}
}
