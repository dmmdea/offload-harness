package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// stateIsolationPrefix names the root package's throwaway state root, so
// TestMainIsolatesTheStateRoot can tell the TestMain's directory from one a
// caller exported.
const stateIsolationPrefix = "lo-root-state-"

// suiteStateRootEnv is set by the suite process, once it has made its throwaway state root, to that root's path. A copy of this
// test binary that the suite re-launches inherits it (every re-launch site passes the process environment on), and
// inheritedSuiteRoot reads it as "the suite that started me owns the root".
const suiteStateRootEnv = "LO_ROOT_SUITE_STATE_ROOT"

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
	// A copy of this test binary that a test re-launched (the helper processes of the gpu verb tests: a wrapped command the parent
	// kills, or one that ends by os.Exit from inside a test) never reaches the removal in runIsolated, so a root of its own was left in
	// the OS temp dir by every launch. It shares the root of the suite that launched it, through the environment it inherited, and
	// runs on the same known host.
	if _, ok := inheritedSuiteRoot(os.Getenv); ok {
		os.Exit(runOnTestHost(m))
	}
	os.Exit(runIsolated(m, os.MkdirTemp, os.Setenv, os.RemoveAll))
}

// suiteOrigin is the LOCAL_OFFLOAD_ORIGIN every process of the suite carries, so that a ledger row that escapes names its writer.
const suiteOrigin = "go-test"

// suiteHome is the offload home of the suite whose throwaway state root is root: a folder inside it.
func suiteHome(root string) string { return filepath.Join(filepath.Clean(root), "home") }

// inheritedSuiteRoot reports the state root of the suite that launched this process, when this process is a copy of the test binary
// that the suite re-launched. The environment must say so five ways: the suite's variable names a root, that root is the one
// LOCAL_OFFLOAD_STATE_DIR points at, it is one of this package's throwaway roots and still on disk, LOCAL_OFFLOAD_HOME is the home
// inside it and LOCAL_OFFLOAD_ORIGIN is go-test. The last two are what runIsolated pins for the suite, and a copy skips runIsolated, so
// a copy launched from a test that had cleared or moved them (every launcher passes the environment as that test left it) would
// otherwise build config.Default() on the operator's real home and write its real ledger (2026-10-03: 1,204 fixture rows). Anything
// less is not a copy to trust, and TestMain then makes a root of its own, which pins all of them, as it always did: this net exists to
// keep the suite off the machine-wide state root and the real ledger, so it fails closed.
func inheritedSuiteRoot(getenv func(string) string) (string, bool) {
	root := getenv(suiteStateRootEnv)
	if filepath.Clean(getenv("LOCAL_OFFLOAD_STATE_DIR")) != filepath.Clean(root) {
		return "", false
	}
	if !strings.HasPrefix(filepath.Base(root), stateIsolationPrefix) {
		return "", false
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", false
	}
	if home := getenv("LOCAL_OFFLOAD_HOME"); home == "" || filepath.Clean(home) != suiteHome(root) {
		return "", false
	}
	if getenv("LOCAL_OFFLOAD_ORIGIN") != suiteOrigin {
		return "", false
	}
	return root, true
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
	// After the state dir it names, so that a copy of this binary that inherits the pair finds them agreeing (inheritedSuiteRoot).
	if err := setenv(suiteStateRootEnv, filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("root tests: could not set " + suiteStateRootEnv + ": " + err.Error() + "\n")
		return 1
	}
	// The offload home holds the ledger, the delegation corpus and every other config.Default() path (DefaultBase),
	// and config.Default() fixes them when it is called: a test that builds its config with it and sets Home
	// afterwards still wrote the operator's real ledger (2026-10-03: 1,204 agent_run fixture rows read as fleet
	// traffic). LOCAL_OFFLOAD_ORIGIN labels any row that still escapes.
	for _, kv := range [][2]string{{"LOCAL_OFFLOAD_HOME", suiteHome(dir)}, {"LOCAL_OFFLOAD_ORIGIN", suiteOrigin}} {
		if err := setenv(kv[0], kv[1]); err != nil {
			os.Stderr.WriteString("root tests: could not set " + kv[0] + ": " + err.Error() + "\n")
			return 1
		}
	}
	return runOnTestHost(m)
}

// runOnTestHost runs the suite against a known host. A reservation that declares host RAM reads the host before it is granted
// (internal/gpulease/hostram.go), so the suite runs against a known host and not against whatever the machine running it is doing;
// a re-launched copy runs on the same one.
func runOnTestHost(m suiteRunner) int {
	defer gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return roomyTestHost, true })()
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
