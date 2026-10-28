package main

// H-26. CI scans the code that ships for known vulnerabilities, with the compiler that ships.
//
// Before this job nothing in the repository looked, so a Go advisory fixed in a patch release (the
// tree built with go1.27.1 calls eleven standard-library ones) would have shipped unnoticed. The job
// installs the newest govulncheck and runs it over the root module and both printed CLIs under tools/
// (separate modules, so `./...` at the root never reaches them) for linux AND windows, because some
// advisories are platform-specific: a scan of one OS says nothing of the other.
//
// Two details are easy to get wrong and are checked here:
//
//   - The compiler. Nothing names one: the job reads the floor from go.mod (setup-go's go-version-file, taken at its
//     newest by check-latest) and the go command downloads a newer toolchain when the module needs one.
//   - The verdict. govulncheck exits 3 for a finding and 1 for an error, such as a vulnerability
//     database it cannot reach; either must fail the job, for every module and OS, never only the last.
//
// The tests run the job's own scripts under bash with stand-ins for go and govulncheck, so a script that
// reads fine and exits 0 when it should not is caught, and they read the yaml for what a run cannot show.

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	govulncheckInstallRe = regexp.MustCompile(`go install\s+golang\.org/x/vuln/cmd/govulncheck@(\S+)`)
)

// The scan matrix: every module the repository builds, for every OS it ships on.
var (
	govulncheckModules = []string{".", "tools/comfyui", "tools/llamaswap"}
	govulncheckOSes    = []string{"linux", "windows"}
)

// govulncheckExcluded are the directories that hold a go.mod and are deliberately not scanned, each with the
// reason. A scan reads the code that ships, and these modules are not part of it. Every other go.mod in the
// tree must be in govulncheckModules (TestEveryTrackedGoModuleIsScannedOrNamedAsExcluded), so a new printed CLI
// is scanned or this list says why it is not.
var govulncheckExcluded = map[string]string{
	"contracts/write-door/t1": "a task module of a write-door agent contract (contract.json beside it): the work the contract hands " +
		"the agent, with a planted bug; it is not built into the harness and ships nothing",
	"contracts/write-door/t2": "a task module of a write-door agent contract (contract.json beside it): the work the contract hands " +
		"the agent; it is not built into the harness and ships nothing",
	"scripts/testdata/mutfixture": "the fixture module of the mutation-check self-test (the self-test copies it into a scratch folder and " +
		"mutates the copy): it is not built into the harness and ships nothing",
	"scripts/testdata/tripwirefixture": "the fixture module of the temp-folder tripwire self-test: its packages leave a folder, a process " +
		"and a failing test on purpose, so it is never built or tested with the harness and ships nothing",
}

// moduleScanProblems compares the directories that hold a go.mod (found) with the ones the govulncheck job
// scans and the ones named as excluded, and says what does not add up. The first two are slash paths relative
// to the repository root, "." for the root module.
func moduleScanProblems(found, scanned []string, excluded map[string]string) []string {
	var problems []string
	has := map[string]bool{}
	for _, d := range found {
		has[d] = true
	}
	inScan := map[string]bool{}
	for _, d := range scanned {
		inScan[d] = true
	}
	sorted := append([]string(nil), found...)
	sort.Strings(sorted)
	for _, d := range sorted {
		_, isExcluded := excluded[d]
		switch {
		case inScan[d] && isExcluded:
			problems = append(problems, d+" is both scanned and excluded: it is one or the other")
		case !inScan[d] && !isExcluded:
			problems = append(problems, d+" holds a go.mod and the govulncheck job does not scan it: add it to the loop in ci.yml "+
				"and to govulncheckModules, or name it in govulncheckExcluded with the reason it ships nothing")
		}
	}
	for _, d := range scanned {
		if !has[d] {
			problems = append(problems, "govulncheckModules names "+d+", which holds no go.mod")
		}
	}
	var stale []string
	for d := range excluded {
		if !has[d] {
			stale = append(stale, d)
		}
	}
	sort.Strings(stale)
	for _, d := range stale {
		problems = append(problems, "govulncheckExcluded names "+d+", which holds no go.mod: remove the stale entry")
	}
	return problems
}

