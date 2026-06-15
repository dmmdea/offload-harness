package llamaclient

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

// cannedChatResp is a minimal valid /v1/chat/completions response.
const cannedChatResp = `{
	"choices": [{
		"message": {"content": "a small dog"},
		"finish_reason": "stop"
	}],
	"usage": {"prompt_tokens": 42, "completion_tokens": 7},
	"timings": {"predicted_per_second": 12.5}
}`

// TestGenerateVision asserts the vision path sends an ARRAY content with an
// image_url data URI part plus the top-level grammar field, and decodes the
// canned response into GenResult telemetry.
func TestGenerateVision(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, cannedChatResp)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "qwen3vl-4b", 5*time.Second)
	dataURI := "data:image/png;base64,iVBORw0KGgo="
	res, err := c.GenerateVision(context.Background(), "", "you are a vision model", "what is in this image?", []string{dataURI}, "root ::= [a-z ]+", 64, 0, 0)
	if err != nil {
		t.Fatalf("GenerateVision: %v", err)
	}

	// Telemetry from the canned response.
	if res.Content != "a small dog" {
		t.Errorf("Content = %q, want %q", res.Content, "a small dog")
	}
	if res.TokensIn != 42 || res.TokensOut != 7 {
		t.Errorf("tokens = (%d,%d), want (42,7)", res.TokensIn, res.TokensOut)
	}
	if res.TokPerSec != 12.5 {
		t.Errorf("TokPerSec = %v, want 12.5", res.TokPerSec)
	}

	// Top-level grammar field must be present.
	if g, ok := gotBody["grammar"].(string); !ok || g == "" {
		t.Errorf("grammar field missing/empty: %#v", gotBody["grammar"])
	}

	// cache_prompt MUST be false on the vision path: consecutive-image KV reuse
	// can corrupt output (llama.cpp #17200). This is the load-bearing mitigation —
	// guard it so a future edit can't silently flip it back to true.
	if cp, ok := gotBody["cache_prompt"].(bool); !ok || cp {
		t.Errorf("cache_prompt = %#v, want false on the vision path (llama.cpp #17200)", gotBody["cache_prompt"])
	}

	// The user message content must be an ARRAY containing an image_url part
	// whose url starts with data:image/.
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages not an array: %#v", gotBody["messages"])
	}
	// Find the user message (last one with role "user").
	var userMsg map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "user" {
			userMsg = mm
		}
	}
	if userMsg == nil {
		t.Fatalf("no user message in %#v", msgs)
	}
	content, ok := userMsg["content"].([]any)
	if !ok {
		t.Fatalf("user content is not an ARRAY: %#v", userMsg["content"])
	}
	foundImage := false
	for _, part := range content {
		p, _ := part.(map[string]any)
		if p["type"] == "image_url" {
			iu, _ := p["image_url"].(map[string]any)
			url, _ := iu["url"].(string)
			if strings.HasPrefix(url, "data:image/") {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Errorf("no image_url part with data:image/ url in %#v", content)
	}

	// System message should also be present (as array content).
	hasSystem := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "system" {
			hasSystem = true
		}
	}
	if !hasSystem {
		t.Errorf("system message missing: %#v", msgs)
	}
}

// TestGenerateTextContentIsString proves the text path is unchanged: the user
// message content must be a plain STRING, not an array.
func TestGenerateTextContentIsString(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, cannedChatResp)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "offload-e4b", 5*time.Second)
	res, err := c.Generate(context.Background(), "", "sys", "hello", "", 32, 0, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Content != "a small dog" {
		t.Errorf("Content = %q", res.Content)
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages not an array: %#v", gotBody["messages"])
	}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] != "user" {
			continue
		}
		if _, isStr := mm["content"].(string); !isStr {
			t.Errorf("user content is not a plain STRING (text path changed): %#v", mm["content"])
		}
	}
}
