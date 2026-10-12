package fleetnode

// A copy of the test binary that this package's suite re-launches makes no folder in the OS temp dir (the test below).
// The repo-wide lint (tempdir_lint_test.go in the root package) requires this proof from every package that re-launches
// its test binary and fails on a new direct use of the OS temp dir in any test file.

import (
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The suite re-launches its own test binary: the stand-in ffmpeg of stt_e2e_test.go and the dead-owner
// helper of sweep_ownership_test.go. Every copy runs the package's init functions before it reaches a test,
// and a copy that exits from a test, or from a later init, never returns to a cleanup. An init that made a
// folder in the OS temp dir therefore left one for the suite and one for each launch (13 folders in one
// run of this package, counted with the temp dir pointed at an empty folder). This launches the helper
// with the temp dir pointed at a fresh folder and requires the folder to be empty when the helper is gone.
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := winexec.Command(os.Args[0], "-test.run=^TestSweepHelperExitNow$") // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — the test binary itself, a fixed argument list
	cmd.Env = append(os.Environ(), "FLEETNODE_SWEEP_HELPER=1", "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
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
