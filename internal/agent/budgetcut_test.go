package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// PR #462 review, item 2: the same defect class at the sibling sites. vLLM's
// finish_reason is not a reliable cut signal, so every "was this cut by the
// budget?" test reads cutByBudget: finish "length" OR the server's
// completion_tokens reached the call's max_tokens.

func TestCutByBudget(t *testing.T) {
	cases := []struct {
		name string
		c    Completion
		max  int
		want bool
	}{
		{"length", Completion{FinishReason: "length"}, 1024, true},
		{"stop-at-cap", Completion{FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1024}}, 1024, true},
		{"tool_calls-over-cap", Completion{FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 1030}}, 1024, true},
		{"stop-under-cap", Completion{FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1023}}, 1024, false},
		{"stop-no-usage", Completion{FinishReason: "stop"}, 1024, false},
		{"stop-unknown-cap", Completion{FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1024}}, 0, false},
	}
	for _, c := range cases {
		if got := cutByBudget(c.c, c.max); got != c.want {
			t.Errorf("%s: cutByBudget = %v, want %v", c.name, got, c.want)
		}
	}
}

// A content-only final with finish "stop" but completion_tokens == max_tokens
// is a TRUNCATED final: re-issued at the final budget, and a second cut is
// flagged OutputTruncated.
func TestTruncatedFinalAtTheCapReportedAsStopIsReissuedThenFlagged(t *testing.T) {
	full := mkTools("list_dir")
	cut := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "partial"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1024}},
		{Msg: Msg{Role: "assistant", Content: "longer partial"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 4096}},
	}}
	res, err := NewLoop(cut, full, 3).WithMaxTokens(1024).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cut.seen) != 2 {
		t.Fatalf("Chat calls = %d, want 2: the cut final and its re-issue", len(cut.seen))
	}
	if cut.seenMax[1] != 4096 || !cut.seenNoThink[1] {
		t.Errorf("the re-issue must run at the final budget with thinking off: max=%v nothink=%v", cut.seenMax, cut.seenNoThink)
	}
	if !res.OutputTruncated || res.Output != "longer partial" || res.StopReason != "done" {
		t.Fatalf("truncated=%v output=%q stop=%q, want a flagged cut answer", res.OutputTruncated, res.Output, res.StopReason)
	}
}

// CONTROL: a "stop" final under the cap is an answer, not a cut.
func TestFinalUnderTheCapIsNotTruncated(t *testing.T) {
	c := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 900}},
	}}
	res, err := NewLoop(c, mkTools("list_dir"), 3).WithMaxTokens(1024).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputTruncated || len(c.seen) != 1 {
		t.Fatalf("truncated=%v calls=%d, want an unflagged answer in one call", res.OutputTruncated, len(c.seen))
	}
}

// Starvation: an empty completion that used the whole budget is starved, not
// merely empty, whatever the finish reason says.
func TestStarvationAtTheCapReportedAsStop(t *testing.T) {
	cases := []struct {
		name string
		c    Completion
		kind string
	}{
		{"stop-at-cap-reasoning-text", Completion{FinishReason: "stop", Reasoning: "thinking…", Serve: &ServeStats{UsageCompletionTokens: 1024}}, StopReasoningStarved},
		{"stop-at-cap-nothing", Completion{FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1024}}, StopReasoningStarved},
		{"stop-under-cap-reasoning-text", Completion{FinishReason: "stop", Reasoning: "thinking…", Serve: &ServeStats{UsageCompletionTokens: 50}}, StopEmpty},
		{"stop-under-cap-nothing", Completion{FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 50}}, StopEmpty},
	}
	for _, c := range cases {
		kind, basis, ok := c.c.Starvation(1024)
		if !ok || kind != c.kind {
			t.Errorf("%s: kind=%q ok=%v (basis %q), want %q", c.name, kind, ok, basis, c.kind)
		}
	}
}

// The loop's starvation branch reads the same signal: two empty completions
// that each used their whole budget end on reasoning_starved, not empty.
func TestLoopStarvationAtTheCapReportedAsStop(t *testing.T) {
	c := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 1024}},
		{Msg: Msg{Role: "assistant"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 4096}},
	}}
	res, err := NewLoop(c, mkTools("list_dir"), 3).WithMaxTokens(1024).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopReasoningStarved {
		t.Fatalf("stop_reason = %q (note %q), want %q", res.StopReason, res.StopNote, StopReasoningStarved)
	}
}

// The client's reasoning fallback: a think block cut at the cap is not the
// answer even when the engine says "stop".
func TestChatReasoningCutAtTheCapReportedAsStopIsNotTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"Let me think about"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":64}}`))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "" || comp.Reasoning == "" {
		t.Fatalf("content=%q reasoning=%q: a think block cut at the cap was promoted to the answer", comp.Msg.Content, comp.Reasoning)
	}
}
