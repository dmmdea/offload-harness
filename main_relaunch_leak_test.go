package main

// The root package's tests re-launch their own test binary: the helper processes of the gpu verb tests (a wrapped command the parent
// kills, or one that ends by os.Exit from inside a test). A copy runs TestMain too, and TestMain made a throwaway state root of its
// own in the OS temp dir and removed it after m.Run, which a copy that is killed or exits from inside a test never reaches: every
// launch left a lo-root-state-* folder behind. A copy shares the root of the suite that launched it instead (main_state_isolation_test.go).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// relaunchLeakHelperEnv asks the helper below to act; without it the helper is an ordinary skipped test.
const relaunchLeakHelperEnv = "LO_RELAUNCH_LEAK_HELPER"

// relaunchLeakReportEnv names a file the helper writes the state dir it sees to, as one "state=<dir>" line; relaunchLeakSleepEnv asks it to
// wait there for its parent to kill it instead of ending by os.Exit.
const (
	relaunchLeakReportEnv = "LO_RELAUNCH_LEAK_REPORT"
	relaunchLeakSleepEnv  = "LO_RELAUNCH_LEAK_SLEEP"
)

// TestRelaunchLeakHelperProcess is the child of the relaunch-leak tests below. It ends the way the re-launched copies of this suite end:
// by os.Exit from inside the test, or killed by the parent while it waits, so that nothing after m.Run is reached.
func TestRelaunchLeakHelperProcess(t *testing.T) {
	if os.Getenv(relaunchLeakHelperEnv) != "1" {
		t.Skip("the child of the relaunch-leak tests")
	}
	if report := os.Getenv(relaunchLeakReportEnv); report != "" {
		_ = os.WriteFile(report, []byte("state="+os.Getenv("LOCAL_OFFLOAD_STATE_DIR")+"\n"), 0o644)
	}
	os.Stdout.WriteString("relaunch-leak helper: ending from inside the test\n")
	if os.Getenv(relaunchLeakSleepEnv) == "1" {
		time.Sleep(60 * time.Second) // the parent kills this process long before
	}
	os.Exit(0)
}

// A copy of the test binary that the suite re-launches, with the OS temp dir pointed at a fresh folder, must leave nothing in it.
// The copy is started the way the suite starts its own (os.Args[0], the inherited environment), and its temp dir is set on its own
// environment only, so the parent's is untouched.
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := winexec.Command(os.Args[0], "-test.run=^TestRelaunchLeakHelperProcess$", "-test.timeout=60s")
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

