package pairworkloads

import (
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
	dir, err := os.MkdirTemp("", stateRootPrefix)
	if err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not isolate the state root: " + err.Error() + "\n")
		os.Exit(m.Run())
	}
	if err := os.Setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
	}
	// After the state dir it names, so that a copy of this binary that inherits the pair finds them agreeing (inheritedSuiteRoot).
	if err := os.Setenv(suiteStateRootEnv, filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not set " + suiteStateRootEnv + ": " + err.Error() + "\n")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
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
