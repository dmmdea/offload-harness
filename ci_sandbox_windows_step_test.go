package main

// D-98, phase 1. The Windows cage's environment proof runs on a Windows runner, on every change.
//
// internal/sandbox hands a caged child an explicit environment block instead of the parent's. Until
// 0.117.7 the Windows side passed a nil block, which means "inherit", so every `run` child received
// the delegator's credentials. TestWindowsChildDoesNotInheritParentSecrets and
// TestWindowsChildGetsTheAllowlistedEnvironment prove the fix, and they can only run on Windows: on every
// other OS the same names are t.Skip stubs. The `build` job runs `go test ./...` on Linux, where both
// are SKIPs, and the one job on a Windows runner (installer-windows) never ran `go test`. So the proof
// ran only on a developer's own Windows machine, and a regression could merge with every check green.
//
// The step is a trap with three jaws, and each one is checked:
//
//   - `go test` exits 0 when the -run pattern matches nothing ("no tests to run") and when the tests
//     skip, so the step must demand the `--- PASS:` line of each test, not trust the exit code;
//   - a pipe hides the exit code of its left side unless pipefail is on, so a failing test whose PASS
//     lines already scrolled by would pass;
//   - the PASS line is a line of its own: the same words quoted inside other output are not a pass.
//
// Phase 2 (a later change) widens the step to the whole ./internal/sandbox/... package once the tests
// that cover the exit-status reading are merged; this test then changes with it.

import (
	"fmt"
	"strings"
	"testing"
)

// windowsCageProofs are the two tests the step must see pass, by name.
var windowsCageProofs = []string{
	"TestWindowsChildDoesNotInheritParentSecrets",
	"TestWindowsChildGetsTheAllowlistedEnvironment",
}

// findWindowsCageStep finds the step that runs go test on ./internal/sandbox, in a job on a Windows runner.
func findWindowsCageStep(t *testing.T, w ciWorkflow) (jobName string, job ciJob, index int) {
	t.Helper()
	var elsewhere []string
	for _, name := range w.jobNames() {
		j := w.Jobs[name]
		for i, st := range j.Steps {
			if !strings.Contains(st.Run, "go test") || !strings.Contains(st.Run, "internal/sandbox") {
				continue
			}
			if j.runner() == "windows-latest" {
				return name, j, i
			}
			elsewhere = append(elsewhere, fmt.Sprintf("%s (runs-on %v)", name, j.RunsOn))
		}
	}
	if len(elsewhere) > 0 {
		t.Fatalf("ci.yml runs the sandbox tests only in %s, where both Windows proofs are SKIP stubs; the step must be in "+
			"a job whose runs-on is exactly windows-latest (the standard runner: a larger one is always billed)",
			strings.Join(elsewhere, ", "))
	}
	t.Fatal("no step of any job in ci.yml runs `go test` on ./internal/sandbox: the Windows cage's environment proof " +
		"runs only on a developer's own Windows machine (D-98)")
	return "", ciJob{}, -1
}

