package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func propsJSON(nctx int) string {
	return `{"default_generation_settings":{"n_ctx":` + itoa(nctx) + `,"params":{"seed":1}},"model_path":"x"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// llama-swap topology: /props at the root 404s, the per-model passthrough
// answers — the probe must find the passthrough.
func TestProbeServedWindowLlamaSwap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upstream/gemma-4-e4b/props" {
			w.Write([]byte(propsJSON(8192)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	n, ok := ProbeServedWindow(context.Background(), srv.URL, "gemma-4-e4b")
	if !ok || n != 8192 {
		t.Fatalf("probe = (%d,%v), want (8192,true)", n, ok)
	}
}

// Bare llama-server: /props at the root answers directly.
func TestProbeServedWindowBareServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			w.Write([]byte(propsJSON(4096)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	// A trailing /v1 (an OpenAI-style base) must be stripped before probing.
	n, ok := ProbeServedWindow(context.Background(), srv.URL+"/v1", "m")
	if !ok || n != 4096 {
		t.Fatalf("probe = (%d,%v), want (4096,true)", n, ok)
	}
}

// A generic OpenAI endpoint with no /props: the probe fails cleanly (callers
// fall back; a probe must never be able to break a run).
func TestProbeServedWindowUnanswerable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if n, ok := ProbeServedWindow(context.Background(), srv.URL, "m"); ok {
		t.Fatalf("expected probe failure, got %d", n)
	}
	if _, ok := ProbeServedWindow(context.Background(), "", "m"); ok {
		t.Fatal("empty base must fail the probe")
	}
}

// Malformed / zero-n_ctx answers are failures, not zero windows.
func TestProbeServedWindowBadPayload(t *testing.T) {
	for _, body := range []string{"not json", `{"default_generation_settings":{"n_ctx":0}}`, `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		if n, ok := ProbeServedWindow(context.Background(), srv.URL, "m"); ok {
			t.Errorf("payload %q: expected failure, got %d", body, n)
		}
		srv.Close()
	}
}

// ResolveContextTokens is the whole knob contract — pin every case. The
// assumed-16384/served-8192 mismatch killed real runs (2026-07-24); auto must
// take the probe, and an over-served explicit flag must warn.
func TestResolveContextTokens(t *testing.T) {
	cases := []struct {
		name          string
		flag, probed  int
		probeOK       bool
		want          int
		noteFragment  string // "" = no note required; otherwise must appear
		forbidWarning bool
	}{
		{"auto+probe", 0, 8192, true, 8192, "probed", false},
		{"auto+noprobe", 0, 0, false, FallbackContextTokens, "fallback", false},
		{"explicit-over-served", 16384, 8192, true, 16384, "exceed", false},
		{"explicit-under-served", 4096, 8192, true, 4096, "", true},
		{"explicit+noprobe", 16384, 0, false, 16384, "", true},
	}
	for _, c := range cases {
		got, note := ResolveContextTokens(c.flag, c.probed, c.probeOK)
		if got != c.want {
			t.Errorf("%s: tokens = %d, want %d", c.name, got, c.want)
		}
		if c.noteFragment != "" && !strings.Contains(strings.ToLower(note), c.noteFragment) {
			t.Errorf("%s: note %q must mention %q", c.name, note, c.noteFragment)
		}
		if c.forbidWarning && note != "" {
			t.Errorf("%s: unexpected note %q", c.name, note)
		}
	}
}
