package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// stateIsolationPrefix names the root package's throwaway state root, so
// TestMainIsolatesTheStateRoot can tell the TestMain's directory from one a
// caller exported.
const stateIsolationPrefix = "lo-root-state-"

// TestMain keeps the root package's tests off the machine-wide state root
// (%ProgramData%\local-offload on Windows), the root seat-inflight, the GPU
// lease, the delegate intent ledger and the PAIR open-card register all share.
//
// WHY. TestLeaseCardLifecycle built an enabled PAIR emitter with no StateDir;
// the register resolved to the machine-wide pair-open directory, and the
// emitter's first sweep closed the operator's REAL orphan markers into the
// test's httptest ingress and deleted them, so the real cards stayed "Running"
// (a 31.9 h ghost card, and the recorded `frames = 4` flake). A test that
// forgets its own isolation must land in a throwaway root, not the real one.
//
// LOCAL_OFFLOAD_STATE_DIR is the override gpulease.ResolveStateRoot honours in
// production, and an explicit cfg.StateDir still wins over it, so the tests that
// set their own root (leaseFixture and the gpu_* tests) are unchanged; none of
// this package's tests depends on the default resolution. Same shape as
// internal/pipeline/leaseisolation_test.go and internal/pairworkloads/main_test.go.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m, os.MkdirTemp, os.Setenv, os.RemoveAll))
}

// suiteRunner is what TestMain runs: *testing.M, or a stub in the fail-closed test.
type suiteRunner interface{ Run() int }

// runIsolated points LOCAL_OFFLOAD_STATE_DIR at a throwaway directory, runs the
// suite, and removes the directory. It FAILS CLOSED: when the directory or the
// variable cannot be set up it returns 1 without running a single test, because
// a suite that fell back to the machine-wide state root would be exposed to the
// live data this net exists to protect. The three effects are parameters so
// TestRunIsolatedFailsClosed can make each one fail.
func runIsolated(m suiteRunner, mkdirTemp func(dir, pattern string) (string, error), setenv func(key, value string) error, removeAll func(string) error) int {
	dir, err := mkdirTemp("", stateIsolationPrefix)
	if err != nil {
		os.Stderr.WriteString("root tests: could not isolate the state root: " + err.Error() + "\n")
		return 1
	}
	defer func() { _ = removeAll(dir) }()
	if err := setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("root tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
		return 1
	}
	return m.Run()
}

// stubRunner records whether the suite was run.
type stubRunner struct{ ran bool }

func (s *stubRunner) Run() int { s.ran = true; return 0 }

// TestRunIsolatedFailsClosed pins the fail-closed rule: a directory or variable
// that cannot be set up ends the suite with a non-zero code and no test run.
func TestRunIsolatedFailsClosed(t *testing.T) {
	boom := errors.New("boom")
	ok := func(dir, pattern string) (string, error) { return t.TempDir(), nil }
	cases := []struct {
		name     string
		mkdir    func(dir, pattern string) (string, error)
		setenv   func(key, value string) error
		wantCode int
		wantRan  bool
	}{
		{"mkdir fails", func(string, string) (string, error) { return "", boom }, func(string, string) error { return nil }, 1, false},
		{"setenv fails", ok, func(string, string) error { return boom }, 1, false},
		{"both work", ok, func(string, string) error { return nil }, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stub stubRunner
			removed := ""
			code := runIsolated(&stub, c.mkdir, c.setenv, func(p string) error { removed = p; return nil })
			if code != c.wantCode || stub.ran != c.wantRan {
				t.Fatalf("code = %d, ran = %v; want %d, %v", code, stub.ran, c.wantCode, c.wantRan)
			}
			if c.wantRan && removed == "" {
				t.Fatal("the throwaway directory must be removed after the run")
			}
		})
	}
}

// TestMainIsolatesTheStateRoot pins the safety net: the default state-root
// resolution is the TestMain's throwaway directory, not the machine's.
func TestMainIsolatesTheStateRoot(t *testing.T) {
	env := os.Getenv("LOCAL_OFFLOAD_STATE_DIR")
	if !strings.HasPrefix(filepath.Base(env), stateIsolationPrefix) {
		t.Fatalf("LOCAL_OFFLOAD_STATE_DIR = %q: the root package's TestMain must point it at its own throwaway directory", env)
	}
	got, err := gpulease.ResolveStateRoot("")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("default state root = %q, want the isolated %q", got, want)
	}
}
