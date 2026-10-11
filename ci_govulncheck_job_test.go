package main

// H-26. CI scans the code that ships for known vulnerabilities, with the compiler that ships.
//
// Before this job nothing in the repository looked, so a Go advisory fixed in a patch release (the
// tree built with go1.27.1 calls eleven standard-library ones) would have shipped unnoticed. The job
// installs a pinned govulncheck and runs it over the root module and both printed CLIs under tools/
// (separate modules, so `./...` at the root never reaches them) for linux AND windows, because some
// advisories are platform-specific: a scan of one OS says nothing of the other.
//
// Two details are easy to get wrong and are checked here:
//
//   - The compiler. govulncheck reports a standard-library finding against the toolchain that analyses
//     the code, and the printed CLIs carry no toolchain line of their own, so every go command of the
//     job must run under the toolchain go.mod names. The job reads that name from go.mod (never from
//     the workflow: two copies drift) into GOTOOLCHAIN. That also builds govulncheck itself with it,
//     which matters because `go install pkg@version` ignores the current module's toolchain line.
//   - The verdict. govulncheck exits 3 for a finding and 1 for an error, such as a vulnerability
//     database it cannot reach; either must fail the job, for every module and OS, never only the last.
//
// The tests run the job's own scripts under bash with stand-ins for go and govulncheck, so a script that
// reads fine and exits 0 when it should not is caught, and they read the yaml for what a run cannot show.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var (
	govulncheckInstallRe = regexp.MustCompile(`go install\s+golang\.org/x/vuln/cmd/govulncheck@(\S+)`)
	// A fixed release tag: not @latest, a branch, or a bare commit.
	fixedTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// The scan matrix: every module the repository builds, for every OS it ships on.
var (
	govulncheckModules = []string{".", "tools/comfyui", "tools/llamaswap"}
	govulncheckOSes    = []string{"linux", "windows"}
)

// govulncheckJob is the job that scans, with the indexes of its steps.
type govulncheckJob struct {
	name      string
	job       ciJob
	setupGo   int // the last actions/setup-go step before the toolchain step, -1 when there is none
	toolchain int // the step that writes GOTOOLCHAIN to GITHUB_ENV, -1 when there is none
	install   int // the step that runs go install ...govulncheck@<tag>
	scan      int // the first step after the install that runs govulncheck, -1 when there is none
}

// findGovulncheckJob finds the one job that installs govulncheck.
func findGovulncheckJob(t *testing.T, w ciWorkflow) govulncheckJob {
	t.Helper()
	var found []govulncheckJob
	for _, name := range w.jobNames() {
		job := w.Jobs[name]
		for i, st := range job.Steps {
			if !govulncheckInstallRe.MatchString(st.Run) {
				continue
			}
			g := govulncheckJob{name: name, job: job, setupGo: -1, toolchain: -1, install: i, scan: -1}
			for j := 0; j < i; j++ {
				prev := job.Steps[j]
				if strings.HasPrefix(prev.Uses, "actions/setup-go@") {
					g.setupGo = j
				}
				if strings.Contains(prev.Run, "GITHUB_ENV") && strings.Contains(prev.Run, "GOTOOLCHAIN") {
					g.toolchain = j
				}
			}
			for j := i + 1; j < len(job.Steps); j++ {
				if strings.Contains(job.Steps[j].Run, "govulncheck") {
					g.scan = j
					break
				}
			}
			found = append(found, g)
		}
	}
	switch len(found) {
	case 0:
		t.Fatal("ci.yml has no step that runs `go install golang.org/x/vuln/cmd/govulncheck@<tag>`: " +
			"nothing scans the code for known vulnerabilities (H-26)")
	case 1:
		return found[0]
	}
	t.Fatalf("ci.yml installs govulncheck in %d places: one job owns the scan", len(found))
	return govulncheckJob{}
}

// hasLine reports whether text has line as one of its lines, exactly.
func hasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// The stand-ins for the commands the job's steps call. Both record the call; go answers `go install` and
// `go env GOPATH`, and govulncheck fails for the one module and OS the test names.
const goStandInBody = `record go "$@"
case "$1" in
  install) exit "${FAKE_GO_INSTALL_RC:-0}" ;;
  env) echo "${FAKE_GOPATH:-/fake/gopath}" ;;
esac
`

const govulncheckStandInBody = `record govulncheck "$@"
if [ "$module ${GOOS-}" = "${FAKE_VULN_FAIL-}" ]; then
  echo "FAKE govulncheck: Vulnerability #1: GO-0000-0000 (planted by the test)"
  exit "${FAKE_VULN_RC:-3}"
fi
echo "FAKE govulncheck: No vulnerabilities found."
`

