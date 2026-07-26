package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestChatCapturesServeStats: the client must surface the server's own
// accounting (KV reuse + REAL token counts) — the only trustworthy basis for
// the Phase D measurement, since the ladder's own estimator is chars/4.
func TestChatCapturesServeStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
		  "choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":1769,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":1768}},
		  "timings":{"cache_n":1768,"prompt_n":1,"prompt_ms":38.0,"predicted_n":1,"predicted_ms":12.5}
		}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Msg.Content != "ok" {
		t.Fatalf("content = %q", comp.Msg.Content)
	}
	if comp.Serve == nil {
		t.Fatal("Serve is nil — the server reported timings and they were dropped")
	}
	if comp.Serve.CacheN != 1768 || comp.Serve.PromptN != 1 {
		t.Fatalf("timings wrong: %+v", *comp.Serve)
	}
	if comp.Serve.UsagePromptTokens != 1769 || comp.Serve.UsageCachedTokens != 1768 {
		t.Fatalf("usage wrong: %+v", *comp.Serve)
	}
	if comp.Serve.PromptMS != 38.0 {
		t.Fatalf("prompt_ms = %v", comp.Serve.PromptMS)
	}
}

// A backend that reports no accounting must leave Serve NIL — "unmeasured"
// must never be readable as "measured zero reuse".
func TestChatServeNilWhenBackendSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Serve != nil {
		t.Fatalf("Serve must be nil for a silent backend, got %+v", *comp.Serve)
	}
}

// The production request body must be BYTE-IDENTICAL to what it was before the
// instrumentation: capturing the response must never perturb the request (a
// changed request would invalidate every KV measurement it is meant to take).
func TestChatRequestBodyUnchangedByInstrumentation(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "m", "", 10*time.Second)
	if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 7); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"model": true, "messages": true, "max_tokens": true, "temperature": true, "stream": true}
	for k := range got {
		if !want[k] {
			t.Errorf("request grew an unexpected field %q — instrumentation must not change the wire request", k)
		}
	}
	if _, ok := got["cache_prompt"]; ok {
		t.Error("cache_prompt must not be sent: the default behaviour is what production uses and what we measure")
	}
}
