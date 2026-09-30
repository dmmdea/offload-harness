package llamaclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// A call whose context owns the deadline is not cut by the client's own
// Timeout. The re-pack under the liveness monitor needs it: the monitor holds a
// request whose seat's engine is busy for others (ADR 0061), and a transport
// bound of its own sat under that hold, cut the request part-way and re-sent it
// from the back of the engine's queue (up to three attempts).
func TestGenerateWithoutClientTimeoutOutlivesTheClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "m", 150*time.Millisecond)

	// Without the option the client Timeout still applies, as before.
	_, err := c.Generate(context.Background(), "", "", "hola", "", 16, 0, 0)
	var uerr *url.Error
	if !errors.As(err, &uerr) || !uerr.Timeout() {
		t.Fatalf("an option-free call must still hit the client timeout, got %v", err)
	}

	// With it the request runs to its answer, and the client is left untouched.
	res, err := c.Generate(context.Background(), "", "", "hola", "", 16, 0, 0, WithoutClientTimeout())
	if err != nil || res.Content != "ok" {
		t.Fatalf("WithoutClientTimeout: res=%+v err=%v, want the answer that arrived after the client timeout", res, err)
	}
	if c.http.Timeout != 150*time.Millisecond {
		t.Fatalf("the shared client's Timeout was changed to %v", c.http.Timeout)
	}

	// The context still bounds it: the caller's deadline ends the request.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Generate(ctx, "", "", "hola", "", 16, 0, 0, WithoutClientTimeout()); err == nil || ctx.Err() == nil {
		t.Fatalf("the context deadline must still end the call, got err=%v ctx=%v", err, ctx.Err())
	}
}
