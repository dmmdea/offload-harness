package main

// H-26. The root module names the toolchain that ships.
//
// go.mod carries `go 1.26.7`, which is only the OLDEST compiler allowed to build the module. The
// compiler that built every shipped binary was whatever each box happened to have (go1.27.1 on the
// workstation), CI floated on setup-go's `1.26.x`, and so no job ran the toolchain that ships. A
// `toolchain` line says which one that is: with GOTOOLCHAIN at its default (auto) the go command
// fetches and runs it when the local one is older (go.dev/doc/toolchain), so a box on an older Go
// builds with the pinned one, and setup-go@v5 (which leaves GOTOOLCHAIN alone) lets CI do the same.
//
// The pin is a floor, not an exact match: a newer patch release is always welcome, a lower one never.
// Raise the floors below with go.mod when a Go advisory is fixed in a newer patch, because the
// govulncheck job in ci.yml reports a standard-library finding for as long as the pin lags it.

import (
	"fmt"
	gover "go/version"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The floors. toolchainFloor is the first release that carries the fixes for the standard-library
// advisories open against the release before it: go1.27.2 when this lane was added (the planted scan with
// go1.27.1 reports them and the go1.27.2 scan does not), then go1.27.3 for GO-2026-6809 (crypto/x509
// distrusts compromised intermediates; reached through cliutil.ProbeReachable). The module floors guard
// against a downgrade: they are the versions the tree builds with today.
const toolchainFloor = "go1.27.3"

var moduleFloors = []struct{ path, version string }{
	{"golang.org/x/sys", "v0.48.0"},
	{"golang.org/x/text", "v0.41.0"},
	{"github.com/modelcontextprotocol/go-sdk", "v1.7.0"},
}

// goModFile is what the pin test needs of a go.mod: the go and toolchain directives and the required
// versions. go.mod's grammar is small, and x/mod is not a dependency of this module, so it is read
// line by line.
type goModFile struct {
	Go        string            // "1.26.7"
	Toolchain string            // "go1.27.2", "" when there is no toolchain line
	Requires  map[string]string // module path to version, direct and indirect
}

func parseGoMod(src string) goModFile {
	m := goModFile{Requires: map[string]string{}}
	inRequire := false
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line) // also drops the CR of a CRLF checkout
		if line == "" {
			continue
		}
		switch {
		case inRequire:
			if line == ")" {
				inRequire = false
			} else if f := strings.Fields(line); len(f) >= 2 {
				m.Requires[f[0]] = f[1]
			}
		case line == "require (":
			inRequire = true
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(strings.TrimPrefix(line, "require ")); len(f) >= 2 {
				m.Requires[f[0]] = f[1]
			}
		case strings.HasPrefix(line, "go "):
			m.Go = strings.TrimSpace(strings.TrimPrefix(line, "go "))
		case strings.HasPrefix(line, "toolchain "):
			m.Toolchain = strings.TrimSpace(strings.TrimPrefix(line, "toolchain "))
		}
	}
	return m
}

// goVersionAtLeast reports have >= want for Go release names: go1.27.10 is newer than go1.27.2.
// go/version compares the numeric parts as numbers, so the release candidate and the language version
// order the way the go command orders them.
func goVersionAtLeast(have, want string) bool {
	return gover.IsValid(have) && gover.IsValid(want) && gover.Compare(have, want) >= 0
}

// moduleVersionAtLeast reports have >= want for module versions ("v0.100.0" is newer than "v0.48.0").
// Segments compare as numbers; a pre-release ("v1.7.0-rc.1") sorts below its release, as semver says.
func moduleVersionAtLeast(have, want string) bool {
	return compareModuleVersions(have, want) >= 0
}

