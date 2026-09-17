package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewLLMClientNormalizesV1Suffix (register S-37) pins the ONE rule for a
// configured base: whatever `/v1` shape the operator wrote, the client dials
// /v1/chat/completions exactly once.
//
// NewLLMClient used to trim only a trailing slash and then append
// "/v1/chat/completions", so a base written as "http://x/v1" — the shape
// `nim_endpoint` documents and the shape an operator copies off a vLLM seat's
// own docs — dialled "/v1/v1/chat/completions" and the seat answered 404. The
// harness already owned the normalisation (swapclient.BaseURL), in the package
// every other reader of `endpoint` goes through; this client was the one that
// did not.
func TestNewLLMClientNormalizesV1Suffix(t *testing.T) {
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		t.Run("base"+strings.ReplaceAll(suffix, "/", "_"), func(t *testing.T) {
			var gotPaths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPaths = append(gotPaths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			c := NewLLMClient(srv.URL+suffix, "m", "", 5*time.Second)
			if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 16); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if len(gotPaths) != 1 || gotPaths[0] != "/v1/chat/completions" {
				t.Fatalf("base %q dialled %v, want one request to /v1/chat/completions", srv.URL+suffix, gotPaths)
			}
		})
	}
}
