package llamaclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// bodyCapture serves cannedChatResp and hands back the RAW request bytes — raw
// on purpose: the omitted-key case below is pinned byte-for-byte, and a
// decode/re-encode round trip would hide exactly the difference it must catch.
func bodyCapture(t *testing.T, got *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*got = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, cannedChatResp)
	}))
}

// TestGenerateWithoutThinkingSendsChatTemplateKwargs pins the wire shape the
// live fix depends on: `chat_template_kwargs: {"enable_thinking": false}`
// alongside the grammar. Nothing else in the request may move.
func TestGenerateWithoutThinkingSendsChatTemplateKwargs(t *testing.T) {
	var raw string
	srv := bodyCapture(t, &raw)
	defer srv.Close()

	c := New(srv.URL, "", "seat-thinky", 5*time.Second)
	if _, err := c.Generate(context.Background(), "", "sys", "hi", `root ::= "{}"`, 64, 0, 0, WithoutThinking()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode request body (%v): %s", err, raw)
	}
	kw, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing or not an object: %s", raw)
	}
	et, ok := kw["enable_thinking"].(bool)
	if !ok || et {
		t.Fatalf("chat_template_kwargs.enable_thinking = %#v, want false: %s", kw["enable_thinking"], raw)
	}
	// The option must not disturb the rest of the request — a thinking flag
	// that also dropped the grammar would "pass" this test and break the seam.
	if g, _ := body["grammar"].(string); g != `root ::= "{}"` {
		t.Fatalf("grammar = %q, want it untouched by the option", g)
	}
}

// TestGenerateWithoutOptionOmitsChatTemplateKwargs is the compatibility pin:
// with no option the key must be ABSENT — not present-and-true — and the whole
// body must be byte-identical to what every existing call site has always sent.
// Pinned as an exact string rather than a field check because adding a request
// field is precisely the kind of change that silently alters every other call's
// payload.
func TestGenerateWithoutOptionOmitsChatTemplateKwargs(t *testing.T) {
	var raw string
	srv := bodyCapture(t, &raw)
	defer srv.Close()

	c := New(srv.URL, "", "seat-thinky", 5*time.Second)
	if _, err := c.Generate(context.Background(), "", "sys", "hi", `root ::= "{}"`, 64, 0, 0); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	const want = `{"model":"seat-thinky","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"temperature":0,"max_tokens":64,"grammar":"root ::= \"{}\"","cache_prompt":true,"stream":false}`
	if raw != want {
		t.Fatalf("request body drifted from the pre-option payload:\n got: %s\nwant: %s", raw, want)
	}
}
