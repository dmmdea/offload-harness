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

// A copy of the test binary shares the suite's state root only when the environment says so five ways (inheritedSuiteRoot): the
// suite's variable names a root, LOCAL_OFFLOAD_STATE_DIR points at the same one, it is one of the root package's throwaway roots that
// is still on disk, LOCAL_OFFLOAD_HOME is that root's "home" and LOCAL_OFFLOAD_ORIGIN is go-test. The last two are what runIsolated
// pins for the suite (the ledger guard of 2026-10-03: 1,204 fixture rows written to the real ledger); a copy skips runIsolated, so a
// copy whose environment lost them must not run on trust and gets a root of its own, which pins them. Any other environment is no
// copy to trust either, because the net exists to keep the suite off the machine-wide state root.
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
	type environ struct{ owner, state, home, origin string }
	env := func(e environ) func(string) string {
		return func(key string) string {
			switch key {
			case suiteStateRootEnv:
				return e.owner
			case "LOCAL_OFFLOAD_STATE_DIR":
				return e.state
			case "LOCAL_OFFLOAD_HOME":
				return e.home
			case "LOCAL_OFFLOAD_ORIGIN":
				return e.origin
			}
			return ""
		}
	}
	// What the suite process has in its environment once runIsolated has run: the home inside the root, the origin go-test.
	suite := func(root string) environ {
		return environ{owner: root, state: root, home: filepath.Join(root, "home"), origin: "go-test"}
	}
	with := func(e environ, edit func(*environ)) environ { edit(&e); return e }
	for _, c := range []struct {
		name string
		e    environ
		want bool
	}{
		{"a copy: the suite's variable, the state dir, the home and the origin all agree on one live throwaway root", suite(root), true},
		{"the suite's variable is not set", with(suite(root), func(e *environ) { e.owner = "" }), false},
		{"the state dir is not set", with(suite(root), func(e *environ) { e.state = "" }), false},
		{"the state dir points at another folder", with(suite(root), func(e *environ) { e.state = elsewhere }), false},
		{"the root is not one of this package's throwaway roots", suite(elsewhere), false},
		{"the root is gone", suite(gone), false},
		{"the root is a file", suite(file), false},
		{"the home is not set", with(suite(root), func(e *environ) { e.home = "" }), false},
		{"the home is the operator's own, outside the root", with(suite(root), func(e *environ) { e.home = elsewhere }), false},
		{"the home is the root itself, not its home folder", with(suite(root), func(e *environ) { e.home = root }), false},
		{"the origin is not set", with(suite(root), func(e *environ) { e.origin = "" }), false},
		{"the origin names another writer", with(suite(root), func(e *environ) { e.origin = "another-writer" }), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := inheritedSuiteRoot(env(c.e))
			if ok != c.want || (ok && got != c.e.owner) {
				t.Fatalf("inheritedSuiteRoot(%+v) = %q, %v; want trusted=%v", c.e, got, ok, c.want)
			}
		})
	}
}