// govulncheckJob is the job that scans, with the indexes of its steps.
type govulncheckJob struct {
	name      string
	job       ciJob
	setupGo   int // the last actions/setup-go step before the install, -1 when there is none
	install   int // the step that runs go install ...govulncheck@latest
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
			g := govulncheckJob{name: name, job: job, setupGo: -1, install: i, scan: -1}
			for j := 0; j < i; j++ {
				prev := job.Steps[j]
				if strings.HasPrefix(prev.Uses, "actions/setup-go@") {
					g.setupGo = j
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
		t.Fatal("ci.yml has no step that runs `go install golang.org/x/vuln/cmd/govulncheck@latest`: " +
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
	if tag != "latest" {
		t.Errorf("job %s installs govulncheck@%s: install the newest release (@latest); no version is pinned in this repository", g.name, tag)
	}
	// A job that is there and never gates: an `if` or a `continue-on-error` on the job or on any of its steps.
	for _, p := range ciAlwaysGatesProblems(g.name, job) {
		t.Error(p)
	}
	if g.setupGo < 0 {
		t.Errorf("job %s has no actions/setup-go step before the install", g.name)
	}
	if g.scan < 0 {
		t.Fatalf("job %s installs govulncheck and never runs it", g.name)
	}
	if !(g.setupGo < g.install && g.install < g.scan) {
		t.Errorf("job %s must set up Go, then install govulncheck, then scan; steps are at %d, %d, %d",
			g.name, g.setupGo, g.install, g.scan)
	}
	// The Go release comes from go.mod's floor, never from a version typed into the workflow.
	if st := job.Steps[g.setupGo]; st.With["go-version-file"] != "go.mod" || st.With["go-version"] != nil {
		t.Errorf("job %s: setup-go must read go.mod (go-version-file) and name no go-version; with is %v", g.name, st.With)
	}

	t.Run("govulncheck is installed and can be found", func(t *testing.T) {
		sb := newCIScriptSandbox(t)
		sb.standIn("go", goStandInBody)
		out, rc := sb.run(install.Run, "FAKE_GOPATH=/fake/gopath")
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
		out, rc := sb.run(scan.Run)
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
		scans := len(govulncheckModules) * len(govulncheckOSes)
		for _, f := range failures {
			// As the runner starts the step (-e on) and under a bare bash: the script must keep its own count of
			// the failures, not lean on an option the runner may drop.
			for _, how := range []string{"as the runner starts it", "under a bare bash"} {
				sb := newScanSandbox(t)
				run := sb.run
				if how == "under a bare bash" {
					run = sb.runPlain
				}
				out, rc := run(scan.Run, "FAKE_VULN_FAIL="+f.module+" "+f.goos, fmt.Sprintf("FAKE_VULN_RC=%d", f.code))
				if rc == 0 {
					t.Errorf("%s: govulncheck exited %d in %s for GOOS=%s and the step exited 0:\n%s", how, f.code, f.module, f.goos, out)
				}
				// All of them run, whichever one fails: a step that stops at the first finding hides what the others
				// would have reported, and the log of a red run is where the next fix starts.
				if n := len(sb.calls("govulncheck")); n != scans {
					t.Errorf("%s: govulncheck failed with %d in %s for GOOS=%s and only %d of the %d scans ran:\n%s",
						how, f.code, f.module, f.goos, n, scans, out)
				}
				// The failing scan is named, so the log says which module and system to look at.
				named := false
				for _, line := range strings.Split(out, "\n") {
					named = named || (strings.Contains(line, "::error::") &&
						strings.Contains(line, fmt.Sprintf("in %s for GOOS=%s", f.module, f.goos)))
				}
				if !named {
					t.Errorf("%s: no ::error:: line names %s for GOOS=%s:\n%s", how, f.module, f.goos, out)
				}
			}
		}
	})
}

// TestEveryTrackedGoModuleIsScannedOrNamedAsExcluded: "every module" is a fact about the tree, not about the two
// lists that repeat it (the loop in ci.yml and govulncheckModules). The printed CLIs under tools/ are separate
// modules the root's ./... never reaches, so a new one is invisible to every other check; this one finds it by
// its go.mod. The job test above holds ci.yml's loop to govulncheckModules exactly, so the chain is closed:
// tree, list, workflow.
func TestEveryTrackedGoModuleIsScannedOrNamedAsExcluded(t *testing.T) {
	files, _ := trackedFiles(t)
	var found []string
	for _, f := range files {
		if p := filepath.ToSlash(f); path.Base(p) == "go.mod" {
			found = append(found, path.Dir(p))
		}
	}
	if !hasString(found, ".") {
		t.Fatalf("the tracked files hold no root go.mod (%d files, go.mod directories %v): the check went blind", len(files), found)
	}
	for _, p := range moduleScanProblems(found, govulncheckModules, govulncheckExcluded) {
		t.Error(p)
	}
	for dir, why := range govulncheckExcluded {
		if strings.TrimSpace(why) == "" {
			t.Errorf("govulncheckExcluded names %s without a reason", dir)
		}
	}
}

// TestModuleScanCheckReportsEachProblem: the check above says what is wrong for each way the tree, the list and
// the exclusions can disagree, and nothing for a tree that agrees with them.
func TestModuleScanCheckReportsEachProblem(t *testing.T) {
	scanned := []string{".", "tools/comfyui", "tools/llamaswap"}
	excluded := map[string]string{"contracts/write-door/t1": "a fixture"}
	cases := []struct {
		name    string
		found   []string
		scanned []string
		expect  []string // substrings, one per expected problem, in order; none for a tree that agrees
	}{
		{"every module scanned, the fixture excluded",
			[]string{".", "tools/comfyui", "tools/llamaswap", "contracts/write-door/t1"}, scanned, nil},
		{"a new printed CLI",
			[]string{".", "tools/comfyui", "tools/llamaswap", "tools/newcli", "contracts/write-door/t1"}, scanned,
			[]string{"tools/newcli holds a go.mod and the govulncheck job does not scan it"}},
		{"a new module nobody listed, two of them, in order",
			[]string{".", "tools/comfyui", "tools/llamaswap", "tools/zeta", "tools/alpha", "contracts/write-door/t1"}, scanned,
			[]string{"tools/alpha holds a go.mod", "tools/zeta holds a go.mod"}},
		{"a scanned module whose go.mod is gone",
			[]string{".", "tools/comfyui", "contracts/write-door/t1"}, scanned,
			[]string{"govulncheckModules names tools/llamaswap, which holds no go.mod"}},
		{"an exclusion whose go.mod is gone",
			[]string{".", "tools/comfyui", "tools/llamaswap"}, scanned,
			[]string{"govulncheckExcluded names contracts/write-door/t1, which holds no go.mod"}},
		{"a module both scanned and excluded",
			[]string{".", "tools/comfyui", "tools/llamaswap", "contracts/write-door/t1"}, append([]string{"contracts/write-door/t1"}, scanned...),
			[]string{"contracts/write-door/t1 is both scanned and excluded"}},
	}
	for _, c := range cases {
		got := moduleScanProblems(c.found, c.scanned, excluded)
		if len(got) != len(c.expect) {
			t.Errorf("%s: reported %v, want %d problem(s) %v", c.name, got, len(c.expect), c.expect)
			continue
		}
		for i, want := range c.expect {
			if !strings.Contains(got[i], want) {
				t.Errorf("%s: problem %d is %q, want it to contain %q", c.name, i, got[i], want)
			}
		}
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
