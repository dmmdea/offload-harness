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

// TestGenerateVisionInterleaved asserts that GenerateVisionInterleaved POSTs a
// user message whose content array is, in order: text "<0.0 seconds>", image,
// text "<0.5 seconds>", image, then text "the question".
func TestGenerateVisionInterleaved(t *testing.T) {
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
	dataURI1 := "data:image/png;base64,iVBORw0KGgo="
	dataURI2 := "data:image/png;base64,iVBORw0KGgo="

	res, err := c.GenerateVisionInterleaved(
		context.Background(),
		"", "system prompt",
		[]string{"<0.0 seconds>", "<0.5 seconds>"},
		[]string{dataURI1, dataURI2},
		"the question",
		"", 64, 0, 0,
	)
	if err != nil {
		t.Fatalf("GenerateVisionInterleaved: %v", err)
	}
	if res.Content != "a small dog" {
		t.Errorf("Content = %q, want %q", res.Content, "a small dog")
	}

	// cache_prompt must be false on the vision path.
	if cp, ok := gotBody["cache_prompt"].(bool); !ok || cp {
		t.Errorf("cache_prompt = %#v, want false on the vision path", gotBody["cache_prompt"])
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages not an array: %#v", gotBody["messages"])
	}

	// Find the user message.
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

	// Expected order: text "<0.0 seconds>", image, text "<0.5 seconds>", image, text "the question"
	if len(content) != 5 {
		t.Fatalf("expected 5 content parts, got %d: %#v", len(content), content)
	}

	checkText := func(i int, want string) {
		t.Helper()
		p, _ := content[i].(map[string]any)
		if p["type"] != "text" {
			t.Errorf("part[%d] type = %v, want \"text\"", i, p["type"])
		}
		if p["text"] != want {
			t.Errorf("part[%d] text = %q, want %q", i, p["text"], want)
		}
	}
	checkImage := func(i int) {
		t.Helper()
		p, _ := content[i].(map[string]any)
		if p["type"] != "image_url" {
			t.Errorf("part[%d] type = %v, want \"image_url\"", i, p["type"])
		}
		iu, _ := p["image_url"].(map[string]any)
		url, _ := iu["url"].(string)
		if !strings.HasPrefix(url, "data:image/") {
			t.Errorf("part[%d] image_url.url = %q, want data:image/ prefix", i, url)
		}
	}

	checkText(0, "<0.0 seconds>")
	checkImage(1)
	checkText(2, "<0.5 seconds>")
	checkImage(3)
	checkText(4, "the question")
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
