package agent

import (
	"context"
	"testing"
)

// TestEmptyFinalMessageIsReissuedOnceWithoutANudge: an assistant message with no
// content and no tool calls on finish "stop" (a bare, closed think block) is not
// accepted as the answer on first sight — the SAME step is re-issued once with
// thinking off at the final budget, and no user turn is appended (0.115.8
// replaced the 0.113.6 nudge: a nudge cost one more full-budget generation and
// then accepted the second empty as "done").
func TestEmptyFinalMessageIsReissuedOnceWithoutANudge(t *testing.T) {
	full := mkTools("list_dir")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: ""}, FinishReason: "stop", Reasoning: "…", ReasoningKey: "reasoning_content"},
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
	if len(client.seen[1]) != len(client.seen[0]) {
		t.Fatalf("no nudge turn may be appended: %d vs %d messages", len(client.seen[1]), len(client.seen[0]))
	}
	if !client.seenNoThink[1] {
		t.Fatal("the re-issue must render without thinking")
	}
}

// TestEmptyFinalMessageTwiceIsANamedEmptyStop: two empties in a row end the run
// on StopEmpty with an empty output — bounded at two calls, and the stop is
// NAMED so the node can defer instead of re-packing the empty string.
func TestEmptyFinalMessageTwiceIsANamedEmptyStop(t *testing.T) {
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
	if res.StopReason != StopEmpty || res.StopNote == "" {
		t.Fatalf("stop %q note %q, want %q with a note", res.StopReason, res.StopNote, StopEmpty)
	}
}
