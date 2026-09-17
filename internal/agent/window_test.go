package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// ProbeUpstreamWindow must NOT fall back to the bare root: the root /props
// answers for whatever model is currently loaded, so a multi-model caller
// (the cascade's TO-3 repack) would budget one tier against another tier's
// window. Reverting the restriction makes this test fail (round-2 review
// 2026-08-14: the restriction previously had zero regression coverage).
func TestProbeUpstreamWindowNeverFallsBackToRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" { // bare-root only — the WRONG model's window
			w.Write([]byte(propsJSON(4096)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if n, ok := ProbeUpstreamWindow(context.Background(), srv.URL, "m"); ok {
		t.Fatalf("upstream-only probe fell back to the bare root and returned %d — the cascade would budget with the wrong model's window", n)
	}
	// The per-model route still answers.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upstream/m/props" {
			w.Write([]byte(propsJSON(8192)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv2.Close()
	if n, ok := ProbeUpstreamWindow(context.Background(), srv2.URL, "m"); !ok || n != 8192 {
		t.Fatalf("upstream probe = (%d,%v), want (8192,true)", n, ok)
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
		got, note := ResolveContextTokens(c.flag, c.probed, 0, c.probeOK)
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

// vLLM behind llama-swap (the Qube agent-pool seat): the per-model /props
// passthrough answers 404 — vLLM has no /props — and the served window is
// only reported as max_model_len on the backend's own /v1/models. Before
// 0.113.14 this seat budgeted FallbackContextTokens (8,192) against a
// 163,840-token window on every run.
func TestProbeServedWindowVLLMBehindLlamaSwap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upstream/agent-pool/v1/models":
			// vLLM lists the canonical name AND every --served-model-name alias,
			// all with the same window.
			w.Write([]byte(`{"object":"list","data":[{"id":"qwen3.8-27b-vllm","object":"model","max_model_len":163840},{"id":"agent-pool","object":"model","max_model_len":163840}]}`))
			return
		case "/v1/models":
			// llama-swap's OWN roster — no max_model_len; must never be read as one.
			w.Write([]byte(`{"object":"list","data":[{"id":"agent-pool"},{"id":"gemma-4-e4b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	n, ok := ProbeServedWindow(context.Background(), srv.URL, "agent-pool")
	if !ok || n != 163840 {
		t.Fatalf("probe = (%d,%v), want (163840,true) from max_model_len", n, ok)
	}
	// The upstream-only probe (the cascade's per-tier repack) gets the same answer.
	n, ok = ProbeUpstreamWindow(context.Background(), srv.URL, "agent-pool")
	if !ok || n != 163840 {
		t.Fatalf("upstream-only probe = (%d,%v), want (163840,true)", n, ok)
	}
}

// TestFetchMaxModelLenRules pins the extraction: the entry named like the
// probed model wins; otherwise the list must AGREE on one positive value; a
// list that disagrees, or has no max_model_len at all, answers nothing.
func TestFetchMaxModelLenRules(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		model string
		want  int
		ok    bool
	}{
		{"named entry wins", `{"data":[{"id":"a","max_model_len":100},{"id":"m","max_model_len":300}]}`, "m", 300, true},
		{"aliases agree", `{"data":[{"id":"a","max_model_len":300},{"id":"b","max_model_len":300}]}`, "m", 300, true},
		{"aliases disagree", `{"data":[{"id":"a","max_model_len":100},{"id":"b","max_model_len":300}]}`, "m", 0, false},
		{"llama-server list (no field)", `{"data":[{"id":"m","meta":{"n_ctx_train":8192}}]}`, "m", 0, false},
		{"empty", `{"data":[]}`, "m", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(c.body)) }))
			defer srv.Close()
			n, ok := fetchMaxModelLen(context.Background(), srv.Client(), srv.URL+"/upstream/m/v1/models", c.model)
			if n != c.want || ok != c.ok {
				t.Fatalf("fetchMaxModelLen = (%d,%v), want (%d,%v)", n, ok, c.want, c.ok)
			}
		})
	}
}

// TestResolveContextTokensConfiguredFallback pins the cold-seat fix. When the
// served-window probe fails — a cold seat still loading answers /props and
// /v1/models with 400 — the loop must budget against the seat's CONFIGURED
// window (config agent_ctx_tokens) rather than FallbackContextTokens. Before the
// fix the same agent_run measured ctx_window 8,192 cold and 114,688 warm minutes
// apart: a silent 14x loss that exhausted five compactions and truncated every
// file read. The probed path and the operator flag are unchanged.
func TestResolveContextTokensConfiguredFallback(t *testing.T) {
	cases := []struct {
		name       string
		flag       int
		probed     int
		configured int
		probeOK    bool
		want       int
		noteHas    string
	}{
		{"probe fails, configured window known: use it", 0, 0, 114688, false, 114688, "configured window"},
		{"probe fails, nothing configured: conservative fallback", 0, 0, 0, false, FallbackContextTokens, "conservative fallback"},
		{"probe answers: probed wins over configured, and the disagreement is named", 0, 114688, 163840, true, 114688, "disagrees"},
		{"probe answers and config agrees: no disagreement named", 0, 114688, 114688, true, 114688, "probed from the serving endpoint)"},
		{"operator flag set: flag wins even when the probe fails", 32768, 0, 114688, false, 32768, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, note := ResolveContextTokens(c.flag, c.probed, c.configured, c.probeOK)
			if got != c.want {
				t.Fatalf("ResolveContextTokens(flag=%d, probed=%d, configured=%d, probeOK=%v) = %d, want %d",
					c.flag, c.probed, c.configured, c.probeOK, got, c.want)
			}
			if c.noteHas != "" && !strings.Contains(note, c.noteHas) {
				t.Fatalf("note = %q, want it to contain %q", note, c.noteHas)
			}
		})
	}
}

// TestProbeServedWindowWaitsOutAColdStart (2026-09-16): llama-swap holds a
// request for a model that is not loaded until its health check passes, so a
// cold seat answers the per-model probe only after the load. Measured on the
// Qube agent-pool seat: `ready` at 222 s, while the old 60 s per-request timeout
// gave up at 60 s and 120 s and the run budgeted 8,192 against 114,688. Here the
// load is 600 ms and the bare-root timeout 100 ms: a probe still bounded by the
// per-request timeout gives up twice before the seat is up.
func TestProbeServedWindowWaitsOutAColdStart(t *testing.T) {
	oldReq := probeRequestTimeout
	probeRequestTimeout = 100 * time.Millisecond
	defer func() { probeRequestTimeout = oldReq }()
	loaded := make(chan struct{})
	var startLoad sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/upstream/agent-pool/") {
			http.NotFound(w, r)
			return
		}
		// The first passthrough request starts the load; every request waits for it.
		startLoad.Do(func() {
			go func() { time.Sleep(600 * time.Millisecond); close(loaded) }()
		})
		select {
		case <-loaded:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/upstream/agent-pool/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"agent-pool","max_model_len":114688}]}`))
			return
		}
		http.NotFound(w, r) // a vLLM seat has no /props
	}))
	defer srv.Close()
	n, ok := ProbeServedWindow(context.Background(), srv.URL, "agent-pool")
	if !ok || n != 114688 {
		t.Fatalf("probe = (%d,%v), want (114688,true): the probe must wait out the cold start, not fall back", n, ok)
	}
}

// TestProbeServedWindowColdStartWaitIsBounded: the wait for a cold start is
// bounded by coldStartWait even when the caller's context carries no deadline,
// so a seat that never comes up cannot hang the probe.
func TestProbeServedWindowColdStartWaitIsBounded(t *testing.T) {
	oldWait := coldStartWait
	coldStartWait = 200 * time.Millisecond
	defer func() { coldStartWait = oldWait }()
	never := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upstream/") {
			select {
			case <-never:
			case <-r.Context().Done():
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	defer close(never)
	type answer struct {
		n  int
		ok bool
	}
	got := make(chan answer, 1)
	go func() {
		n, ok := ProbeServedWindow(context.Background(), srv.URL, "agent-pool")
		got <- answer{n, ok}
	}()
	select {
	case a := <-got:
		if a.ok {
			t.Fatalf("probe = (%d,true) against a seat that never loaded, want (0,false)", a.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cold-start wait is unbounded: the probe was still waiting after 5 s with coldStartWait 200 ms")
	}
}
