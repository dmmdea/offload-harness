package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// SF-16: the agent behind --serve drives write and GitHub tools, so its routes sit behind
// the ingress guard with RequireJSONPost. Under enforce a cross-site text/plain POST (which
// needs no CORS preflight) never reaches the chat handler, and a DNS-rebinding Host is
// refused; warn (the default) still serves. The chat handler is never reached in these
// cases, so no agent loop is needed.
func TestServeRoutesSitBehindTheIngressGuard(t *testing.T) {
	do := func(mode, method, path, host, ctype string) int {
		t.Helper()
		h := guardedOpenAIHandler(openAIMux(nil, "m"), mode, nil)
		r := httptest.NewRequest(method, "http://placeholder"+path, strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
		if host != "" {
			r.Host = host
		}
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("enforce", http.MethodPost, "/v1/chat/completions", "127.0.0.1:18800", "text/plain"); c != http.StatusUnsupportedMediaType {
		t.Errorf("enforce, cross-site text/plain POST to the agent: %d, want 415", c)
	}
	if c := do("enforce", http.MethodGet, "/v1/models", "attacker.example:18800", ""); c != http.StatusMisdirectedRequest {
		t.Errorf("enforce, rebinding Host: %d, want 421", c)
	}
	if c := do("enforce", http.MethodGet, "/v1/models", "127.0.0.1:18800", ""); c != http.StatusOK {
		t.Errorf("enforce, a clean request: %d, want 200", c)
	}
	if c := do("warn", http.MethodGet, "/v1/models", "attacker.example:18800", ""); c != http.StatusOK {
		t.Errorf("warn refused: %d", c)
	}
}

// The server --serve actually runs is the guarded one (review: the test above builds the
// guarded handler itself, so a serveOpenAI that dropped the guard stayed green).
func TestTheRealServerIsGuarded(t *testing.T) {
	srv := newOpenAIServer("127.0.0.1:0", nil, "m", "enforce")
	for _, c := range []struct {
		method, host, ctype string
		want                int
	}{
		{http.MethodGet, "attacker.example:18800", "", http.StatusMisdirectedRequest},
		{http.MethodPost, "127.0.0.1:18800", "text/plain", http.StatusUnsupportedMediaType},
	} {
		path := "/v1/models"
		if c.method == http.MethodPost {
			path = "/v1/chat/completions"
		}
		r := httptest.NewRequest(c.method, "http://placeholder"+path, strings.NewReader("{}"))
		r.Host = c.host
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s Host=%s: %d, want %d", c.method, path, c.host, w.Code, c.want)
		}
	}
}

// --serve refuses to start on a config that failed to load: its fleet_ingress_guard would
// otherwise fall back to the default without a word (review finding).
func TestServeRefusesAConfigThatFailedToLoad(t *testing.T) {
	if err := serveConfigError(config.Source{Path: "x.json", LoadErr: errors.New("trailing comma")}); err == nil {
		t.Fatal("a config that failed to load was accepted for --serve")
	}
	if err := serveConfigError(config.Source{Path: "x.json"}); err != nil {
		t.Fatalf("a loaded config was refused: %v", err)
	}
}

// main starts --serve through runServe with the CONFIGURED mode (pinned at the source: main's
// --serve branch cannot be driven in-process; runServe and serveOpenAI are tested below).
func TestMainPassesTheConfiguredGuardModeToServe(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := "runServe(cfgSrc, func() error { return serveOpenAI(*listen, loop, plannerModel, cfg.FleetIngressGuardMode()) })"; !strings.Contains(string(src), want) {
		t.Fatalf("main must start --serve as %s", want)
	}
}

// runServe refuses a config that failed to load before anything starts, and starts otherwise.
func TestRunServeRefusesAFailedConfigBeforeStarting(t *testing.T) {
	started := false
	err := runServe(config.Source{Path: "x.json", LoadErr: errors.New("bad json")}, func() error { started = true; return nil })
	if err == nil || started {
		t.Fatalf("a failed config: err %v, started %v; want a refusal and no start", err, started)
	}
	if err := runServe(config.Source{Path: "x.json"}, func() error { started = true; return nil }); err != nil || !started {
		t.Fatalf("a loaded config: err %v, started %v; want a start", err, started)
	}
}

// The server serveOpenAI actually runs is the guarded one (SF-16 verify S3).
func TestServeOpenAIRunsTheGuardedServer(t *testing.T) {
	var got *http.Server
	old := listenAndServe
	listenAndServe = func(srv *http.Server) error { got = srv; return nil }
	defer func() { listenAndServe = old }()
	if err := serveOpenAI("127.0.0.1:0", nil, "m", "enforce"); err != nil || got == nil {
		t.Fatalf("serveOpenAI: %v, server %v", err, got)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "attacker.example:8080"
	rec := httptest.NewRecorder()
	got.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("serveOpenAI's server answered %d to a rebinding Host, want 421", rec.Code)
	}
}