func TestCIRunsGovulncheckOnEveryModuleOnBothOSes(t *testing.T) {
	g := findGovulncheckJob(t, loadCIWorkflow(t))
	job := g.job
	install := job.Steps[g.install]

	// What only the yaml can show.
	if got := job.runner(); got != "ubuntu-latest" {
		t.Errorf("job %s runs on %q, want ubuntu-latest: the standard runner is free on a public repository (a "+
			"larger runner is always billed) and analyses GOOS=windows from Linux", g.name, got)
	}
	tag := govulncheckInstallRe.FindStringSubmatch(install.Run)[1]
	if !fixedTagRe.MatchString(tag) {
		t.Errorf("job %s installs govulncheck@%s: pin a release tag such as v1.8.0, so a new release of the "+
			"scanner cannot change what a pull request is judged by", g.name, tag)
	}
	if g.setupGo < 0 {
		t.Errorf("job %s has no actions/setup-go step before its toolchain step", g.name)
	}
	if g.toolchain < 0 {
		t.Fatalf("job %s has no step before the install that writes GOTOOLCHAIN to $GITHUB_ENV from go.mod: the "+
			"nested modules have no toolchain line, so without it they are scanned with the runner's own compiler", g.name)
	}
	if g.scan < 0 {
		t.Fatalf("job %s installs govulncheck and never runs it", g.name)
	}
	if !(g.setupGo < g.toolchain && g.toolchain < g.install && g.install < g.scan) {
		t.Errorf("job %s must set up Go, then pin the toolchain, then install govulncheck, then scan; steps are at %d, %d, %d, %d",
			g.name, g.setupGo, g.toolchain, g.install, g.scan)
	}

	t.Run("the toolchain step reads go.mod", func(t *testing.T) {
		step := job.Steps[g.toolchain]
		// A version nobody ships: if the step read it from the file, GOTOOLCHAIN has it; if it were typed into
		// the workflow, the value would be the workflow's.
		const made = "go1.99.9"
		for _, c := range []struct {
			name, goMod string
		}{
			{"LF", "module example.invalid/m\n\ngo 1.26.7\n\ntoolchain " + made + "\n"},
			{"CRLF", "module example.invalid/m\r\n\r\ngo 1.26.7\r\n\r\ntoolchain " + made + "\r\n"},
		} {
			sb := newCIScriptSandbox(t)
			sb.writeFile("go.mod", c.goMod)
			if out, rc := sb.run(step.Run); rc != 0 {
				t.Errorf("%s go.mod: the step exited %d:\n%s", c.name, rc, out)
			}
			if got := sb.githubEnv(); !hasLine(got, "GOTOOLCHAIN="+made) {
				t.Errorf("%s go.mod: GITHUB_ENV is %q, want a line GOTOOLCHAIN=%s", c.name, got, made)
			}
		}
		for _, c := range []struct {
			name, goMod string
		}{
			{"no toolchain line", "module example.invalid/m\n\ngo 1.26.7\n"},
			{"toolchain default", "module example.invalid/m\n\ngo 1.26.7\n\ntoolchain default\n"},
		} {
			// An empty GOTOOLCHAIN means "the default", and the job would carry on with the runner's own
			// compiler and call it pinned: the step must fail instead.
			sb := newCIScriptSandbox(t)
			sb.writeFile("go.mod", c.goMod)
			if out, rc := sb.run(step.Run); rc == 0 {
				t.Errorf("%s: the step exited 0 and the job would go on unpinned:\n%s", c.name, out)
			}
			if got := sb.githubEnv(); strings.Contains(got, "GOTOOLCHAIN") {
				t.Errorf("%s: the step wrote %q to GITHUB_ENV", c.name, got)
			}
		}
	})

	t.Run("govulncheck is built by that toolchain and can be found", func(t *testing.T) {
		sb := newCIScriptSandbox(t)
		sb.standIn("go", goStandInBody)
		out, rc := sb.run(install.Run, "GOTOOLCHAIN=go1.99.9", "FAKE_GOPATH=/fake/gopath")
		if rc != 0 {
			t.Fatalf("the install step exited %d:\n%s", rc, out)
		}
		var installs []ciCall
		for _, c := range sb.calls("go") {
			if len(c.Args) > 0 && c.Args[0] == "install" {
				installs = append(installs, c)
			}
		}
		if len(installs) != 1 {
			t.Fatalf("the install step ran %d go install commands, want 1: %v", len(installs), sb.calls("go"))
		}
		if want := "golang.org/x/vuln/cmd/govulncheck@" + tag; !installs[0].hasArgs("install", want) {
			t.Errorf("go install ran with %v, want install %s", installs[0].Args, want)
		}
		if installs[0].Toolchain != "go1.99.9" {
			t.Errorf("go install saw GOTOOLCHAIN=%s, want go1.99.9: the scanner must be built by the toolchain that analyses the code",
				installs[0].Toolchain)
		}
		if got := sb.githubPath(); !hasLine(got, "/fake/gopath/bin") {
			t.Errorf("GITHUB_PATH is %q: the scan finds govulncheck only if $(go env GOPATH)/bin is on PATH, and the "+
				"step does not rely on the setup action to put it there", got)
		}
		// A scanner that failed to install must not be followed by a scan that quietly finds nothing to run, and
		// the script must say so on its own: under a bare bash too, without the options the runner adds.
		for _, how := range []string{"as the runner starts it", "under a bare bash"} {
			sb = newCIScriptSandbox(t)
			sb.standIn("go", goStandInBody)
			run := sb.run
			if how == "under a bare bash" {
				run = sb.runPlain
			}
			if out, rc := run(install.Run, "FAKE_GO_INSTALL_RC=1"); rc == 0 {
				t.Errorf("%s: the install step exited 0 although go install failed:\n%s", how, out)
			}
		}
	})

	scan := job.Steps[g.scan]
	newScanSandbox := func(t *testing.T) *ciScriptSandbox {
		sb := newCIScriptSandbox(t)
		sb.standIn("govulncheck", govulncheckStandInBody)
		for _, m := range govulncheckModules {
			sb.writeFile(m+"/go.mod", "module example.invalid/"+strings.ReplaceAll(m, "/", "_")+"\n\ngo 1.26.7\n")
		}
		return sb
	}

	t.Run("every module is scanned for both OSes", func(t *testing.T) {
		sb := newScanSandbox(t)
		out, rc := sb.run(scan.Run, "GOTOOLCHAIN=go1.99.9")
		if rc != 0 {
			t.Fatalf("a scan that finds nothing exited %d:\n%s", rc, out)
		}
		calls := sb.calls("govulncheck")
		seen := map[string]int{}
		for _, c := range calls {
			seen[c.Dir+" "+c.Goos]++
			if !c.hasArgs("-show", "version") {
				t.Errorf("govulncheck in %s for %s ran with %v: -show version puts the toolchain and scanner versions in the log",
					c.Dir, c.Goos, c.Args)
			}
			if n := len(c.Args); n == 0 || c.Args[n-1] != "./..." {
				t.Errorf("govulncheck in %s for %s ran with %v, want the package pattern ./... last", c.Dir, c.Goos, c.Args)
			}
			if c.Toolchain != "go1.99.9" {
				t.Errorf("govulncheck in %s for %s saw GOTOOLCHAIN=%s, want go1.99.9", c.Dir, c.Goos, c.Toolchain)
			}
		}
		for _, m := range govulncheckModules {
			for _, goos := range govulncheckOSes {
				if seen[m+" "+goos] != 1 {
					t.Errorf("govulncheck ran %d times in %s for GOOS=%s, want once; calls: %v", seen[m+" "+goos], m, goos, calls)
				}
			}
		}
		if len(calls) != len(govulncheckModules)*len(govulncheckOSes) {
			t.Errorf("govulncheck ran %d times, want %d", len(calls), len(govulncheckModules)*len(govulncheckOSes))
		}
	})

	t.Run("a finding or an error fails the step", func(t *testing.T) {
		type failure struct {
			module, goos string
			code         int
		}
		var failures []failure
		for _, m := range govulncheckModules {
			for _, goos := range govulncheckOSes {
				failures = append(failures, failure{m, goos, 3}) // 3: vulnerabilities found
			}
		}
		// 1: the scan could not run (the vulnerability database is unreachable, a package does not load).
		failures = append(failures, failure{govulncheckModules[len(govulncheckModules)-1], govulncheckOSes[len(govulncheckOSes)-1], 1})
		for _, f := range failures {
			sb := newScanSandbox(t)
			out, rc := sb.run(scan.Run, "FAKE_VULN_FAIL="+f.module+" "+f.goos, fmt.Sprintf("FAKE_VULN_RC=%d", f.code))
			if rc == 0 {
				t.Errorf("govulncheck exited %d in %s for GOOS=%s and the step exited 0:\n%s", f.code, f.module, f.goos, out)
			}
		}
	})
}
