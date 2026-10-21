package main

// H-26. The jobs that build and test the root module prove, in their own log, that the go command runs the
// compiler go.mod names.
//
// go.mod's `toolchain go1.27.3` line only works through a switch inside the go command: with GOTOOLCHAIN at its
// default (auto) a go command older than the line fetches the release and runs under it. That switch can stop
// working without a word: GOTOOLCHAIN=local (exported by setup-go from v6, or set by a runner image) makes the
// go command build with the installed compiler and ignore the line, and nothing prints. A job that then builds,
// vets and tests on go1.26.x is green and judges a compiler that does not ship. TestCISetupGoLeavesTheToolchainSwitchAlone
// reads the yaml for the known causes; it cannot see what a runner exports. So the jobs also ask the go command
// itself, once, before anything else uses it: `go env GOVERSION` must be exactly the release the toolchain line
// names. Exactly, not "at least": the govulncheck job scans under that release, and a build that judged a
// different one would leave the two jobs disagreeing about which compiler ships.
//
// The tests find the jobs by what they are (a job that sets up Go and compiles the root module, and does not pin
// the toolchain itself), check the step's place and its shell in the yaml, and run its script under bash with a
// stand-in for the go command, as the runner starts a step and under a bare bash.

import (
	"regexp"
	"strings"
	"testing"
)

// toolchainProofRootJobs are the jobs that must carry the proof today. The discovery below finds the jobs by
// what they do; this list keeps the discovery from going blind (a rule that matched nothing would pass).
var toolchainProofRootJobs = []string{"build", "installer-windows"}

// goCommandRe is a step that runs a go command: `go build`, `go test -v`, `go env`.
var goCommandRe = regexp.MustCompile(`(^|[\s;&|(])go (build|vet|test|run|install|list|env|mod|generate)\b`)

// rootToolchainJobs are the jobs that set up Go and compile the root module and rely on go.mod's toolchain
// line for the compiler: a setup-go step, no working-directory default (the nested printed CLIs have no
// toolchain line), and no step that pins GOTOOLCHAIN itself (the govulncheck job does, and its own test holds it).
func rootToolchainJobs(w ciWorkflow) []string {
	var names []string
	for _, name := range w.jobNames() {
		job := w.Jobs[name]
		if job.Defaults.Run.WorkingDirectory != "" {
			continue
		}
		setsUp, pins := false, false
		for _, st := range job.Steps {
			setsUp = setsUp || strings.HasPrefix(st.Uses, "actions/setup-go@")
			pins = pins || (strings.Contains(st.Run, "GITHUB_ENV") && strings.Contains(st.Run, "GOTOOLCHAIN"))
		}
		if setsUp && !pins {
			names = append(names, name)
		}
	}
	return names
}

// toolchainProofStep is the index of the job's step that asks the go command for its version, -1 when there is
// none, and the number of such steps.
func toolchainProofStep(job ciJob) (index, count int) {
	index = -1
	for i, st := range job.Steps {
		if strings.Contains(st.Run, "go env GOVERSION") {
			if index < 0 {
				index = i
			}
			count++
		}
	}
	return index, count
}

// toolchainProofGoStandIn answers `go env GOVERSION` with FAKE_GOVERSION (plus a carriage return when
// FAKE_GO_CRLF is set, as a native Windows go.exe could print), or fails with FAKE_GO_ENV_RC.
const toolchainProofGoStandIn = `record go "$@"
case "$1" in
  env)
    if [ "${FAKE_GO_ENV_RC:-0}" != 0 ]; then exit "$FAKE_GO_ENV_RC"; fi
    if [ -n "${FAKE_GO_CRLF-}" ]; then printf '%s\r\n' "${FAKE_GOVERSION-}"; else echo "${FAKE_GOVERSION-}"; fi
    ;;
esac
`

