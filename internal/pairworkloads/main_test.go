package pairworkloads

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateRootPrefix names this package's throwaway state root.
const stateRootPrefix = "lo-pairworkloads-state-"

// suiteStateRootEnv is set by the suite process, once it has made its throwaway state root, to that root's path. A copy of this
// test binary that a test re-launches inherits it (every re-launch site passes the process environment on), and
// inheritedSuiteRoot reads it as "the suite that started me owns the root".
const suiteStateRootEnv = "LO_PAIRWORKLOADS_SUITE_STATE_ROOT"

// TestMain keeps the open-card register (orphans.go) off the machine: every
// enabled emitter in this package writes markers under the machine-wide state
// root, and a sweep there would close the operator's own orphaned cards into a
// test's httptest ingress. LOCAL_OFFLOAD_STATE_DIR is the override
// gpulease.ResolveStateRoot honours in production.
func TestMain(m *testing.M) {
	// A copy of this test binary that a test re-launched (a child that is killed, or one that ends by os.Exit from inside a test)
	// never reaches the removal below, so a root of its own was left in the OS temp dir by every launch. It shares the root of the
	// suite that launched it, through the environment it inherited, and makes none.
	if _, ok := inheritedSuiteRoot(os.Getenv); ok {
		os.Exit(m.Run())
	}
	os.Exit(runIsolated(m, os.MkdirTemp, os.Setenv, os.RemoveAll))
}

// suiteRunner is what TestMain runs: *testing.M, or a stub in the fail-closed test.
type suiteRunner interface{ Run() int }

// runIsolated points LOCAL_OFFLOAD_STATE_DIR at a throwaway directory, runs the suite, and removes the directory. It FAILS CLOSED:
// when the directory or either variable cannot be set up it returns 1 without running a single test, because a suite that fell back
// to the machine-wide state root would be exposed to the live data this net exists to protect (the 31.9 h ghost card in docs/systems/pair-workloads.md, "Tests must isolate their state root"). The
// three effects are parameters so TestRunIsolatedFailsClosed can make each one fail.
func runIsolated(m suiteRunner, mkdirTemp func(dir, pattern string) (string, error), setenv func(key, value string) error, removeAll func(string) error) int {
	dir, err := mkdirTemp("", stateRootPrefix)
	if err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not isolate the state root: " + err.Error() + "\n")
		return 1
	}
	defer func() { _ = removeAll(dir) }()
	if err := setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
		return 1
	}
	// After the state dir it names, so that a copy of this binary that inherits the pair finds them agreeing (inheritedSuiteRoot).
	if err := setenv(suiteStateRootEnv, filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not set " + suiteStateRootEnv + ": " + err.Error() + "\n")
		return 1
	}
	return m.Run()
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
	if !strings.HasPrefix(filepath.Base(root), stateRootPrefix) {
		return "", false
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", false
	}
	return root, true
}

// isolationStderr is where runIsolated reports a throwaway root it could not remove; os.Stderr, except in the test of that report.
var isolationStderr io.Writer = os.Stderr

// stubRunner records whether the suite was run, and answers code as the suite's own result.
type stubRunner struct {
	ran  bool
	code int
}

func (s *stubRunner) Run() int { s.ran = true; return s.code }

// TestRunIsolatedFailsClosed pins the fail-closed rule: a directory or variable that cannot be set up ends the suite with a non-zero
// code and no test run, and the directory a failed setup made is removed. A suite that fell back to the machine-wide state root
// would sweep the operator's own open-card markers into a test's ingress (the 31.9 h ghost card in docs/systems/pair-workloads.md).
func TestRunIsolatedFailsClosed(t *testing.T) {
	boom := errors.New("boom")
	made := func(string, string) (string, error) { return t.TempDir(), nil }
	failOn := func(key string) func(string, string) error {
		return func(k, _ string) error {
			if k == key {
				return boom
			}
			return nil
		}
	}
	cases := []struct {
		name        string
		mkdir       func(dir, pattern string) (string, error)
		setenv      func(key, value string) error
		wantCode    int
		wantRan     bool
		wantRemoved bool
	}{
		{"mkdir fails", func(string, string) (string, error) { return "", boom }, failOn(""), 1, false, false},
		{"the state dir cannot be set", made, failOn("LOCAL_OFFLOAD_STATE_DIR"), 1, false, true},
		{"the suite variable cannot be set", made, failOn(suiteStateRootEnv), 1, false, true},
		{"all three work", made, failOn(""), 0, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stub stubRunner
			removed := ""
			code := runIsolated(&stub, c.mkdir, c.setenv, func(p string) error { removed = p; return nil })
			if code != c.wantCode || stub.ran != c.wantRan {
				t.Fatalf("code = %d, ran = %v; want %d, %v", code, stub.ran, c.wantCode, c.wantRan)
			}
			if (removed != "") != c.wantRemoved {
				t.Fatalf("throwaway directory removed = %q; want removed=%v", removed, c.wantRemoved)
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
				for _, want := range []string{"pairworkloads tests:", filepath.Clean(dir), boom.Error()} {
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
