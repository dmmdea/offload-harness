package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// loopedFinal is the answer shape the guard exists for: a correct opening,
// then the same block over and over until the budget is gone.
var loopedFinal = "summary:\nExo-Bench measures TPS.\nnumbers:\n" + strings.Repeat(loopBlock, 20) + "- 1 sentence cap for "

// TestRepetitionLoopFinalIsTreatedAsCutAndReissuedOnce (D-95b): a degenerate
// loop is a cut answer even when the engine says `stop`. The run re-issues
// ONCE with the list caps AND the do-not-repeat sentence, and calls[] keeps
// the finish reason the engine reported — the guard reads the text, it does
// not rewrite the seat's report.
func TestRepetitionLoopFinalIsTreatedAsCutAndReissuedOnce(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: loopedFinal}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: "summary:\nshort and complete.\nnumbers:\n- 100 words cap for summary (enforced)."}, FinishReason: "stop"},
	}}
	const instr = "cap every list at 8 items"
	l := NewLoop(client, mkTools("list_dir"), 2).WithCutFinalReissue(instr, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 3 {
		t.Fatalf("want 3 completions (tool step, looped final, ONE re-issue), got %d", len(client.seen))
	}
	last := client.seen[2]
	turn := last[len(last)-1].Content
	if !strings.Contains(turn, instr) || !strings.Contains(turn, NoRepeatInstruction) {
		t.Fatalf("the re-issue must carry the list caps AND the do-not-repeat sentence: %q", turn)
	}
	if res.FinalReissue != FinalReissueListCap {
		t.Fatalf("final_reissue = %q, want %q", res.FinalReissue, FinalReissueListCap)
	}
	if len(res.Calls) != 3 || res.Calls[1].FinishReason != "stop" {
		t.Fatalf("calls[] must keep the ENGINE's finish reason for the looped turn: %+v", res.Calls)
	}
	if res.OutputTruncated || !strings.HasPrefix(res.Output, "summary:\nshort and complete.") {
		t.Fatalf("the re-issued answer is the output: truncated=%v output=%q", res.OutputTruncated, res.Output)
	}
}

// TestASecondRepetitionLoopAbstainsWithTheTrimmedPartial: the re-issue is
// bounded at one. A seat that loops again ends the run flagged cut, with the
// evidence in stop_note and ONE copy of the block in output — never the twenty
// that used to ride out to the caller.
func TestASecondRepetitionLoopAbstainsWithTheTrimmedPartial(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: loopedFinal}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: loopedFinal}, FinishReason: "length"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 2).WithCutFinalReissue("cap every list at 8 items", 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 3 {
		t.Fatalf("the re-issue fires ONCE: want 3 completions, got %d", len(client.seen))
	}
	if !res.OutputTruncated {
		t.Fatal("a looped final is a CUT final — the run must be flagged truncated")
	}
	if !strings.Contains(res.StopNote, `repetition loop (20x "- 100 words cap`) {
		t.Fatalf("stop_note must name the loop: %q", res.StopNote)
	}
	if n := strings.Count(res.Output, "- 5 quotes cap (enforced)."); n != 1 {
		t.Fatalf("output must carry ONE copy of the block, got %d", n)
	}
	if !strings.Contains(res.Output, "[repetition trimmed x19]") {
		t.Fatalf("output must say what was trimmed: %q", clip(res.Output, 400))
	}
}

// TestACleanFinalIsNeverTrimmed: the guard runs on every final, so a correct
// long list must pass through byte for byte with nothing recorded.
func TestACleanFinalIsNeverTrimmed(t *testing.T) {
	answer := "summary:\nExo-Bench measures TPS.\nnumbers:\n" + distinctList(30)
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: answer}, FinishReason: "stop"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 2).WithCutFinalReissue("cap every list at 8 items", 30*time.Second)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != answer || res.OutputTruncated || res.FinalReissue != "" {
		t.Fatalf("a clean answer was touched: truncated=%v reissue=%q changed=%v", res.OutputTruncated, res.FinalReissue, res.Output != answer)
	}
	if strings.Contains(res.StopNote, "repetition loop") {
		t.Fatalf("stop_note = %q", res.StopNote)
	}
}
