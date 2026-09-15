package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

const cutJSON = `{"mechanisms":[{"name":"gradient checkpointing","detail":"recompute activations in the backward pass"},{"name":"paged attention","detail":"`

// TestForcedFinalRunsAtTheFittedBudget (D-95, half A): with a fit installed the
// forced final step opens at the budget the REMAINING wall can decode, not at
// the configured 4x rule — and the run publishes both the budget it used and
// the arithmetic behind it.
func TestForcedFinalRunsAtTheFittedBudget(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	var sawConfigured int
	var sawRemaining time.Duration
	l := NewLoop(client, mkTools("list_dir"), 2).WithMaxTokens(2048).
		WithFinalBudgetFit(func(configured int, remaining time.Duration) (int, string) {
			sawConfigured, sawRemaining = configured, remaining
			return 3592, "final 8192 → 3592 to fit 900 s at 15.0 tok/s (split with the re-pack)"
		})
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawConfigured != 8192 {
		t.Fatalf("the fit must be handed the CONFIGURED budget as its cap, got %d, want 8192", sawConfigured)
	}
	if sawRemaining < 800*time.Second || sawRemaining > 900*time.Second {
		t.Fatalf("the fit must be handed the remaining wall, got %v", sawRemaining)
	}
	if got := client.seenMax[len(client.seenMax)-1]; got != 3592 {
		t.Fatalf("the forced final step ran at %d tokens, want the fitted 3592", got)
	}
	if res.FinalBudgetFit != 3592 || !strings.Contains(res.BudgetNote, "final 8192 → 3592") {
		t.Fatalf("the run must publish the fit and its note: %d %q", res.FinalBudgetFit, res.BudgetNote)
	}
}

// TestNoFitInstalledKeepsTheConfiguredFinalBudget: every caller that installs no
// fit (the CLI, --serve, every test) runs byte-for-byte as before — the final
// step opens at finalMaxTokens and nothing is published.
func TestNoFitInstalledKeepsTheConfiguredFinalBudget(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, mkTools("list_dir"), 2).WithMaxTokens(2048).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := client.seenMax[len(client.seenMax)-1], finalMaxTokens(2048); got != want {
		t.Fatalf("final step budget = %d, want the configured %d", got, want)
	}
	if res.FinalBudgetFit != 0 || res.BudgetNote != "" {
		t.Fatalf("nothing to publish when no fit is installed: %d %q", res.FinalBudgetFit, res.BudgetNote)
	}
}

// TestCutFinalOnASchemaContractIsReissuedOnceWithListCaps (D-95, half B): a
// final answer cut at the completion budget on a schema contract is a JSON
// prefix. Before 0.121.2 the node abstained on it (D-91: a partial cannot be
// re-packed). Now the final turn is re-issued ONCE, thinking off, with the
// schema's own list caps spelled out — the answer that fits.
func TestCutFinalOnASchemaContractIsReissuedOnceWithListCaps(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: cutJSON}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: `{"mechanisms":[{"name":"paged attention","detail":"kv cache in pages"}]}`}, FinishReason: "stop"},
	}}
	const instr = "cap every list at 8 items"
	l := NewLoop(client, mkTools("list_dir"), 1).WithCutFinalReissue(instr, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalReissue != FinalReissueListCap {
		t.Fatalf("final_reissue = %q, want %q", res.FinalReissue, FinalReissueListCap)
	}
	if res.OutputTruncated {
		t.Fatal("the re-issued answer closed on stop — the run must not be flagged truncated")
	}
	if !strings.HasPrefix(res.Output, `{"mechanisms":[{"name":"paged attention"`) {
		t.Fatalf("output %q, want the re-issued complete object", res.Output)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want exactly 2 completions (the cut final and ONE re-issue), got %d", len(client.seen))
	}
	last := client.seen[1]
	if !strings.Contains(last[len(last)-1].Content, instr) {
		t.Fatalf("the re-issue must carry the list-cap instruction, last turn was %q", last[len(last)-1].Content)
	}
	if !client.seenNoThink[1] {
		t.Fatal("the re-issue must render with thinking off")
	}
	if client.seenMax[1] < client.seenMax[0] {
		t.Fatalf("the re-issue must run at the same budget or better: %d then %d", client.seenMax[0], client.seenMax[1])
	}
	if len(res.Calls) != 2 || res.Calls[0].FinishReason != "length" || res.Calls[1].FinishReason != "stop" {
		t.Fatalf("calls[] must record both finish reasons: %+v", res.Calls)
	}
}

// TestCutTwiceAbstainsWithBothReasons: the re-issue is bounded at ONE. A seat
// that cuts the capped answer too ends the run truncated, with the list-cap
// attempt and both finish reasons on the record — never a third generation.
func TestCutTwiceAbstainsWithBothReasons(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: cutJSON}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: cutJSON}, FinishReason: "length"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 1).WithCutFinalReissue("cap every list at 8 items", 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want exactly 2 completions, got %d — the re-issue must fire once", len(client.seen))
	}
	if !res.OutputTruncated || res.FinalReissue != FinalReissueListCap {
		t.Fatalf("a second cut must abstain with the attempt recorded: truncated=%v reissue=%q", res.OutputTruncated, res.FinalReissue)
	}
	if len(res.Calls) != 2 || res.Calls[0].FinishReason != "length" || res.Calls[1].FinishReason != "length" {
		t.Fatalf("calls[] must carry both finish reasons: %+v", res.Calls)
	}
}

// TestNoListCapReissueWithoutASchema: the instruction is derived from the
// output_schema, so a schemaless contract installs none and the loop behaves
// exactly as it did — the pre-existing truncated-final re-issue may still run,
// but it carries no list caps and nothing is recorded as a list-cap re-issue.
func TestNoListCapReissueWithoutASchema(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: cutJSON}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: "a complete prose answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 1)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalReissue != "" {
		t.Fatalf("no schema, no list-cap re-issue: %q", res.FinalReissue)
	}
	for i, seen := range client.seen {
		for _, m := range seen {
			if strings.Contains(m.Content, "cap every list") {
				t.Fatalf("call %d carried a list cap with no schema installed", i)
			}
		}
	}
}

// TestNoListCapReissueWhenTheWallCannotHoldOneMoreTurn: min_turn is the gate.
// A wall with less left than one turn at the seat's rate gets the honest
// abstention at once rather than a re-issue that will be killed mid-generation
// — the D-91 shape the re-issue must not re-create.
func TestNoListCapReissueWhenTheWallCannotHoldOneMoreTurn(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: cutJSON}, FinishReason: "length"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 2).WithCutFinalReissue("cap every list at 8 items", 600*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want 2 completions and no re-issue, got %d", len(client.seen))
	}
	if res.FinalReissue != "" || !res.OutputTruncated {
		t.Fatalf("a wall that cannot hold one more turn must abstain at once: reissue=%q truncated=%v", res.FinalReissue, res.OutputTruncated)
	}
}

// TestNoListCapReissueOnProseThatIsNotAPartialObject: the re-issue exists for a
// JSON-shaped partial. A cut NARRATIVE is not one; re-issuing it with list caps
// would spend a full generation on an instruction that does not apply.
func TestNoListCapReissueOnProseThatIsNotAPartialObject(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "The methodology section describes three phases, the first of which"}, FinishReason: "length"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 2).WithCutFinalReissue("cap every list at 8 items", 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	res, err := l.Run(ctx, "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 2 || res.FinalReissue != "" {
		t.Fatalf("a cut narrative must not earn a list-cap re-issue: calls=%d reissue=%q", len(client.seen), res.FinalReissue)
	}
}
