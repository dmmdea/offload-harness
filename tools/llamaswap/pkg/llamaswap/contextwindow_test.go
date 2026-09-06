// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package llamaswap_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"llamaswap-pp-cli/pkg/llamaswap"
)

// fakeSwap is a llama-swap with one vLLM seat (no /props: 404, window on the
// backend's /v1/models) and one llama-server seat (/props answers n_ctx).
// It records every path so a test can prove the probe never touched a path
// that would auto-start a cold model.
func fakeSwap(t *testing.T, running []string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[
			  {"id":"qwen3.8-27b-vllm","meta":{"llamaswap":{"aliases":["agent-pool"]}}},
			  {"id":"gemma-4-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}}]}`)
		case "/running":
			fmt.Fprint(w, `{"running":[`)
			for i, id := range running {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"model":%q,"state":"ready"}`, id)
			}
			fmt.Fprint(w, `]}`)
		case "/upstream/qwen3.8-27b-vllm/props":
			http.NotFound(w, r) // vLLM has no /props
		case "/upstream/qwen3.8-27b-vllm/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3.8-27b-vllm","max_model_len":163840},{"id":"agent-pool","max_model_len":163840}]}`)
		case "/upstream/gemma-4-e4b/props":
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":32768},"model_path":"x"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// TestContextWindowVLLMFallsBackToModels: a loaded vLLM seat, addressed by
// alias, reports its window from /v1/models max_model_len after /props 404s.
func TestContextWindowVLLMFallsBackToModels(t *testing.T) {
	srv, _ := fakeSwap(t, []string{"qwen3.8-27b-vllm"})
	c, err := llamaswap.New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.ContextWindow(context.Background(), "agent-pool")
	if err != nil || n != 163840 {
		t.Fatalf("ContextWindow = (%d, %v), want (163840, nil)", n, err)
	}
}

// TestContextWindowLlamaServerReadsProps: the llama-server path is unchanged —
// /props n_ctx answers and /v1/models is never consulted.
func TestContextWindowLlamaServerReadsProps(t *testing.T) {
	srv, paths := fakeSwap(t, []string{"gemma-4-e4b"})
	c, err := llamaswap.New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.ContextWindow(context.Background(), "offload-e4b")
	if err != nil || n != 32768 {
		t.Fatalf("ContextWindow = (%d, %v), want (32768, nil)", n, err)
	}
	for _, p := range paths() {
		if p == "/upstream/gemma-4-e4b/v1/models" {
			t.Fatalf("probe read /v1/models although /props answered: %v", paths())
		}
	}
}

// TestContextWindowColdIsNotLoaded keeps Props' contract: a cold model returns
// ErrNotLoaded and no /upstream path is touched (an /upstream GET would start it).
func TestContextWindowColdIsNotLoaded(t *testing.T) {
	srv, paths := fakeSwap(t, nil)
	c, err := llamaswap.New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContextWindow(context.Background(), "agent-pool"); !errors.Is(err, llamaswap.ErrNotLoaded) {
		t.Fatalf("err = %v, want ErrNotLoaded", err)
	}
	for _, p := range paths() {
		if len(p) >= 10 && p[:10] == "/upstream/" {
			t.Fatalf("cold probe touched %q — that path auto-starts the model", p)
		}
	}
}

// TestContextWindowPropsFailureIsWindowUnknown: a non-404 /props failure on a
// LOADED model is not masked by the /v1/models fallback — it comes back as
// ErrWindowUnknown (residency established, window unreadable), never as
// ErrNotLoaded, with the HTTP status still reachable.
func TestContextWindowPropsFailureIsWindowUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"gemma-4-e4b"}]}`)
		case "/running":
			fmt.Fprint(w, `{"running":[{"model":"gemma-4-e4b","state":"ready"}]}`)
		case "/upstream/gemma-4-e4b/props":
			http.Error(w, "upstream exploded", http.StatusBadGateway)
		default:
			t.Errorf("unexpected path %s — a 502 on /props must not fall through to /v1/models", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := llamaswap.New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ContextWindow(context.Background(), "gemma-4-e4b")
	if !errors.Is(err, llamaswap.ErrWindowUnknown) || errors.Is(err, llamaswap.ErrNotLoaded) {
		t.Fatalf("err = %v, want ErrWindowUnknown and not ErrNotLoaded", err)
	}
	var he *llamaswap.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want the 502 HTTPError reachable through Unwrap", err)
	}
}