func compareModuleVersions(a, b string) int {
	coreA, preA := splitModuleVersion(a)
	coreB, preB := splitModuleVersion(b)
	for i := 0; i < 3; i++ {
		if coreA[i] != coreB[i] {
			if coreA[i] < coreB[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	}
	fa, fb := strings.Split(preA, "."), strings.Split(preB, ".")
	for i := 0; i < len(fa) && i < len(fb); i++ {
		na, errA := strconv.Atoi(fa[i])
		nb, errB := strconv.Atoi(fb[i])
		switch {
		case errA == nil && errB == nil && na != nb:
			if na < nb {
				return -1
			}
			return 1
		case errA == nil && errB != nil: // a numeric identifier sorts below an alphanumeric one
			return -1
		case errA != nil && errB == nil:
			return 1
		case errA != nil && errB != nil && fa[i] != fb[i]:
			return strings.Compare(fa[i], fb[i])
		}
	}
	switch {
	case len(fa) < len(fb): // all the shared identifiers are equal: the longer pre-release is the greater
		return -1
	case len(fa) > len(fb):
		return 1
	}
	return 0
}

// splitModuleVersion returns the numeric major, minor and patch of a module version and its pre-release
// part. Anything that is not a number reads as 0, so a malformed version never compares above a real one.
func splitModuleVersion(v string) (core [3]int, pre string) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "-"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		core[i], _ = strconv.Atoi(part)
	}
	return core, pre
}

// goModPinProblems lists what is wrong with a go.mod as far as the pin goes: the toolchain line is missing,
// is not a Go release, is below the floor or below the go line, or a module the tree depends on went back.
func goModPinProblems(m goModFile) []string {
	var problems []string
	switch {
	case m.Toolchain == "":
		problems = append(problems, "go.mod has no toolchain line: add `toolchain "+toolchainFloor+"` after the go line, so that "+
			"the build box, the gate hosts and CI build with the compiler that ships (H-26)")
	case !gover.IsValid(m.Toolchain):
		problems = append(problems, "go.mod toolchain "+strconv.Quote(m.Toolchain)+" is not a Go release name such as "+toolchainFloor)
	default:
		if !goVersionAtLeast(m.Toolchain, toolchainFloor) {
			problems = append(problems, "go.mod toolchain is "+m.Toolchain+", below the floor "+toolchainFloor+": that release "+
				"still carries the standard-library advisories the govulncheck job reports")
		}
		if m.Go == "" {
			problems = append(problems, "go.mod has no go line")
		} else if !goVersionAtLeast(m.Toolchain, "go"+m.Go) {
			problems = append(problems, "go.mod toolchain "+m.Toolchain+" is older than its go line "+m.Go+": the go command "+
				"ignores a toolchain line below the go line, so the pin would do nothing")
		}
	}
	for _, floor := range moduleFloors {
		got, ok := m.Requires[floor.path]
		switch {
		case !ok:
			problems = append(problems, "go.mod no longer requires "+floor.path+" (floor "+floor.version+")")
		case !moduleVersionAtLeast(got, floor.version):
			problems = append(problems, "go.mod requires "+floor.path+" "+got+", below the floor "+floor.version)
		}
	}
	return problems
}