func TestCIJobsProveTheCompilerGoModNames(t *testing.T) {
	w := loadCIWorkflow(t)
	found := rootToolchainJobs(w)
	for _, want := range toolchainProofRootJobs {
		if !hasString(found, want) {
			t.Fatalf("the jobs that set up Go and compile the root module are %v, want %s among them: the rule that finds them went blind", found, want)
		}
	}

	for _, name := range found {
		job := w.Jobs[name]
		idx, n := toolchainProofStep(job)
		if n != 1 {
			t.Errorf("job %s has %d steps that run `go env GOVERSION`, want 1: after setup-go and before anything else uses the go "+
				"command, it must fail the job unless the go command runs the release go.mod's toolchain line names, because "+
				"GOTOOLCHAIN=local makes the go command ignore that line without a message (H-26)", name, n)
			continue
		}
		step := job.Steps[idx]

		// What only the yaml can show.
		for _, p := range ciAlwaysGatesProblems(name, job, idx) {
			t.Error(p)
		}
		if step.Shell != "bash" {
			t.Errorf("job %s: step %q has shell %q: it needs `shell: bash` (the Windows default is PowerShell, which reads none of it)",
				name, step.label(), step.Shell)
		}
		setup := -1
		for i := 0; i < idx; i++ {
			if strings.HasPrefix(job.Steps[i].Uses, "actions/setup-go@") {
				setup = i
			}
		}
		if setup < 0 {
			t.Errorf("job %s: step %q has no actions/setup-go step before it", name, step.label())
		}
		for i := 0; i < idx; i++ {
			if goCommandRe.MatchString(job.Steps[i].Run) {
				t.Errorf("job %s: step %q runs a go command before the proof: the first go command in the job is the one that "+
					"switches toolchains, and a step ahead of the proof would run on whatever compiler it finds", name, job.Steps[i].label())
			}
		}

		t.Run(name+" runs the step's script", func(t *testing.T) {
			// A version nobody ships: if the step read it from go.mod the match holds; a number typed into the workflow
			// would not match it.
			const made = "go1.99.9"
			goMod := func(toolchain string) string {
				if toolchain == "" {
					return "module example.invalid/m\n\ngo 1.26.7\n"
				}
				return "module example.invalid/m\n\ngo 1.26.7\n\ntoolchain " + toolchain + "\n"
			}
			type env []string
			cases := []struct {
				name       string
				goMod      string
				env        env
				wantOK     bool
				asks       bool     // the step must put the question to the go command (a go.mod without a usable line is refused first)
				wantInText []string // in the output of a run that fails
			}{
				{"the go command runs the release go.mod names", goMod(made), env{"FAKE_GOVERSION=" + made}, true, true, nil},
				{"a CRLF go.mod", strings.ReplaceAll(goMod(made), "\n", "\r\n"), env{"FAKE_GOVERSION=" + made}, true, true, nil},
				{"a go command that prints a carriage return", goMod(made), env{"FAKE_GOVERSION=" + made, "FAKE_GO_CRLF=1"}, true, true, nil},
				{"an older compiler: the line was ignored", goMod(made), env{"FAKE_GOVERSION=go1.26.9"}, false, true, []string{made, "go1.26.9"}},
				{"a newer compiler than the line", goMod(made), env{"FAKE_GOVERSION=go1.99.10"}, false, true, []string{made, "go1.99.10"}},
				{"a prefix of the release", goMod(made), env{"FAKE_GOVERSION=go1.99"}, false, true, []string{made, "go1.99"}},
				{"a release that begins with the line's", goMod(made), env{"FAKE_GOVERSION=" + made + "0"}, false, true, []string{made, made + "0"}},
				{"a go command that prints nothing", goMod(made), env{"FAKE_GOVERSION="}, false, true, []string{made}},
				{"a go command that fails", goMod(made), env{"FAKE_GO_ENV_RC=1"}, false, true, []string{made}},
				{"go.mod without a toolchain line", goMod(""), env{"FAKE_GOVERSION=go1.26.9"}, false, false, []string{"toolchain"}},
				{"toolchain default", goMod("default"), env{"FAKE_GOVERSION=go1.26.9"}, false, false, []string{"toolchain"}},
				// Nothing equals nothing: without the check of the line itself, a go command that printed no version would match a
				// go.mod that names none.
				{"no toolchain line and a go command that prints nothing", goMod(""), env{"FAKE_GOVERSION="}, false, false, []string{"toolchain"}},
			}
			for _, c := range cases {
				for _, how := range []string{"as the runner starts it", "under a bare bash"} {
					sb := newCIScriptSandbox(t)
					sb.standIn("go", toolchainProofGoStandIn)
					sb.writeFile("go.mod", c.goMod)
					run := sb.run
					if how == "under a bare bash" {
						run = sb.runPlain
					}
					out, rc := run(step.Run, c.env...)
					if c.wantOK && rc != 0 {
						t.Errorf("%s, %s: the step exited %d:\n%s", c.name, how, rc, out)
					}
					if !c.wantOK {
						if rc == 0 {
							t.Errorf("%s, %s: the step exited 0 and the job would go on with the wrong compiler:\n%s", c.name, how, out)
						}
						for _, want := range c.wantInText {
							if !strings.Contains(out, want) {
								t.Errorf("%s, %s: the output does not mention %q, so the log does not say what differs:\n%s", c.name, how, want, out)
							}
						}
					}
					// The question is put to the go command in the job's working directory, where go.mod is.
					asked := false
					for _, call := range sb.calls("go") {
						asked = asked || (call.Dir == "." && call.hasArgs("env", "GOVERSION"))
					}
					if c.asks && !asked {
						t.Errorf("%s, %s: the step never ran `go env GOVERSION` in the repository root: %v", c.name, how, sb.calls("go"))
					}
				}
			}
		})
	}
}
