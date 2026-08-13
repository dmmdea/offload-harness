// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

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

// benchStub stands in for llama-swap so the request body and the timings
// parsing are testable without loading a model.
func benchStub(t *testing.T, handler func(body map[string]any) any) (*rootFlags, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(handler(body))
	}))
	t.Setenv("LLAMASWAP_BASE_URL", srv.URL)
	return &rootFlags{}, srv.Close
}

// The two encoded traps, asserted on the wire: the production route is used,
// and cache_prompt is false in the body.
func TestBenchRequestSendsCachePromptFalseWithTheFixedPrompt(t *testing.T) {
	var seen map[string]any
	flags, closeFn := benchStub(t, func(body map[string]any) any {
		seen = body
		return map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"timings": map[string]any{
				"prompt_n": 320, "prompt_ms": 200.0, "prompt_per_second": 1600.0,
				"predicted_n": 256, "predicted_ms": 2000.0, "predicted_per_second": 128.0,
			},
		}
	})
	defer closeFn()

	run := benchRequest(context.Background(), flags, "gemma-4-e2b", benchPrompt, "", 256, false, 10*time.Second)
	if run.Error != "" {
		t.Fatalf("bench request failed: %s", run.Error)
	}
	if cache, ok := seen["cache_prompt"].(bool); !ok || cache {
		t.Errorf(`body must carry "cache_prompt": false, got %v`, seen["cache_prompt"])
	}
	if seen["stream"] != false {
		t.Errorf("bench must not stream (timings come from the completed response), got %v", seen["stream"])
	}
	msgs, _ := seen["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected one user message, got %v", seen["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].(string)
	if content != benchPrompt {
		t.Error("the benchmark prompt on the wire is not the fixed const: results across runs stop being comparable")
	}

	if run.Source != "timings" {
		t.Errorf("rate source = %q, want the server's timings object", run.Source)
	}
	if run.PPPerSec != 1600 || run.TGPerSec != 128 {
		t.Errorf("rates = %v/%v, want the timings values 1600/128", run.PPPerSec, run.TGPerSec)
	}
	if run.PromptN != 320 {
		t.Errorf("prompt_n = %d, want 320", run.PromptN)
	}
	if run.WallMS <= 0 {
		t.Error("wall clock not measured")
	}
}

// A response with no timings block must be labeled degraded, never silently
// turned into a comparable rate.
func TestBenchRequestLabelsTheWallClockFallback(t *testing.T) {
	flags, closeFn := benchStub(t, func(map[string]any) any {
		return map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 320, "completion_tokens": 64},
		}
	})
	defer closeFn()

	run := benchRequest(context.Background(), flags, "m", benchPrompt, "", 64, false, 10*time.Second)
	if run.Error != "" {
		t.Fatalf("unexpected error: %s", run.Error)
	}
	if !strings.Contains(run.Source, "NO timings") {
		t.Errorf("the fallback must announce itself, got source = %q", run.Source)
	}
	if run.PPPerSec != 0 {
		t.Errorf("a prompt-processing rate must NOT be invented from wall clock, got %v", run.PPPerSec)
	}
	if run.PromptN != 320 || run.PredictedN != 64 {
		t.Errorf("usage fallback token counts = %d/%d", run.PromptN, run.PredictedN)
	}
}

// Timings that carry counts but no per-second fields are still usable: derive
// from the server's own millisecond figures, which is not the same thing as
// deriving from wall clock.
func TestBenchRequestDerivesRatesFromServerMilliseconds(t *testing.T) {
	flags, closeFn := benchStub(t, func(map[string]any) any {
		return map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"timings": map[string]any{
				"prompt_n": 100, "prompt_ms": 250.0,
				"predicted_n": 50, "predicted_ms": 500.0,
			},
		}
	})
	defer closeFn()

	run := benchRequest(context.Background(), flags, "m", benchPrompt, "", 50, false, 10*time.Second)
	if run.Source != "timings" {
		t.Fatalf("source = %q", run.Source)
	}
	if run.PPPerSec != 400 {
		t.Errorf("PP = %v, want 100 tokens / 0.25 s = 400", run.PPPerSec)
	}
	if run.TGPerSec != 100 {
		t.Errorf("TG = %v, want 50 tokens / 0.5 s = 100", run.TGPerSec)
	}
}

func TestBenchRequestReportsHTTPFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"grammar not supported on this route"}`))
	}))
	defer srv.Close()
	t.Setenv("LLAMASWAP_BASE_URL", srv.URL)

	run := benchRequest(context.Background(), &rootFlags{}, "m", benchPrompt, "", 8, false, 10*time.Second)
	if run.Error == "" {
		t.Fatal("a 500 must be recorded as a failed sample, not a zero-rate success")
	}
	if !strings.Contains(run.Error, "500") {
		t.Errorf("error should name the status: %s", run.Error)
	}
}

// The fixed prompt must stay long enough to actually exercise prompt
// processing; a short prompt makes PP numbers noise.
func TestBenchPromptIsSubstantial(t *testing.T) {
	if len(benchPrompt) < 1000 {
		t.Errorf("benchmark prompt is only %d chars; it is documented as ~300 tokens", len(benchPrompt))
	}
	if strings.Contains(benchPrompt, "\n") {
		t.Error("the prompt must stay a single line so it survives copy/paste into curl unchanged")
	}
}
