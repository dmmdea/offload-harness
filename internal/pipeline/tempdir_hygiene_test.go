package pipeline

// This package's tests leave nothing in the OS temp dir. Two things keep it so:
//
//   - a copy of the test binary that the suite re-launches makes no folder there (the first test below), and
//   - a test that wants scratch space asks the testing package for it, or is listed here with the
//     reason its folder is gone on every path (the second).

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The suite re-launches its own test binary as the browse sidecar stand-in (TestBrowseHelperProcess), and
// that copy ends with os.Exit from inside a test, so it never returns to a cleanup. A state root made by
// every copy's TestMain was therefore left in the OS temp dir once per launch (13 folders in one run of
// this package, counted with the temp dir pointed at an empty folder). This launches the stand-in the way
// production launches the sidecar (its argument, its allowlisted environment) with the temp dir pointed at
// a fresh folder, and requires the folder to be empty when the stand-in is gone.
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := winexec.Command(os.Args[0], browseHelperArg)
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

// rawTempUse matches a call that makes something in the OS temp dir directly. A test should take its
// scratch from t.TempDir (removed when the test ends), or point the code under test at that folder.
var rawTempUse = regexp.MustCompile(`os\.(?:MkdirTemp|CreateTemp)\(\s*""|os\.TempDir\(\)|ioutil\.Temp(?:Dir|File)\(`)

// rawTempAllowed lists the test files that may reach for the OS temp dir directly, how many calls each may
// hold, and why every one of them leaves no folder behind. A new call fails the test below until it is
// listed with its reason; a removed call fails it until the count is brought down.
var rawTempAllowed = map[string]struct {
	calls  int
	reason string
}{
	"leaseisolation_test.go":  {1, "the suite's state root: made once by the suite process and removed after its run; the re-launched sidecar copy makes none"},
	"pipeline_vision_test.go": {1, "joins a lock path that must stay absent: nothing in the vision path creates it, so no folder is made"},
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
			t.Errorf("%s makes %d direct use(s) of the OS temp dir, the list allows %d%s: use t.TempDir, or list the file with the reason it leaves no folder behind", f, calls, allowed.calls, why)
		}
	}
	for name := range rawTempAllowed {
		if !seen[name] {
			t.Errorf("%s is listed in rawTempAllowed but is not a test file here: remove the entry", name)
		}
	}
}
