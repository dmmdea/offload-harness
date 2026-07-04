package nimclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// canned OpenAI-compatible chat response (mirrors build.nvidia.com shape).
const chatJSON = `{
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "9.8 is larger."},
    "finish_reason": "stop"
  }],
  "model": "nvidia/nemotron-3-ultra-550b-a55b",
  "usage": {"prompt_tokens": 35, "completion_tokens": 7}
}`

func TestChatSendsRequestAndParsesContent(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatJSON)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "secret-key", 5*time.Second)
	res, err := c.Chat(context.Background(), "nvidia/nemotron-3-ultra-550b-a55b", "be terse", "Which is larger, 9.11 or 9.8?", 64, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "9.8 is larger." {
		t.Errorf("content = %q, want %q", res.Content, "9.8 is larger.")
	}
	if res.TokensIn != 35 || res.TokensOut != 7 {
		t.Errorf("tokens = %d/%d, want 35/7", res.TokensIn, res.TokensOut)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("auth = %q, want Bearer secret-key", gotAuth)
	}
	// the request must carry the model, the system prompt, and the user text.
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if req.Model != "nvidia/nemotron-3-ultra-550b-a55b" {
		t.Errorf("sent model = %q", req.Model)
	}
	if req.Stream {
		t.Errorf("stream must be false")
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Content != "Which is larger, 9.11 or 9.8?" {
		t.Errorf("messages wrong: %+v", req.Messages)
	}
}

func TestChatNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	// a self-hosted NIM on localhost needs no bearer key — the header must be absent.
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, _ = io.WriteString(w, chatJSON)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "", 5*time.Second)
	if _, err := c.Chat(context.Background(), "m", "", "hi", 8, 0); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if hadAuth {
		t.Errorf("Authorization header must be absent when apiKey is empty")
	}
}

func TestChatFallsBackToReasoningContent(t *testing.T) {
	// reasoning models can leave message.content empty and put the text in
	// reasoning_content (or get cut at max_tokens mid-thought) — surface both
	// so an empty content is not reported as a blank success.
	const reasoningJSON = `{"choices":[{"message":{"content":"","reasoning_content":"thinking out loud"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":8}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, reasoningJSON)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "k", 5*time.Second)
	res, err := c.Chat(context.Background(), "m", "", "go", 8, 0)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.ReasoningContent != "thinking out loud" {
		t.Errorf("reasoning = %q", res.ReasoningContent)
	}
	if !res.Truncated {
		t.Errorf("Truncated must be true on finish_reason=length")
	}
}

func TestChatErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"invalid api key"}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "bad", 5*time.Second)
	_, err := c.Chat(context.Background(), "m", "", "hi", 8, 0)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error should carry status + body, got %v", err)
	}
}

func TestIsHostedNVIDIA(t *testing.T) {
	cases := map[string]bool{
		"https://integrate.api.nvidia.com/v1": true,
		"https://ai.api.nvidia.com/v1":        true,
		"http://127.0.0.1:8000/v1":            false,
		"http://localhost:8000/v1":            false,
	}
	for base, want := range cases {
		if got := IsHostedNVIDIA(base); got != want {
			t.Errorf("IsHostedNVIDIA(%q) = %v, want %v", base, got, want)
		}
	}
}

func TestAPIKeyFromEnvPrecedenceAndTrim(t *testing.T) {
	// NGC_API_KEY is the fallback (the name NVIDIA's own docs use)...
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "ngc-key")
	if got := APIKeyFromEnv(); got != "ngc-key" {
		t.Errorf("fallback to NGC_API_KEY: got %q", got)
	}
	// ...but NVIDIA_API_KEY wins when both are set, and is trimmed.
	t.Setenv("NVIDIA_API_KEY", "  nv-key  ")
	if got := APIKeyFromEnv(); got != "nv-key" {
		t.Errorf("NVIDIA_API_KEY should win and be trimmed: got %q", got)
	}
}

func TestKeyForBaseOnlySendsToNVIDIAHosts(t *testing.T) {
	t.Setenv("NGC_API_KEY", "")
	t.Setenv("NVIDIA_API_KEY", "sek")
	// hosted NVIDIA base -> the env key is used
	if got := KeyForBase("https://integrate.api.nvidia.com/v1"); got != "sek" {
		t.Errorf("hosted base should get the key, got %q", got)
	}
	// any non-NVIDIA base (self-hosted, a typo, a third party) -> NO key, so the
	// NVIDIA secret is never transmitted off-NVIDIA.
	for _, base := range []string{"http://127.0.0.1:8000/v1", "https://evil.example/v1", "http://localhost:9000/v1"} {
		if got := KeyForBase(base); got != "" {
			t.Errorf("non-NVIDIA base %q must NOT receive the key, got %q", base, got)
		}
	}
}

func TestListModelsParsesAndSorts(t *testing.T) {
	const modelsJSON = `{"object":"list","data":[{"id":"meta/llama-3.3-70b-instruct"},{"id":"nvidia/nemotron-3-ultra-550b-a55b"},{"id":"deepseek-ai/deepseek-v4-flash"}]}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, modelsJSON)
	}))
	defer srv.Close()

	c := New(srv.URL+"/v1", "k", 5*time.Second)
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	want := []string{"deepseek-ai/deepseek-v4-flash", "meta/llama-3.3-70b-instruct", "nvidia/nemotron-3-ultra-550b-a55b"}
	if len(ids) != len(want) {
		t.Fatalf("got %d ids, want %d (%v)", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q (not sorted?)", i, ids[i], want[i])
		}
	}
}
