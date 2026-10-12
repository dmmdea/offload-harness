package fleetnode

// This package's tests leave nothing in the OS temp dir. Two things keep it so:
//
//   - a copy of the test binary that the suite re-launches makes nothing there (the first test below), and
//   - a test that wants scratch space asks the testing package for it, or is listed here with the
//     reason its folder is gone on every path (the second).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	cmd := winexec.Command(os.Args[0], "-test.run=^TestSweepHelperExitNow$")
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

// rawTempUse matches a call that makes something in the OS temp dir directly. A test should take its
// scratch from t.TempDir (removed when the test ends), or point the code under test at that folder.
var rawTempUse = regexp.MustCompile(`os\.(?:MkdirTemp|CreateTemp)\(\s*""|os\.TempDir\(\)|ioutil\.Temp(?:Dir|File)\(`)

// rawTempAllowed lists the test files that may make something in the OS temp dir directly, how many
// calls each may hold, and why every one of them is removed on every path. A new call fails the test below
// until it is listed with its reason; a removed call fails it until the count is brought down.
var rawTempAllowed = map[string]struct {
	calls  int
	reason string
}{
	"compose_project_test.go": {1, "projectPayload packs into a folder it removes with a deferred RemoveAll before it returns, panic included"},
}

func TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files found: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			calls += len(rawTempUse.FindAllString(line, -1))
		}
		allowed := rawTempAllowed[filepath.Base(f)]
		seen[filepath.Base(f)] = true
		if calls != allowed.calls {
			why := ""
			if allowed.reason != "" {
				why = " (listed: " + allowed.reason + ")"
			}
			t.Errorf("%s makes %d direct use(s) of the OS temp dir, the list allows %d%s: use t.TempDir, or list the file with the reason its folder is removed on every path", f, calls, allowed.calls, why)
		}
	}
	for name := range rawTempAllowed {
		if !seen[name] {
			t.Errorf("%s is listed in rawTempAllowed but is not a test file here: remove the entry", name)
		}
	}
}
