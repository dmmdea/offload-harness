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

// captures the request the client sends so we can assert the OpenAI wire mapping.
func TestLLMClientMapsWireAndParsesToolCalls(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"x1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "gemma4-e2b", "", 5*time.Second)
	msgs := []Msg{
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "p0", Name: "noop", Args: `{}`}}},
		{Role: "tool", ToolCallID: "p0", Content: "prior result"},
	}
	specs := []ToolSpec{{Name: "list_dir", Description: "list a dir", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}
	comp, err := c.Chat(context.Background(), msgs, specs, 256)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// --- response parsing ---
	if comp.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", comp.FinishReason)
	}
	if len(comp.Msg.ToolCalls) != 1 || comp.Msg.ToolCalls[0].Name != "list_dir" ||
		comp.Msg.ToolCalls[0].Args != `{"path":"."}` || comp.Msg.ToolCalls[0].ID != "x1" {
		t.Errorf("parsed tool call wrong: %+v", comp.Msg.ToolCalls)
	}
	// --- request mapping ---
	if body["model"] != "gemma4-e2b" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream must be false, got %v", body["stream"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools not mapped: %v", body["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "list_dir" || fn["parameters"] == nil {
		t.Errorf("tool function mapping wrong: %v", fn)
	}
	wm, _ := body["messages"].([]any)
	if len(wm) != 3 {
		t.Fatalf("messages count = %d, want 3", len(wm))
	}
	// assistant message must carry tool_calls in OpenAI shape
	asst := wm[1].(map[string]any)
	atc, _ := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || len(atc) != 1 {
		t.Errorf("assistant tool_calls not mapped: %v", asst)
	}
	if f := atc[0].(map[string]any)["function"].(map[string]any); f["name"] != "noop" || f["arguments"] != "{}" {
		t.Errorf("assistant tool_call function wrong: %v", f)
	}
	// tool result must map to role:tool + tool_call_id
	toolMsg := wm[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "p0" || toolMsg["content"] != "prior result" {
		t.Errorf("tool result mapping wrong: %v", toolMsg)
	}
	if auth != "" {
		t.Errorf("no Authorization expected for keyless local endpoint, got %q", auth)
	}
}

func TestLLMClientParsesStopAndSendsBearer(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "sek", 5*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 64)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if comp.FinishReason != "stop" || comp.Msg.Content != "all done" || len(comp.Msg.ToolCalls) != 0 {
		t.Errorf("stop parse wrong: %+v / %q", comp.Msg, comp.FinishReason)
	}
	if auth != "Bearer sek" {
		t.Errorf("bearer = %q, want Bearer sek", auth)
	}
}

func TestLLMClientErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "m", "", 5*time.Second)
	if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "x"}}, nil, 16); err == nil {
		t.Fatal("expected error on 500")
	}
}
