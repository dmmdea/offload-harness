package main

// The root package's tests re-launch their own test binary: the helper processes of the gpu verb tests (a wrapped command the parent
// kills, or one that ends by os.Exit from inside a test). A copy runs TestMain too, and TestMain made a throwaway state root of its
// own in the OS temp dir and removed it after m.Run, which a copy that is killed or exits from inside a test never reaches: every
// launch left a lo-root-state-* folder behind. A copy shares the root of the suite that launched it instead (main_state_isolation_test.go).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// relaunchLeakHelperEnv asks the helper below to act; without it the helper is an ordinary skipped test.
const relaunchLeakHelperEnv = "LO_RELAUNCH_LEAK_HELPER"

// TestRelaunchLeakHelperProcess is the child of TestARelaunchedTestBinaryLeavesNothingInTheTempDir. It ends the way the re-launched
// copies of this suite end: by os.Exit from inside the test, so that nothing after m.Run is reached.
func TestRelaunchLeakHelperProcess(t *testing.T) {
	if os.Getenv(relaunchLeakHelperEnv) != "1" {
		t.Skip("the child of TestARelaunchedTestBinaryLeavesNothingInTheTempDir")
	}
	os.Stdout.WriteString("relaunch-leak helper: ending from inside the test\n")
	os.Exit(0)
}

// A copy of the test binary that the suite re-launches, with the OS temp dir pointed at a fresh folder, must leave nothing in it.
// The copy is started the way the suite starts its own (os.Args[0], the inherited environment), and its temp dir is set on its own
// environment only, so the parent's is untouched.
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRelaunchLeakHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), relaunchLeakHelperEnv+"=1", "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ending from inside the test") {
		t.Fatalf("test premise: the helper never ran, so an empty folder proves nothing:\n%s", out)
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

// A copy of the test binary shares the suite's state root only when the environment says so three ways (inheritedSuiteRoot): the
// suite's variable names a root, LOCAL_OFFLOAD_STATE_DIR points at the same one, and it is one of the root package's throwaway roots that is
// still on disk. Any other environment is no copy to trust and gets a root of its own, because the net exists to keep the suite off
// the machine-wide state root.
func TestACopyTrustsTheSuitesStateRootOnlyWhenTheEnvironmentAgreesWithIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, stateIsolationPrefix+"1234")
	elsewhere := filepath.Join(base, "somewhere")
	file := filepath.Join(base, stateIsolationPrefix+"afile")
	for _, d := range []string{root, elsewhere} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(base, stateIsolationPrefix+"gone")
	env := func(owner, state string) func(string) string {
		return func(key string) string {
			switch key {
			case suiteStateRootEnv:
				return owner
			case "LOCAL_OFFLOAD_STATE_DIR":
				return state
			}
			return ""
		}
	}
	for _, c := range []struct {
		name         string
		owner, state string
		want         bool
	}{
		{"a copy: the suite's variable and the state dir name one live throwaway root", root, root, true},
		{"the suite's variable is not set", "", root, false},
		{"the state dir is not set", root, "", false},
		{"the state dir points at another folder", root, elsewhere, false},
		{"the root is not one of this package's throwaway roots", elsewhere, elsewhere, false},
		{"the root is gone", gone, gone, false},
		{"the root is a file", file, file, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := inheritedSuiteRoot(env(c.owner, c.state))
			if ok != c.want || (ok && got != c.owner) {
				t.Fatalf("inheritedSuiteRoot(owner %q, state %q) = %q, %v; want trusted=%v", c.owner, c.state, got, ok, c.want)
			}
		})
	}
}