// relaunchedCopy starts a copy of this test binary the way the suite starts its own (os.Args[0], winexec, an environment built from the
// suite's), with the OS temp dir pointed at tmp on the copy's environment only, and runs it to its end. The copy ends by os.Exit from
// inside the test, or, with kill, waits until the parent has seen its report and kills it. It returns the state dir the copy reported.
func relaunchedCopy(t *testing.T, env []string, tmp string, kill bool) string {
	t.Helper()
	report := filepath.Join(t.TempDir(), "report")
	cmd := winexec.Command(os.Args[0], "-test.run=^TestRelaunchLeakHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(env, relaunchLeakHelperEnv+"=1", relaunchLeakReportEnv+"="+report, "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp)
	if kill {
		cmd.Env = append(cmd.Env, relaunchLeakSleepEnv+"=1")
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the copy: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if kill {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if b, err := os.ReadFile(report); err == nil && strings.HasSuffix(string(b), "\n") {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				<-done
				t.Fatalf("test premise: the copy never reported, so an empty folder proves nothing:\n%s", out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		<-done
	} else if err := <-done; err != nil {
		t.Fatalf("the copy: %v\n%s", err, out.String())
	}
	b, err := os.ReadFile(report)
	if err != nil || !strings.HasPrefix(string(b), "state=") {
		t.Fatalf("test premise: the copy never reported, so an empty folder proves nothing (%v):\n%s", err, out.String())
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "state="))
}

// namesIn lists the entries of dir.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// THE LEAK THAT REMAINED (F77b left it): the tests that start a real `gpu reserve` wrapper take LOCAL_OFFLOAD_STATE_DIR out of the
// copy's environment (treeEnv), or empty it in their own (isolateFromMachineState), so that the wrapper and its config agree on a
// scratch lease root. A copy whose environment lost the state dir fails inheritedSuiteRoot, makes a root of its own, and ends by os.Exit
// from inside a test or is killed by its parent, so the removal in runIsolated never runs: one empty lo-root-state-* folder per launch
// (13 from the gpu tests alone, 58 on the machine that found it). It must keep a root of its own (that is the net: no fall back to the
// machine-wide root) but keep it inside the suite's root, which the suite removes when it ends.
func TestARelaunchedCopyThatLostTheStateDirLeavesNothingInTheTempDir(t *testing.T) {
	suiteRoot := filepath.Clean(os.Getenv("LOCAL_OFFLOAD_STATE_DIR"))
	if !strings.HasPrefix(filepath.Base(suiteRoot), stateIsolationPrefix) {
		t.Fatalf("test premise: LOCAL_OFFLOAD_STATE_DIR = %q is not this suite's throwaway root", suiteRoot)
	}
	for _, env := range []struct {
		name string
		env  func() []string
	}{
		{"the state dir is taken out of the environment", func() []string { return treeEnv() }},
		{"the state dir is emptied", func() []string { return append(os.Environ(), "LOCAL_OFFLOAD_STATE_DIR=") }},
	} {
		for _, end := range []struct {
			name string
			kill bool
		}{{"it ends by os.Exit from inside a test", false}, {"its parent kills it", true}} {
			t.Run(env.name+", "+end.name, func(t *testing.T) {
				tmp := t.TempDir()
				state := relaunchedCopy(t, env.env(), tmp, end.kill)
				if names := namesIn(t, tmp); len(names) != 0 {
					t.Errorf("a re-launched copy whose environment lost the state dir left %d entries in its temp dir, want none: %v", len(names), names)
				}
				if !strings.HasPrefix(filepath.Base(state), stateIsolationPrefix) {
					t.Errorf("the copy's state dir is %q, want a throwaway root of its own (%s*), never the machine-wide one", state, stateIsolationPrefix)
				}
				if filepath.Clean(state) == suiteRoot {
					t.Errorf("the copy's state dir is the suite's root %q; a copy that lost the state dir must not share it, its own pinned root is the isolation", state)
				}
				if !strings.HasPrefix(filepath.Clean(state), suiteRoot+string(filepath.Separator)) {
					t.Errorf("the copy's state dir %q is not inside the suite's root %q, so the suite's removal does not take it with it", state, suiteRoot)
				}
			})
		}
	}
}

// When no live throwaway root of a suite is named, a copy has no suite to own its root and makes one of its own in the OS temp dir, as
// the first process does: the net fails closed, so nothing is nested under a folder that is not a suite's throwaway root.
func TestACopyWithNoLiveSuiteRootKeepsARootOfItsOwnInTheTempDir(t *testing.T) {
	base := t.TempDir()
	elsewhere := filepath.Join(base, "somewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []struct{ name, value string }{
		{"the suite's variable is not set", ""},
		{"it names a folder that is not a throwaway root", elsewhere},
		{"it names a throwaway root that is gone", filepath.Join(base, stateIsolationPrefix+"gone")},
	} {
		t.Run(owner.name, func(t *testing.T) {
			tmp := t.TempDir()
			env := append(treeEnv(), suiteStateRootEnv+"="+owner.value)
			state := relaunchedCopy(t, env, tmp, false)
			names := namesIn(t, tmp)
			if len(names) != 1 || !strings.HasPrefix(names[0], stateIsolationPrefix) {
				t.Fatalf("a copy with no live suite root should keep one %s* root of its own in its temp dir, found %v", stateIsolationPrefix, names)
			}
			if !strings.HasPrefix(filepath.Base(state), stateIsolationPrefix) {
				t.Errorf("the copy's state dir is %q, want a throwaway root of its own", state)
			}
			if inner := namesIn(t, elsewhere); len(inner) != 0 {
				t.Errorf("the copy put %v inside %s, which is not a suite's throwaway root", inner, elsewhere)
			}
		})
	}
}
