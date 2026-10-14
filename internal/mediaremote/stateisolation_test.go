package mediaremote

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
)

// stateIsolationPrefix names this package's throwaway state root, so TestMainIsolatesTheStateRoot can tell the TestMain's
// directory from one a caller exported.
const stateIsolationPrefix = "lo-mediaremote-state-"

// suiteStateRootEnv is set by the suite process, once it has made its throwaway state root, to that root's path. A copy of this
// test binary that a test re-launches inherits it (every re-launch site passes the process environment on), and
// inheritedSuiteRoot reads it as "the suite that started me owns the root".
const suiteStateRootEnv = "LO_MEDIAREMOTE_SUITE_STATE_ROOT"

// TestMain keeps this package's tests off the machine-wide state root (%ProgramData%\local-offload on Windows,
// /var/lib/local-offload elsewhere).
//
// WHY. The ticket queue, its node claims and the graph path's claims live under the same state root the GPU lease uses, and
// a config that sets no state_dir resolves that root's platform default (storeFor). A test that builds a config without a
// StateDir, and most of this package's fixtures that exist to show a queue do set one, would otherwise create tickets and
// claim files under the operator's REAL root, where a running harness reads them as its own. It lands in a throwaway root
// instead.
//
// LOCAL_OFFLOAD_STATE_DIR is the override gpulease.ResolveStateRoot honours in production, and an explicit cfg.StateDir still
// wins over it, so the tests that set their own root are unchanged. Same shape as internal/mcpserver/stateisolation_test.go.
func TestMain(m *testing.M) {
	// A copy of this test binary that a test re-launched (the holder of a queue lock, which the parent kills) never reaches the
	// removal in runIsolated, so a root of its own was left in the OS temp dir by every launch. It shares the root of the suite that
	// launched it, through the environment it inherited, and makes none.
	if _, ok := inheritedSuiteRoot(os.Getenv); ok {
		os.Exit(m.Run())
	}
	os.Exit(runIsolated(m, os.MkdirTemp, os.Setenv, os.RemoveAll))
}

// inheritedSuiteRoot reports the state root of the suite that launched this process, when this process is a copy of the test binary
// that the suite re-launched. The environment must say so three ways: the suite's variable names a root, that root is the one
// LOCAL_OFFLOAD_STATE_DIR points at, and it is one of this package's throwaway roots and still on disk. Anything less is not a copy
// to trust, and TestMain then makes a root of its own, as it always did: this net exists to keep the suite off the machine-wide
// state root, so it fails closed.
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
	return root, true
}

// suiteRunner is what TestMain runs: *testing.M, or a stub in the fail-closed test.
type suiteRunner interface{ Run() int }

// runIsolated points LOCAL_OFFLOAD_STATE_DIR at a throwaway directory, runs the suite, and removes the directory. It FAILS
// CLOSED: when the directory or the variable cannot be set up it returns 1 without running a single test, because a suite
// that fell back to the machine-wide state root would be exposed to the live data this net exists to protect. The three
// effects are parameters so TestRunIsolatedFailsClosed can make each one fail. A root it cannot remove (removeStateRoot) turns a passing
// suite's code 0 into 1.
func runIsolated(m suiteRunner, mkdirTemp func(dir, pattern string) (string, error), setenv func(key, value string) error, removeAll func(string) error) (code int) {
	dir, err := mkdirTemp("", stateIsolationPrefix)
	if err != nil {
		os.Stderr.WriteString("mediaremote tests: could not isolate the state root: " + err.Error() + "\n")
		return 1
	}
	// A root that stays behind fails a passing suite (exit 1) and leaves a failing suite's own code alone: the tests ran, but the isolation net did
	// not clean up after them, and the line removeStateRoot writes is hidden by `go test ./...` for a package that passed.
	defer func() {
		if removeStateRoot(dir, removeAll) != nil && code == 0 {
			code = 1
		}
	}()
	if err := setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("mediaremote tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
		return 1
	}
	// After the state dir it names, so that a copy of this binary that inherits the pair finds them agreeing (inheritedSuiteRoot).
	if err := setenv(suiteStateRootEnv, filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("mediaremote tests: could not set " + suiteStateRootEnv + ": " + err.Error() + "\n")
		return 1
	}
	return m.Run()
}

// isolationStderr is where runIsolated reports a throwaway root it could not remove; os.Stderr, except in the test of that report.
var isolationStderr io.Writer = os.Stderr

// removeRetryDelay is how long runIsolated waits before it tries a removal that failed once more. On Windows a process that has just
// ended (or a scanner) can still hold a file of the folder, or the folder itself as its working directory, for a moment.
const removeRetryDelay = 250 * time.Millisecond

// removeStateRoot removes the suite's throwaway state root. A removal that fails is tried once more after removeRetryDelay, and a root
// that is still there after that is named on isolationStderr with the error, so that a leak is seen where it is made and not found later as
// a folder in the OS temp dir, and the error is answered so that runIsolated can fail the package: `go test ./...` shows the stderr of a
// package that passed only with -v, so the line alone reaches no one.
func removeStateRoot(dir string, removeAll func(string) error) error {
	err := removeAll(dir)
	if err == nil {
		return nil
	}
	time.Sleep(removeRetryDelay)
	if err = removeAll(dir); err != nil {
		fmt.Fprintf(isolationStderr, "mediaremote tests: could not remove the throwaway state root %s: %v (a process may still hold it open; remove it by hand); the package fails so that this is not missed\n", filepath.Clean(dir), err)
	}
	return err
}

// stubRunner records whether the suite was run, and answers code as the suite's own result.
type stubRunner struct {
	ran  bool
	code int
}

func (s *stubRunner) Run() int { s.ran = true; return s.code }

// TestRunIsolatedFailsClosed pins the fail-closed rule: a directory or variable that cannot be set up ends the suite with a
// non-zero code and no test run.
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
				for _, want := range []string{"mediaremote tests:", filepath.Clean(dir), boom.Error()} {
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

// TestMainIsolatesTheStateRoot pins the safety net: the default state root this package's tests would resolve is the
// TestMain's throwaway directory, never the machine's.
func TestMainIsolatesTheStateRoot(t *testing.T) {
	env := os.Getenv("LOCAL_OFFLOAD_STATE_DIR")
	if env == "" || !strings.HasPrefix(filepath.Base(env), stateIsolationPrefix) {
		t.Fatalf("LOCAL_OFFLOAD_STATE_DIR = %q: this package's TestMain must point it at its own throwaway directory", env)
	}
}
