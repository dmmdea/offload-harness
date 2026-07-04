package llamaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSplitBudgetKeepsTotalTimeout (LO-9): the 2s connect timeout must NOT cap
// the whole request — a response that arrives after connectTimeout but within
// the total budget (the cold-swap shape: llama-swap holds the request while
// the model loads) still succeeds.
func TestSplitBudgetKeepsTotalTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(connectTimeout + 500*time.Millisecond) // past the dial budget, inside the total
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "m", 10*time.Second)
	res, err := c.Generate(context.Background(), "", "", "hola", "", 16, 0, 0)
	if err != nil {
		t.Fatalf("slow-but-within-budget response must succeed: %v", err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q, want ok", res.Content)
	}
}

// TestSplitBudgetTransportConfigured: New installs a cloned transport with a
// custom dialer (not the shared http.DefaultTransport), preserving the total
// client timeout.
func TestSplitBudgetTransportConfigured(t *testing.T) {
	c := New("http://127.0.0.1:1", "", "m", 42*time.Second)
	if c.http.Timeout != 42*time.Second {
		t.Fatalf("total budget = %v, want 42s", c.http.Timeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("expected a cloned *http.Transport, got %T", c.http.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("transport must be a clone, not the shared default")
	}
	if tr.DialContext == nil {
		t.Fatal("custom DialContext (connect timeout) missing")
	}
}
