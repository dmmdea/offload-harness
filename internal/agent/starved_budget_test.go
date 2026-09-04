package agent

import (
	"context"
	"testing"
)

// TestReasoningStarvedBudgetIsRetriedOnceWithMoreTokens: a step that ends on
// finish_reason=length with no content and no tool calls is a completion budget
// eaten by reasoning; the loop must re-issue that step once with a larger
// max_tokens rather than return an empty answer.
func TestReasoningStarvedBudgetIsRetriedOnceWithMoreTokens(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3).WithMaxTokens(1024)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "the answer" {
		t.Fatalf("output %q", res.Output)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want 2 chat calls (starved + retried), got %d", len(client.seen))
	}
	if l.maxTokens != 4096 {
		t.Fatalf("budget should have been raised to 4096, got %d", l.maxTokens)
	}
	if res.Steps != 1 {
		t.Fatalf("the retry must not consume a step: steps=%d", res.Steps)
	}
}

// TestStarvedBudgetRetriesOnlyOnce: after the one budget raise, a second
// empty-and-truncated answer gets the one empty-message nudge (0.113.6) and a
// third empty answer ends the run with an empty output — bounded at three calls,
// no unbounded escalation.
func TestStarvedBudgetRetriesOnlyOnce(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "length"},
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "length"},
	}}
	l := NewLoop(client, full, 4).WithMaxTokens(1024)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 3 {
		t.Fatalf("want exactly 3 chat calls (starved, raised, nudged), got %d", len(client.seen))
	}
	if res.Output != "" {
		t.Fatalf("output %q", res.Output)
	}
}
