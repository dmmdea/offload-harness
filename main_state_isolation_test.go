package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
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
	// First, and outside runIsolated: when this process is the stand-in nvidia-smi a test installed
	// (internal/gpuprobe/smitest) it plays that part and exits without touching the state root.
	smitest.MaybeRun()
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
	// The offload home holds the ledger, the delegation corpus and every other config.Default() path (DefaultBase),
	// and config.Default() fixes them when it is called: a test that builds its config with it and sets Home
	// afterwards still wrote the operator's real ledger (2026-10-03: 1,204 agent_run fixture rows read as fleet
	// traffic). LOCAL_OFFLOAD_ORIGIN labels any row that still escapes.
	for _, kv := range [][2]string{{"LOCAL_OFFLOAD_HOME", filepath.Join(filepath.Clean(dir), "home")}, {"LOCAL_OFFLOAD_ORIGIN", "go-test"}} {
		if err := setenv(kv[0], kv[1]); err != nil {
			os.Stderr.WriteString("root tests: could not set " + kv[0] + ": " + err.Error() + "\n")
			return 1
		}
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

// TestMainIsolatesTheHomeAndTheLedger pins the second half of the safety net: the offload home, and with it the
// ledger config.Default() names, is inside the TestMain's throwaway directory, not the operator's.
func TestMainIsolatesTheHomeAndTheLedger(t *testing.T) {
	state := filepath.Clean(os.Getenv("LOCAL_OFFLOAD_STATE_DIR"))
	home := filepath.Clean(os.Getenv("LOCAL_OFFLOAD_HOME"))
	if os.Getenv("LOCAL_OFFLOAD_HOME") == "" || !strings.HasPrefix(home, state+string(filepath.Separator)) {
		t.Fatalf("LOCAL_OFFLOAD_HOME = %q: TestMain must point it inside its throwaway state root %q", home, state)
	}
	if got := filepath.Clean(config.Default().LedgerPath); !strings.HasPrefix(got, home+string(filepath.Separator)) {
		t.Fatalf("config.Default().LedgerPath = %q, want it under the isolated home %q", got, home)
	}
	if got := os.Getenv("LOCAL_OFFLOAD_ORIGIN"); got != "go-test" {
		t.Fatalf("LOCAL_OFFLOAD_ORIGIN = %q, want go-test so an escaped row names its writer", got)
	}
}
