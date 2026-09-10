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

// vLLM (>= 0.11, every --reasoning-parser) returns the hidden channel under
// `reasoning`, not `reasoning_content`, and counts it in
// usage.completion_tokens_details.reasoning_tokens. Probe of 2026-09-10 against
// both fleet vLLM seats (max_tokens 200): finish_reason length, content null,
// reasoning_content absent, reasoning 670 chars, reasoning_tokens 200 == completion_tokens.

func vllmStarvedBody() string {
	return `{"choices":[{"message":{"role":"assistant","content":null,` +
		`"reasoning":"Thinking Process: 1. **Analyze the Request:** the user wants…"},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":12345,"completion_tokens":200,"completion_tokens_details":{"reasoning_tokens":200}}}`
}

// TestChatDecodesVLLMReasoningFieldAndReasoningTokens: the truncated think block
// is decoded (Completion.Reasoning, key "reasoning"), the reasoning token count
// lands on Serve, and the content stays EMPTY — a cut think block is not the
// answer and must not be handed to the loop or the re-pack as one.
func TestChatDecodesVLLMReasoningFieldAndReasoningTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(vllmStarvedBody()))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, mkSpecs("list_dir"), 200)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "" {
		t.Fatalf("content = %q — a think block cut on finish_reason length was promoted to the answer", comp.Msg.Content)
	}
	if comp.Reasoning == "" || comp.ReasoningKey != "reasoning" {
		t.Fatalf("reasoning = %q key = %q; the vLLM field was not decoded", comp.Reasoning, comp.ReasoningKey)
	}
	if comp.Serve == nil || comp.Serve.UsageReasoningTokens != 200 || comp.Serve.UsageCompletionTokens != 200 {
		t.Fatalf("serve = %+v; reasoning_tokens not decoded", comp.Serve)
	}
	if kind, _, ok := comp.Starvation(); !ok || kind != StopReasoningStarved {
		t.Fatalf("starvation = %q/%v, want %q", kind, ok, StopReasoningStarved)
	}
}

// TestChatFallsBackToVLLMReasoningOnAFinishedTurn: the 0.81 fallback (answer in
// the reasoning channel, finish "stop", no tool calls) now covers the `reasoning`
// key too — gpt-oss / DeepSeek-class seats behind vLLM.
func TestChatFallsBackToVLLMReasoningOnAFinishedTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning":"the answer is 42"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "the answer is 42" {
		t.Fatalf("content = %q — the finished reasoning-channel answer was dropped", comp.Msg.Content)
	}
}

// TestChatTruncatedReasoningContentIsNotTheAnswer: the same rule on the
// llama.cpp key — a `reasoning_content` cut on length stays out of content.
func TestChatTruncatedReasoningContentIsNotTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"Let me think about"},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "" || comp.ReasoningKey != "reasoning_content" || comp.Reasoning == "" {
		t.Fatalf("content=%q key=%q reasoning=%q", comp.Msg.Content, comp.ReasoningKey, comp.Reasoning)
	}
}

// TestChatSendsEnableThinkingFalseOnlyWhenAsked: ContextWithoutThinking puts
// `chat_template_kwargs: {"enable_thinking": false}` on the wire; a plain
// context emits no such key at all (the historical request, byte for byte).
func TestChatSendsEnableThinkingFalseOnlyWhenAsked(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "q"}}, nil, 64); err != nil {
		t.Fatal(err)
	}
	comp, err := c.Chat(ContextWithoutThinking(context.Background()), []Msg{{Role: "user", Content: "q"}}, nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := bodies[0]["chat_template_kwargs"]; present {
		t.Fatalf("a plain call must not send chat_template_kwargs: %v", bodies[0])
	}
	kw, _ := bodies[1]["chat_template_kwargs"].(map[string]any)
	if v, ok := kw["enable_thinking"].(bool); !ok || v {
		t.Fatalf("thinking-off call sent chat_template_kwargs=%v, want {enable_thinking:false}", bodies[1]["chat_template_kwargs"])
	}
	if !comp.ThinkingOff {
		t.Fatal("the completion must record that it was rendered without thinking")
	}
}

func mkSpecs(names ...string) []ToolSpec {
	specs := make([]ToolSpec, 0, len(names))
	for _, t := range mkTools(names...) {
		specs = append(specs, t.ToolSpec)
	}
	return specs
}