// goTestOutput is what `go test -v` prints for the cases below.
const (
	cageBothPass = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"--- PASS: TestWindowsChildDoesNotInheritParentSecrets (0.31s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"--- PASS: TestWindowsChildGetsTheAllowlistedEnvironment (0.28s)\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.912s\n"

	cageFirstSkipped = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"--- SKIP: TestWindowsChildDoesNotInheritParentSecrets (0.00s)\n" +
		"    sandbox_env_other_test.go:16: windows-only: the env-block proof needs CreateProcessAsUser\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"--- PASS: TestWindowsChildGetsTheAllowlistedEnvironment (0.28s)\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.401s\n"

	cageSecondSkipped = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"--- PASS: TestWindowsChildDoesNotInheritParentSecrets (0.31s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"--- SKIP: TestWindowsChildGetsTheAllowlistedEnvironment (0.00s)\n" +
		"    sandbox_env_other_test.go:20: windows-only: the env-block proof needs CreateProcessAsUser\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.402s\n"

	// What go test prints, with exit status 0, when -run matches nothing.
	cageNothingRan = "testing: warning: no tests to run\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.021s [no tests to run]\n"

	// The proofs' own words inside log lines: a PASS line is a line of its own, not text inside another one.
	cageQuotedOnly = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"    sandbox_env_windows_test.go:90: want --- PASS: TestWindowsChildDoesNotInheritParentSecrets (0.31s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"    sandbox_env_windows_test.go:91: want --- PASS: TestWindowsChildGetsTheAllowlistedEnvironment (0.28s)\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.5s\n"

	// A subtest's indented PASS line is not the test passing either.
	cageSubtestsOnly = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"    --- PASS: TestWindowsChildDoesNotInheritParentSecrets/subtest (0.31s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"    --- PASS: TestWindowsChildGetsTheAllowlistedEnvironment/subtest (0.28s)\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.5s\n"

	// -run is a prefix pattern: a test added later whose name starts with a proof's name must not stand in for it.
	cageSiblingStandsIn = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"--- SKIP: TestWindowsChildDoesNotInheritParentSecrets (0.00s)\n" +
		"=== RUN   TestWindowsChildDoesNotInheritParentSecretsEither\n" +
		"--- PASS: TestWindowsChildDoesNotInheritParentSecretsEither (0.02s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"--- PASS: TestWindowsChildGetsTheAllowlistedEnvironment (0.28s)\n" +
		"PASS\n" +
		"ok  \texample.invalid/internal/sandbox\t0.4s\n"

	cageOneFailed = "=== RUN   TestWindowsChildDoesNotInheritParentSecrets\n" +
		"--- PASS: TestWindowsChildDoesNotInheritParentSecrets (0.31s)\n" +
		"=== RUN   TestWindowsChildGetsTheAllowlistedEnvironment\n" +
		"--- PASS: TestWindowsChildGetsTheAllowlistedEnvironment (0.28s)\n" +
		"=== RUN   TestWindowsChildSomethingElse\n" +
		"--- FAIL: TestWindowsChildSomethingElse (0.10s)\n" +
		"FAIL\n" +
		"FAIL\texample.invalid/internal/sandbox\t0.7s\n"
)

