package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The server's completion count is the one number that says how much the seat
// GENERATED. It was decoded and dropped until 0.115.5, which is why an agent run
// that generated for minutes ledgered as 0 tokens.
func TestChatCapturesCompletionUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":120,"completion_tokens":37}
		}`))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Serve == nil {
		t.Fatal("Serve is nil — usage was reported and dropped")
	}
	if comp.Serve.UsagePromptTokens != 120 || comp.Serve.UsageCompletionTokens != 37 {
		t.Fatalf("usage wrong: %+v", *comp.Serve)
	}
}

// A run's seat usage is the SUM over its turns, reported on every exit — the
// budget-ended run is exactly the one that generated for minutes and used to be
// ledgered as 0, so the "budget" path is pinned beside "done".
func TestLoopSumsSeatUsageOnDoneAndOnBudget(t *testing.T) {
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(_ context.Context, args string) (string, error) { return "echoed", nil },
	}}
	turn := func(content string, calls []ToolCall, fin string, in, out int) Completion {
		return Completion{Msg: Msg{Role: "assistant", Content: content, ToolCalls: calls}, FinishReason: fin,
			Serve: &ServeStats{UsagePromptTokens: in, UsageCompletionTokens: out}}
	}
	done := &fakeClient{script: []Completion{
		turn("", []ToolCall{tc("c1", "echo", `{}`)}, "tool_calls", 300, 20),
		turn("final", nil, "stop", 400, 30),
	}}
	res, err := NewLoop(done, tools, 10).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || res.TokensIn != 700 || res.TokensOut != 50 {
		t.Fatalf("done run: stop=%q in=%d out=%d, want done/700/50", res.StopReason, res.TokensIn, res.TokensOut)
	}

	budget := &fakeClient{script: []Completion{
		turn("", []ToolCall{tc("c1", "echo", `{}`)}, "tool_calls", 500, 900),
	}}
	res, err = NewLoop(budget, tools, 1).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "budget" {
		t.Fatalf("stop = %q, want budget", res.StopReason)
	}
	if res.TokensIn != 500 || res.TokensOut != 900 {
		t.Fatalf("budget run lost its usage: in=%d out=%d, want 500/900", res.TokensIn, res.TokensOut)
	}

	// A backend that reports no usage yields 0, never a fabricated count.
	silent := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "final"}, FinishReason: "stop"}}}
	res, err = NewLoop(silent, nil, 3).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TokensIn != 0 || res.TokensOut != 0 {
		t.Fatalf("silent backend must report 0/0, got %d/%d", res.TokensIn, res.TokensOut)
	}
}
