package agent

import (
	"context"
	"testing"
)

// TestEmptyFinalMessageIsNudgedOnce: an assistant message with no content and no
// tool calls is not accepted as the answer on first sight — the loop asks once
// for the answer, then takes what comes.
func TestEmptyFinalMessageIsNudgedOnce(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 4)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "the answer" {
		t.Fatalf("output %q", res.Output)
	}
	if len(client.seen) != 2 {
		t.Fatalf("want 2 chat calls, got %d", len(client.seen))
	}
	last := client.seen[1][len(client.seen[1])-1]
	if last.Role != "user" || last.Content == "" {
		t.Fatalf("the nudge must be the last user message before the retry, got role=%q %q", last.Role, last.Content)
	}
}

// TestEmptyFinalMessageNudgeIsBounded: two empties in a row end the run with an
// empty output — no unbounded nagging.
func TestEmptyFinalMessageNudgeIsBounded(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 4)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) != 2 || res.Output != "" {
		t.Fatalf("calls=%d output=%q", len(client.seen), res.Output)
	}
}