func TestCIRunsTheWindowsCageProofOnWindows(t *testing.T) {
	jobName, job, idx := findWindowsCageStep(t, loadCIWorkflow(t))
	step := job.Steps[idx]

	// What only the yaml can show.
	for _, p := range ciAlwaysGatesProblems(jobName, job, idx) {
		t.Error(p)
	}
	if step.Shell != "bash" {
		t.Errorf("step %q of job %s has shell %q: it needs `shell: bash`, which the runner starts with pipefail on "+
			"(the Windows default is PowerShell, where `go test | tee` and grep behave differently)", step.label(), jobName, step.Shell)
	}
	setup := -1
	for i := 0; i < idx; i++ {
		prev := job.Steps[i]
		if strings.HasPrefix(prev.Uses, "actions/setup-go@") {
			setup = i
		}
		if prev.Shell == "powershell" || prev.Shell == "pwsh" {
			t.Errorf("step %q of job %s comes after the PowerShell step %q: the proof goes before the installer self-tests, "+
				"so an unrelated failure there cannot hide it", step.label(), jobName, prev.label())
		}
	}
	if setup < 0 {
		t.Errorf("step %q of job %s has no actions/setup-go step before it", step.label(), jobName)
	}

	// runStep runs the step with a go that prints goTestOutput and exits goTestRC for `go test`, twice: as the
	// runner starts a `shell: bash` step (-e and pipefail on) and under a bare bash. The script must hold on its
	// own, because a custom `shell:` template or a change of the runner's defaults drops those options silently,
	// so every expectation below is checked for both.
	type stepRun struct {
		how    string
		sb     *ciScriptSandbox
		output string
		rc     int
	}
	runStep := func(t *testing.T, goTestOutput, goTestRC string) (runner, bare stepRun) {
		start := func(how string, plain bool) stepRun {
			sb := newCIScriptSandbox(t)
			sb.standIn("go", cageGoStandInBody)
			canned := sb.file("go-test-output.txt", goTestOutput)
			env := []string{"FAKE_GO_TEST_STDOUT=" + canned, "FAKE_GO_TEST_RC=" + goTestRC}
			run := sb.run
			if plain {
				run = sb.runPlain
			}
			out, rc := run(step.Run, env...)
			return stepRun{how, sb, out, rc}
		}
		return start("as the runner starts it", false), start("under a bare bash", true)
	}

	t.Run("it runs the two proofs and nothing is cached", func(t *testing.T) {
		runner, bare := runStep(t, cageBothPass, "0")
		for _, r := range []stepRun{runner, bare} {
			if r.rc != 0 {
				t.Errorf("%s: the step exited %d although both proofs passed:\n%s", r.how, r.rc, r.output)
			}
		}
		sb := runner.sb
		var tests []ciCall
		for _, c := range sb.calls("go") {
			if len(c.Args) > 0 && c.Args[0] == "test" {
				tests = append(tests, c)
			}
		}
		if len(tests) != 1 {
			t.Fatalf("the step ran %d go test commands, want 1: %v", len(tests), sb.calls("go"))
		}
		args := tests[0].Args
		for _, want := range [][]string{{"-count=1"}, {"-v"}, {"-run", "^TestWindowsChild"}} {
			if !tests[0].hasArgs(want...) {
				t.Errorf("go test ran with %v, missing %v: -count=1 stops a cached result from replaying the PASS lines, "+
					"-v is what prints them, and the anchored -run is the two proofs", args, want)
			}
		}
		timeout := false
		for _, a := range args {
			timeout = timeout || a == "-timeout" || strings.HasPrefix(a, "-timeout=")
		}
		if !timeout {
			t.Errorf("go test ran with %v: set -timeout, so a hung child process ends the step in minutes, not at the default ten", args)
		}
		if last := args[len(args)-1]; last != "./internal/sandbox/" && last != "./internal/sandbox" {
			t.Errorf("go test ran on %q, want ./internal/sandbox/ (phase 1 runs the package directory only)", last)
		}
	})

	t.Run("it demands each PASS line", func(t *testing.T) {
		for _, c := range []struct {
			name, output, rc string
		}{
			{"the secrets proof is skipped", cageFirstSkipped, "0"},
			{"the allowlist proof is skipped", cageSecondSkipped, "0"},
			{"the pattern matches no test and go test exits 0", cageNothingRan, "0"},
			{"the PASS words appear only inside other output", cageQuotedOnly, "0"},
			{"only subtests print an indented PASS line", cageSubtestsOnly, "0"},
			{"a longer-named sibling passes while the proof is skipped", cageSiblingStandsIn, "0"},
			{"go test prints nothing", "", "0"},
		} {
			runner, bare := runStep(t, c.output, c.rc)
			for _, r := range []stepRun{runner, bare} {
				if r.rc == 0 {
					t.Errorf("%s, %s: the step exited 0 and the proof would count as run:\n%s", c.name, r.how, r.output)
				}
			}
		}
	})

	t.Run("it fails when go test fails", func(t *testing.T) {
		// Both PASS lines are on screen and go test still exits 1: only pipefail makes the pipe carry that.
		runner, bare := runStep(t, cageOneFailed, "1")
		for _, r := range []stepRun{runner, bare} {
			if r.rc == 0 {
				t.Errorf("%s: go test exited 1 after printing both PASS lines and the step exited 0:\n%s", r.how, r.output)
			}
		}
	})
}

// cageGoStandInBody plays back a canned `go test -v` run: the output a file holds and the exit status the
// test names.
const cageGoStandInBody = `record go "$@"
if [ "$1" = test ]; then
  cat "$FAKE_GO_TEST_STDOUT"
  exit "${FAKE_GO_TEST_RC:-0}"
fi
`
