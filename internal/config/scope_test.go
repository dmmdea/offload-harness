package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/callscope"
	"github.com/dmmdea/offload-harness/internal/config"
)

// TestCallerScopeKeepsARestrictedNodeOffEveryOtherCaller guards the 2026-11-09 incident: a business session's
// speech-to-text and image work was placed on a node that must only ever take its operator's personal work. The
// contract: a node listed in delegate_remote_scopes is removed from EVERY node list (delegate_remotes, the cascade
// lanes, the seat endpoints) for any caller it does not admit, a caller whose scope cannot be established is
// treated as not admitted, the media path's roster re-read cannot put the node back, and a call naming the node is
// refused with the reason.
func TestCallerScopeKeepsARestrictedNodeOffEveryOtherCaller(t *testing.T) {
	const a, b, c = "http://node-a:18811", "http://node-b:18811", "http://node-c:18811"
	base := config.Config{
		DelegateRemotes:      []string{a, b},
		CascadeRemoteLanes:   map[string]string{"small-model": b},
		SeatEndpoints:        map[string]string{"big-model": "http://node-b:11436"},
		DelegateRemoteScopes: map[string][]string{"http://NODE-B.tailnet.example:18811": {"personal"}},
	}
	for _, scope := range []string{callscope.Business, callscope.Unknown, ""} {
		cfg := base.ForScope(callscope.Scope{Name: scope})
		if !slices.Equal(cfg.DelegateRemotes, []string{a}) || len(cfg.CascadeRemoteLanes) != 0 || len(cfg.SeatEndpoints) != 0 {
			t.Fatalf("scope %q: restricted node still reachable: remotes=%v lanes=%v seats=%v", scope, cfg.DelegateRemotes, cfg.CascadeRemoteLanes, cfg.SeatEndpoints)
		}
		if len(cfg.ScopeExcluded) != 3 {
			t.Fatalf("scope %q: want 3 exclusions listed, got %+v", scope, cfg.ScopeExcluded)
		}
		if got := cfg.WithRoster([]string{a, b, c}).DelegateRemotes; !slices.Equal(got, []string{a, c}) {
			t.Fatalf("scope %q: roster re-read let the restricted node back: %v", scope, got)
		}
		if cfg.ScopeRefusal(b) == nil {
			t.Fatalf("scope %q: naming the restricted node was not refused", scope)
		}
	}
	personal := base.ForScope(callscope.Scope{Name: callscope.Personal})
	if !slices.Equal(personal.DelegateRemotes, []string{a, b}) || len(personal.CascadeRemoteLanes) != 1 || len(personal.SeatEndpoints) != 1 || len(personal.ScopeExcluded) != 0 {
		t.Fatalf("personal caller lost the node it is admitted to: %+v", personal)
	}

	// How the scope is established: the session's working directory, judged by the longest root; a session whose
	// file is missing is unknown and is NOT judged by the process's own directory.
	roots := map[string][]string{callscope.Personal: {"*:/Drive/Projects"}, callscope.Business: {"*:/Drive/Projects/Acme"}}
	files := map[string]string{"/fakehome/.claude/sessions/41.json": `{"sessionId":"s-1","cwd":"g:\\Drive\\Projects\\Acme\\shop"}`}
	in := func(env map[string]string, cwd string) callscope.Inputs {
		return callscope.Inputs{
			Getenv: func(k string) string { return env[k] }, PPID: 41, Home: "/fakehome", Cwd: cwd, Roots: roots,
			ReadFile: func(p string) ([]byte, error) {
				if v, ok := files[filepath.ToSlash(p)]; ok {
					return []byte(v), nil
				}
				return nil, os.ErrNotExist
			},
			Glob:  func(string) ([]string, error) { return nil, nil },
			Sleep: func(time.Duration) {},
		}
	}
	for _, tc := range []struct {
		env  map[string]string
		cwd  string
		want string
	}{
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "s-1"}, "D:/Drive/Projects", callscope.Business},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "s-2"}, "D:/Drive/Projects", callscope.Unknown},
		{map[string]string{}, "D:/Drive/Projects/notes", callscope.Personal},
		{map[string]string{}, "D:/elsewhere", callscope.Unknown},
		{map[string]string{callscope.EnvScope: "persnal"}, "D:/Drive/Projects", callscope.Unknown},
	} {
		if got := callscope.Resolve(in(tc.env, tc.cwd)).Name; got != tc.want {
			t.Fatalf("Resolve(env=%v, cwd=%s) = %s, want %s", tc.env, tc.cwd, got, tc.want)
		}
	}

	// A policy key that names no node of the config fences nothing; the load refuses it by name.
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"delegate_remotes":["http://node-a:18811"],"delegate_remote_scopes":{"http://node-z:18811":["personal"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(p); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a policy naming no configured node loaded without error: %v", err)
	}
}
