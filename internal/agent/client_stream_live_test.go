package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatStreamsAndReportsProgress(t *testing.T) {
	var sawStream, sawUsageOpt bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		sawStream, _ = req["stream"].(bool)
		if so, ok := req["stream_options"].(map[string]any); ok {
			sawUsageOpt, _ = so["include_usage"].(bool)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseFixture)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	var ticks int
	ctx := ContextWithProgress(context.Background(), func(int) { ticks++ })
	comp, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !sawStream || !sawUsageOpt {
		t.Fatalf("request must ask for a stream with usage: stream=%v include_usage=%v", sawStream, sawUsageOpt)
	}
	if comp.Msg.Content != "Hello" || comp.Serve == nil || comp.Serve.UsageCompletionTokens != 9 || ticks != 5 {
		t.Fatalf("content=%q serve=%+v ticks=%d", comp.Msg.Content, comp.Serve, ticks)
	}
	if len(comp.Msg.ToolCalls) != 1 || comp.Msg.ToolCalls[0].Name != "fetch" {
		t.Fatalf("tool calls lost through the stream: %+v", comp.Msg.ToolCalls)
	}
}

func TestChatFallsBackToJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 64)
	if err != nil || comp.Msg.Content != "Hello" || comp.Serve == nil || comp.Serve.UsageCompletionTokens != 2 {
		t.Fatalf("json fallback broke: %+v %v", comp, err)
	}
}

func TestChatEngineErrorMidStreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"He\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"error\":{\"message\":\"NCCL error: unhandled cuda error\",\"type\":\"server_error\"}}\n\n")
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	_, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 64)
	if err == nil {
		t.Fatal("an engine death mid-stream must be an error, not a truncated answer")
	}
}