// TestRootGoModPinsAPatchedToolchain: go.mod has a toolchain line, it is at least the floor and at least
// the go line, and the modules whose fixes this tree depends on have not gone back.
func TestRootGoModPinsAPatchedToolchain(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(ciRepoRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range goModPinProblems(parseGoMod(string(raw))) {
		t.Error(p)
	}
}

// TestGoModPinCheckReportsEachProblem: the check above says what is wrong, for each way a go.mod can go wrong,
// and says nothing about a go.mod that is right or newer. It runs on parsed text rather than on the real
// go.mod because a real go.mod with an older module in it no longer builds (go.sum has no entry for it).
func TestGoModPinCheckReportsEachProblem(t *testing.T) {
	good := func() goModFile {
		return goModFile{Go: "1.26.7", Toolchain: toolchainFloor, Requires: map[string]string{
			"golang.org/x/sys":                       "v0.48.0",
			"golang.org/x/text":                      "v0.41.0",
			"github.com/modelcontextprotocol/go-sdk": "v1.7.0",
		}}
	}
	cases := []struct {
		name   string
		edit   func(*goModFile)
		expect string // a substring of the one problem expected; "" for none
	}{
		{"as shipped", func(*goModFile) {}, ""},
		{"newer everything", func(m *goModFile) {
			m.Toolchain = "go1.27.10"
			m.Requires["golang.org/x/sys"] = "v0.100.0"
			m.Requires["golang.org/x/text"] = "v1.0.0"
			m.Requires["github.com/modelcontextprotocol/go-sdk"] = "v1.10.0"
		}, ""},
		{"no toolchain line", func(m *goModFile) { m.Toolchain = "" }, "no toolchain line"},
		{"toolchain default", func(m *goModFile) { m.Toolchain = "default" }, "not a Go release name"},
		{"toolchain below the floor", func(m *goModFile) { m.Toolchain = "go1.27.1" }, "below the floor go1.27.3"},
		{"toolchain below the go line", func(m *goModFile) { m.Go = "1.28.0"; m.Toolchain = "go1.27.9" }, "older than its go line"},
		{"x/sys went back", func(m *goModFile) { m.Requires["golang.org/x/sys"] = "v0.9.0" }, "golang.org/x/sys v0.9.0, below the floor v0.48.0"},
		{"x/text went back", func(m *goModFile) { m.Requires["golang.org/x/text"] = "v0.40.0" }, "golang.org/x/text v0.40.0, below the floor v0.41.0"},
		{"go-sdk went back", func(m *goModFile) { m.Requires["github.com/modelcontextprotocol/go-sdk"] = "v1.6.9" }, "below the floor v1.7.0"},
		{"go-sdk dropped", func(m *goModFile) { delete(m.Requires, "github.com/modelcontextprotocol/go-sdk") }, "no longer requires github.com/modelcontextprotocol/go-sdk"},
	}
	for _, c := range cases {
		m := good()
		c.edit(&m)
		got := goModPinProblems(m)
		switch {
		case c.expect == "" && len(got) != 0:
			t.Errorf("%s: reported %v, want nothing", c.name, got)
		case c.expect != "" && (len(got) != 1 || !strings.Contains(got[0], c.expect)):
			t.Errorf("%s: reported %v, want exactly one problem containing %q", c.name, got, c.expect)
		}
	}
}

// TestCISetupGoLeavesTheToolchainSwitchAlone: the root-module jobs install `go-version: '1.26.x'` and rely
// on go.mod's toolchain line to move them to the pinned compiler. That holds because setup-go@v5 leaves
// GOTOOLCHAIN at its default: its v5.6.0 source never mentions GOTOOLCHAIN or the toolchain directive.
// From v6 the action exports GOTOOLCHAIN=local, and under local the go command builds with the installed
// compiler and ignores the toolchain line without printing anything (measured with go1.27.1 installed and
// `toolchain go1.27.2` in go.mod: go build succeeds and go version -m reads go1.27.1). A bump of the
// action, or a GOTOOLCHAIN=local in the workflow, would therefore turn "CI tests the compiler that ships"
// into a false claim with every check green. This test is the tripwire for the causes the yaml shows;
// setupGoProblem is its rule. What a runner exports is not in the yaml, so the jobs also ask the go command
// itself (TestCIJobsProveTheCompilerGoModNames).
func TestCISetupGoLeavesTheToolchainSwitchAlone(t *testing.T) {
	w := loadCIWorkflow(t)
	if v, ok := w.Env["GOTOOLCHAIN"]; ok {
		t.Errorf("the workflow sets GOTOOLCHAIN=%q for every job: the toolchain line of go.mod must decide", v)
	}
	for _, name := range w.jobNames() {
		job := w.Jobs[name]
		if v, ok := job.Env["GOTOOLCHAIN"]; ok {
			t.Errorf("job %s sets GOTOOLCHAIN=%q: the toolchain line of go.mod must decide", name, v)
		}
		for _, st := range job.Steps {
			if v, ok := st.Env["GOTOOLCHAIN"]; ok {
				t.Errorf("step %q of job %s sets GOTOOLCHAIN=%q: the toolchain line of go.mod must decide", st.label(), name, v)
			}
			if p := setupGoProblem(st); p != "" {
				t.Errorf("job %s, step %q: %s", name, st.label(), p)
			}
		}
	}
}

// setupGoProblem says why a step that uses actions/setup-go would stop go.mod's toolchain line from deciding
// the compiler of its job, and "" when the step is fine or is not a setup-go step.
//
//   - v5 and v5.x leave GOTOOLCHAIN alone, so `go-version: '1.26.x'` only supplies a go command to start from
//     and the first go command switches to the toolchain line (go.dev/doc/toolchain).
//   - From v6 the action exports GOTOOLCHAIN=local, so the switch never happens; the one way left is to have
//     the action install the pinned release itself: `go-version-file: go.mod`, where the action takes the
//     toolchain line before the go line. `go-version` wins over `go-version-file` when both are set (the
//     action logs a warning), so it must not be there. By the action's documented run order it reads the file
//     before it exports local, so the job's first setup-go step sees the toolchain line and a second one in
//     the same job would see local already and read the go line (read from its documentation, not measured
//     on a runner: no hosted run of this change exists yet).
//
// Any other reference (a commit hash, a branch) is refused: nothing here knows what it does.
func setupGoProblem(st ciStep) string {
	ref, ok := strings.CutPrefix(st.Uses, "actions/setup-go@")
	if !ok {
		return ""
	}
	const base = "uses actions/setup-go@"
	m := setupGoTagRe.FindStringSubmatch(ref)
	if m == nil {
		return base + ref + ": only v5 is known to leave GOTOOLCHAIN alone, and this is no release tag whose behaviour is known"
	}
	major, _ := strconv.Atoi(m[1])
	switch {
	case major == 5:
		return ""
	case major < 5:
		return base + ref + ": only v5 is known to leave GOTOOLCHAIN alone"
	}
	file, _ := st.With["go-version-file"].(string)
	switch {
	case file == "":
		return base + ref + ": only v5 is known to leave GOTOOLCHAIN alone; this release exports GOTOOLCHAIN=local, which makes " +
			"the toolchain line of go.mod a silent no-op. Set `go-version-file: go.mod` (and no go-version) so the action installs " +
			"the toolchain line's release itself"
	case file != "go.mod":
		return base + ref + ": go-version-file is " + strconv.Quote(file) + ", and only go.mod carries the toolchain line this " +
			"repository pins"
	}
	if v, has := st.With["go-version"]; has && v != nil && fmt.Sprint(v) != "" {
		return base + ref + ": go-version and go-version-file are both set, and go-version wins, so go.mod's toolchain line " +
			"is never read"
	}
	return ""
}

// setupGoTagRe is a release tag of the action: v5, v5.6 or v5.6.0. The major version is its first group.
var setupGoTagRe = regexp.MustCompile(`^v([0-9]+)(?:\.[0-9]+){0,2}$`)

// TestSetupGoRuleKnowsTheMigrationFromV5: the rule above in a table, because the real workflow only ever holds
// the shapes it holds today. The accepted rows are the ones ci.yml and the CHANGELOG name as the way to bump
// the action; the refused rows are the ways a bump goes wrong with every check green.
func TestSetupGoRuleKnowsTheMigrationFromV5(t *testing.T) {
	cases := []struct {
		name   string
		uses   string
		with   map[string]any
		expect string // a substring of the problem; "" for none
	}{
		{"v5 with a version range", "actions/setup-go@v5", map[string]any{"go-version": "1.26.x"}, ""},
		{"a v5 patch release", "actions/setup-go@v5.6.0", map[string]any{"go-version": "1.26.x"}, ""},
		{"not a setup-go step", "actions/checkout@v4", nil, ""},
		{"v6 with a version range", "actions/setup-go@v6", map[string]any{"go-version": "1.26.x"}, "only v5 is known"},
		{"v6 with nothing", "actions/setup-go@v6", nil, "only v5 is known"},
		{"v6 reading go.mod", "actions/setup-go@v6", map[string]any{"go-version-file": "go.mod"}, ""},
		{"v7 reading go.mod, cache off", "actions/setup-go@v7", map[string]any{"go-version-file": "go.mod", "cache": false}, ""},
		{"v6 reading go.mod and naming a version", "actions/setup-go@v6", map[string]any{"go-version-file": "go.mod", "go-version": "1.26.x"}, "go-version wins"},
		{"v6 reading go.mod and a numeric version", "actions/setup-go@v6", map[string]any{"go-version-file": "go.mod", "go-version": 1.26}, "go-version wins"},
		{"v6 reading another file", "actions/setup-go@v6", map[string]any{"go-version-file": ".go-version"}, "only go.mod"},
		{"v6 reading a go.mod of a nested module", "actions/setup-go@v6", map[string]any{"go-version-file": "tools/comfyui/go.mod"}, "only go.mod"},
		{"a commit hash", "actions/setup-go@0123456789abcdef0123456789abcdef01234567", map[string]any{"go-version-file": "go.mod"}, "only v5 is known"},
	}
	for _, c := range cases {
		got := setupGoProblem(ciStep{Uses: c.uses, With: c.with})
		switch {
		case c.expect == "" && got != "":
			t.Errorf("%s: reported %q, want nothing", c.name, got)
		case c.expect != "" && !strings.Contains(got, c.expect):
			t.Errorf("%s: reported %q, want a problem containing %q", c.name, got, c.expect)
		}
	}
}

// TestGoVersionCompareIsNumeric: the two comparators the pin test uses read the parts of a version as
// numbers. Compared as text, go1.27.10 sorts below go1.27.2 and v0.100.0 below v0.48.0, and a pin test
// built on that would call a newer toolchain a downgrade (or the reverse) the day a version grows a digit.
func TestGoVersionCompareIsNumeric(t *testing.T) {
	goCases := []struct {
		have, want string
		atLeast    bool
	}{
		{"go1.27.10", "go1.27.2", true},
		{"go1.27.2", "go1.27.10", false},
		{"go1.27.2", "go1.27.2", true},
		{"go1.27.1", "go1.27.2", false},
		{"go1.28", "go1.27.9", true},
		{"go1.26.9", "go1.27.2", false},
		{"go1.100.0", "go1.27.2", true},
		{"go1.27rc1", "go1.27.0", false}, // a release candidate is below the release
		{"1.27.2", "go1.27.2", false},    // not a Go release name
		{"default", "go1.27.2", false},
		{"", "go1.27.2", false},
	}
	for _, c := range goCases {
		if got := goVersionAtLeast(c.have, c.want); got != c.atLeast {
			t.Errorf("goVersionAtLeast(%q, %q) = %v, want %v", c.have, c.want, got, c.atLeast)
		}
	}

	moduleCases := []struct {
		a, b string
		cmp  int
	}{
		{"v0.100.0", "v0.48.0", 1},
		{"v0.48.0", "v0.100.0", -1},
		{"v0.48.0", "v0.48.0", 0},
		{"v1.10.0", "v1.9.9", 1},
		{"v1.7.0", "v1.6.99", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.7.0-rc.1", "v1.7.0", -1},
		{"v1.7.0", "v1.7.0-rc.1", 1},
		{"v1.7.0-rc.2", "v1.7.0-rc.10", -1},
		{"v1.7.0+meta", "v1.7.0", 0},
		{"v0.0.0-20230129092748-24d4a6f8daec", "v0.0.1", -1},
	}
	for _, c := range moduleCases {
		if got := compareModuleVersions(c.a, c.b); got != c.cmp {
			t.Errorf("compareModuleVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.cmp)
		}
	}
	if !moduleVersionAtLeast("v0.100.0", "v0.48.0") || moduleVersionAtLeast("v0.9.0", "v0.48.0") {
		t.Error("moduleVersionAtLeast must compare v0.100.0 above v0.48.0 and v0.9.0 below it")
	}
}

// TestParseGoModReadsTheDirectivesThePinTestNeeds: the line reader handles the shapes go.mod really has:
// a block and a single-line require, indirect comments, a replace block, and CRLF line endings.
func TestParseGoModReadsTheDirectivesThePinTestNeeds(t *testing.T) {
	src := "module example.invalid/m\r\n\r\ngo 1.26.7\r\n\r\ntoolchain go1.27.2\r\n\r\n" +
		"require (\r\n\texample.invalid/a v1.2.3\r\n\texample.invalid/b v0.4.5 // indirect\r\n)\r\n\r\n" +
		"require example.invalid/c v9.9.9\r\n\r\n" +
		"replace (\r\n\texample.invalid/a => ./a\r\n)\r\n"
	m := parseGoMod(src)
	if m.Go != "1.26.7" || m.Toolchain != "go1.27.2" {
		t.Errorf("go/toolchain = %q/%q, want 1.26.7/go1.27.2", m.Go, m.Toolchain)
	}
	want := map[string]string{"example.invalid/a": "v1.2.3", "example.invalid/b": "v0.4.5", "example.invalid/c": "v9.9.9"}
	if len(m.Requires) != len(want) {
		t.Errorf("requires = %v, want %v", m.Requires, want)
	}
	for path, v := range want {
		if m.Requires[path] != v {
			t.Errorf("requires[%s] = %q, want %q", path, m.Requires[path], v)
		}
	}
	if got := parseGoMod("module m\n\ngo 1.26.7\n"); got.Toolchain != "" {
		t.Errorf("a go.mod without a toolchain line reads toolchain %q", got.Toolchain)
	}
}
