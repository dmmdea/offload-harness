package pipeline

// A copy of the test binary that this package's suite re-launches makes no folder in the OS temp dir (the test below).
// The repo-wide lint (tempdir_lint_test.go in the root package) requires this proof from every package that re-launches
// its test binary and fails on a new direct use of the OS temp dir in any test file.

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// The suite re-launches its own test binary as the browse sidecar stand-in (TestBrowseHelperProcess), and
// that copy ends with os.Exit from inside a test, so it never returns to a cleanup. A state root made by
// every copy's TestMain was therefore left in the OS temp dir once per launch (13 folders in one run of
// this package, counted with the temp dir pointed at an empty folder). This launches the stand-in the way
// production launches the sidecar (its argument, its allowlisted environment) with the temp dir pointed at
// a fresh folder, and requires the folder to be empty when the stand-in is gone.
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], browseHelperArg)
	// The environment production gives the sidecar is an allowlist (browseRunnerEnv), so nothing the suite set
	// in its own environment reaches the copy. The temp dir is pointed at the fresh folder on top of it; TMPDIR
	// is not on the allowlist, so on Unix the copy would otherwise write to the system temp dir and this
	// test could not see it.
	cmd.Env = append(browseRunnerEnv(os.Environ(), runtime.GOOS), "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp)
	// With no start line on stdin the stand-in exits 7 at once: a copy that ends by os.Exit from inside a
	// test, which is the shape that cannot clean up after itself. That exit status is expected here.
	var exit *exec.ExitError
	if out, err := cmd.CombinedOutput(); err != nil && !errors.As(err, &exit) {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a re-launched copy of the test binary left %d entries in its temp dir, want none: %v", len(names), names)
	}
}
