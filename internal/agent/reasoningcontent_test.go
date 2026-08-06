package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Reasoning/harmony models (DeepSeek V4 thinking, gpt-oss) can return
// message.content EMPTY with the whole answer in reasoning_content. The client
// must surface it — this exact blind spot disqualified gpt-oss-20b's free-text
// role and hid an eval answer. Mirrors nimclient's
// TestChatFallsBackToReasoningContent.
func TestChatFallsBackToReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",` +
			`"reasoning_content":"the answer is 42"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "the answer is 42" {
		t.Fatalf("content = %q — reasoning_content was dropped", comp.Msg.Content)
	}
}

// A tool-call turn legitimately has empty content; reasoning_content alongside a
// tool call is the model's scratchpad, NOT the answer, and must not overwrite the
// empty content the loop expects on tool-call turns.
func TestToolCallTurnKeepsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",` +
			`"reasoning_content":"I should call the tool",` +
			`"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
			`"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "" {
		t.Fatalf("tool-call turn content = %q, want empty", comp.Msg.Content)
	}
	if len(comp.Msg.ToolCalls) != 1 || comp.Msg.ToolCalls[0].Name != "f" {
		t.Fatalf("tool calls = %+v", comp.Msg.ToolCalls)
	}
}

// Populated content must always win — the fallback engages ONLY on empty content.
func TestContentWinsOverReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"real answer",` +
			`"reasoning_content":"scratch work"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "real answer" {
		t.Fatalf("content = %q, want the real answer", comp.Msg.Content)
	}
}
