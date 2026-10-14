package mcpserver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// stateIsolationPrefix names this package's throwaway state root, so
// TestMainIsolatesTheStateRoot can tell the TestMain's directory from one a
// caller exported.
const stateIsolationPrefix = "lo-mcpserver-state-"

// TestMain keeps this package's tests off the machine-wide state root
// (%ProgramData%\local-offload on Windows), which the GPU lease, the delegate
// intent ledger (delegate-intent.jsonl), seat-inflight and the PAIR open-card
// register share.
//
// WHY. About fifty tests here drive a handler or a status read through a config.Default() with no StateDir: they appended to the operator's REAL delegate-intent.jsonl and created the real gpu/ lease and activity directories (found by a per-test sweep with %ProgramData% redirected, 2026-10-03). A test that forgets its own isolation must land in a
// throwaway root, not the real one.
//
// LOCAL_OFFLOAD_STATE_DIR is the override gpulease.ResolveStateRoot honours in
// production, and an explicit cfg.StateDir still wins over it, so the tests that
// set their own root are unchanged, and a test that needs the variable itself
// sets it with t.Setenv. Same shape as internal/pipeline/leaseisolation_test.go
// and internal/pairworkloads/main_test.go.
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
		os.Stderr.WriteString("mcpserver tests: could not isolate the state root: " + err.Error() + "\n")
		return 1
	}
	defer removeStateRoot(dir, removeAll)
	if err := setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("mcpserver tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
		return 1
	}
	// The offload home holds the ledger, the delegation corpus and every other config.Default() path (DefaultBase),
	// and config.Default() fixes them when it is called: a test that builds its config with it and sets Home
	// afterwards still wrote the operator's real ledger (2026-10-03: 1,204 agent_run fixture rows read as fleet
	// traffic). LOCAL_OFFLOAD_ORIGIN labels any row that still escapes.
	for _, kv := range [][2]string{{"LOCAL_OFFLOAD_HOME", filepath.Join(filepath.Clean(dir), "home")}, {"LOCAL_OFFLOAD_ORIGIN", "go-test"}} {
		if err := setenv(kv[0], kv[1]); err != nil {
			os.Stderr.WriteString("mcpserver tests: could not set " + kv[0] + ": " + err.Error() + "\n")
			return 1
		}
	}
	// offload_status reports the host's memory (gpu_lease.host_memory): the suite runs against a known
	// host, so its answers and its golden do not depend on what the machine running it is doing.
	defer gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return knownTestHost, true })()
	return m.Run()
}

// knownTestHost is the host the suite reports: 128 GiB physical, 40 GiB committed, so the verdict is OK.
var knownTestHost = gpuprobe.HostMemory{PhysicalGiB: 128, AvailableGiB: 64, CommitUsedGiB: 40, CommitLimitGiB: 192}

// isolationStderr is where runIsolated reports a throwaway root it could not remove; os.Stderr, except in the test of that report.
var isolationStderr io.Writer = os.Stderr

// removeRetryDelay is how long runIsolated waits before it tries a removal that failed once more. On Windows a process that has just
// ended (or a scanner) can still hold a file of the folder, or the folder itself as its working directory, for a moment.
const removeRetryDelay = 250 * time.Millisecond

// removeStateRoot removes the suite's throwaway state root. A removal that fails is tried once more after removeRetryDelay, and a root
// that is still there after that is named on isolationStderr with the error, so that a leak is seen where it is made and not found later as
// a folder in the OS temp dir. It does not change the suite's result: the tests ran, and a leftover folder is not a failed test.
func removeStateRoot(dir string, removeAll func(string) error) {
	err := removeAll(dir)
	if err == nil {
		return
	}
	time.Sleep(removeRetryDelay)
	if err = removeAll(dir); err != nil {
		fmt.Fprintf(isolationStderr, "mcpserver tests: could not remove the throwaway state root %s: %v (a process may still hold it open; remove it by hand)\n", filepath.Clean(dir), err)
	}
}

// stubRunner records whether the suite was run, and answers code as the suite's own result.
type stubRunner struct {
	ran  bool
	code int
}

func (s *stubRunner) Run() int { s.ran = true; return s.code }

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

// TestRunIsolatedRetriesARemovalOnceAndReportsWhatStaysBehind pins the removal of the throwaway root: a removal that fails (on
// Windows a process that has not yet let go of a file or of the folder as its working directory) is tried once more, and a root that
// is still there after the second attempt is named on stderr with the error, so that a leak is seen, never dropped. A root that stays
// behind also ends a passing suite with exit code 1, because `go test ./...` hides the stderr of a package that passed and the report alone
// would not reach anyone; a suite that failed keeps its own code, and a root that went on the retry reports nothing.
func TestRunIsolatedRetriesARemovalOnceAndReportsWhatStaysBehind(t *testing.T) {
	boom := errors.New("sharing violation")
	for _, c := range []struct {
		name       string
		failures   int
		suiteCode  int
		wantCalls  int
		wantReport bool
		wantCode   int
	}{
		{"it works at once", 0, 0, 1, false, 0},
		{"the retry works", 1, 0, 2, false, 0},
		{"it fails both times, the suite passed", 99, 0, 2, true, 1},
		{"it fails both times, the suite failed", 99, 3, 2, true, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var report bytes.Buffer
			was := isolationStderr
			isolationStderr = &report
			t.Cleanup(func() { isolationStderr = was })
			var tried []string
			removeAll := func(p string) error {
				tried = append(tried, p)
				if len(tried) <= c.failures {
					return boom
				}
				return nil
			}
			stub := stubRunner{code: c.suiteCode}
			code := runIsolated(&stub, func(string, string) (string, error) { return dir, nil }, func(string, string) error { return nil }, removeAll)
			if code != c.wantCode || !stub.ran {
				t.Fatalf("code = %d, ran = %v; want %d, true (a root that stays behind fails a passing suite and leaves a failing one's code alone)", code, stub.ran, c.wantCode)
			}
			if len(tried) != c.wantCalls {
				t.Fatalf("the removal was tried %d times, want %d (once, and once more only when it failed)", len(tried), c.wantCalls)
			}
			for _, p := range tried {
				if p != dir {
					t.Fatalf("the removal was asked for %q, want the throwaway root %q", p, dir)
				}
			}
			got := report.String()
			if c.wantReport {
				for _, want := range []string{"mcpserver tests:", filepath.Clean(dir), boom.Error()} {
					if !strings.Contains(got, want) {
						t.Fatalf("the report %q should contain %q", got, want)
					}
				}
			} else if got != "" {
				t.Fatalf("nothing is left behind, so nothing is reported, got %q", got)
			}
		})
	}
}

// TestMainIsolatesTheStateRoot pins the safety net: the default state-root
// resolution is the TestMain's throwaway directory, not the machine's.
func TestMainIsolatesTheStateRoot(t *testing.T) {
	env := os.Getenv("LOCAL_OFFLOAD_STATE_DIR")
	if !strings.HasPrefix(filepath.Base(env), stateIsolationPrefix) {
		t.Fatalf("LOCAL_OFFLOAD_STATE_DIR = %q: this package's TestMain must point it at its own throwaway directory", env)
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
