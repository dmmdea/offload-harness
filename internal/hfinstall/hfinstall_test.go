package hfinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Versions here are fixtures: the package's contract is "newest release, at or above a floor",
// so nothing below depends on any real HyperFrames version.
const (
	newest     = "9.9.9-fixture"
	olderBuild = "1.2.0"
	floorBuild = "1.0.0"
)

const setupPkg = `{"name":"offload-harness-hyperframes","private":true,"dependencies":{"hyperframes":"latest"}}`
const oldPkg = `{"name":"offload-harness-hyperframes","private":true,"dependencies":{"hyperframes":"latest"},"note":"previous"}`

// box lays out a runner stub beside a floors.json and, when installed is set, an install of
// that version written the way an older release left it (its own package.json, a marker file).
func box(t *testing.T, installed string) (runner, dir string) {
	t.Helper()
	root := t.TempDir()
	runner = filepath.Join(root, "render", "compose-hyperframes.mjs")
	write(t, runner, "#!/usr/bin/env node\nexport const X = 1;\n")
	write(t, filepath.Join(root, "setup", "floors.json"), `{"hyperframes":{"harness":"`+floorBuild+`"}}`)
	dir = filepath.Join(root, "hyperframes")
	if installed != "" {
		installAt(t, dir, installed)
		pkg := oldPkg
		if installed == newest {
			pkg = setupPkg
		}
		write(t, filepath.Join(dir, "package.json"), pkg)
		write(t, filepath.Join(dir, "node_modules", "marker-"+installed), "")
	}
	return runner, dir
}

func installAt(t *testing.T, dir, version string) {
	t.Helper()
	write(t, filepath.Join(dir, "node_modules", "hyperframes", "package.json"), `{"name":"hyperframes","version":"`+version+`"}`)
	write(t, filepath.Join(dir, "node_modules", "hyperframes", "bin", "hyperframes.mjs"), "")
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// fake records every command (name, args, cwd), answers the registry's latest tag, and plays
// npm install by installing `resolves` in the directory it runs in.
type fake struct {
	t        *testing.T
	calls    []string
	cwds     []string
	argv     [][]string
	failOn   string // a call prefix that fails
	lsFails  bool
	browser  string
	version  string
	latest   string // what `npm view hyperframes dist-tags.latest` prints
	resolves string // what `npm install` leaves in node_modules/hyperframes
}

func (f *fake) run(_ context.Context, dir string, _ []string, name string, args ...string) (string, error) {
	call := filepath.Base(name) + " " + strings.Join(args, " ")
	if strings.HasSuffix(args[0], ".mjs") {
		call = "runner " + args[1]
	}
	f.calls, f.cwds, f.argv = append(f.calls, call), append(f.cwds, dir), append(f.argv, args)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return "", errors.New("exit status 1: simulated")
	}
	switch {
	case strings.HasPrefix(call, "npm view"):
		return f.latest + "\n", nil
	case strings.HasPrefix(call, "npm install"):
		installAt(f.t, dir, f.resolves)
	case strings.HasPrefix(call, "npm ls") && f.lsFails:
		return "", errors.New("exit status 1: missing: hono@^4")
	case call == "runner browser":
		return "progress\n" + f.browser + "\n", nil
	case call == "runner version":
		if strings.Contains(f.version, `"ok":false`) {
			return "COMPOSE-FAIL: CLI_MISSING: x\n" + f.version + "\n", errors.New("exit status 1")
		}
		return f.version + "\n", nil
	}
	return "", nil
}

func newFake(t *testing.T) *fake {
	return &fake{t: t, latest: newest, resolves: newest,
		browser: `{"ok":true,"op":"browser","browser_path":"/b/chrome-headless-shell"}`,
		version: `{"ok":true,"op":"version","version":"` + newest + `","at_floor":true}`}
}

func opts(runner, dir string, f *fake) Options {
	return Options{Dir: dir, Runner: runner, Node: "node", Npm: "npm",
		PackageJSON: []byte(setupPkg), Floor: floorBuild, Run: f.run}
}

func TestANewerReleaseIsStagedVerifiedSwappedInAndRecorded(t *testing.T) {
	runner, dir := box(t, olderBuild)
	f := newFake(t)
	manifest := filepath.Join(t.TempDir(), "installed.json")
	// a schema-1 manifest: other keys stay, the flat hyperframes entry becomes the record
	write(t, manifest, `{"profile":"p","components":{"hyperframes":"`+olderBuild+`","llama":"x"}}`)
	o := opts(runner, dir, f)
	o.ManifestPath = manifest
	o.Now = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
	res, err := Install(context.Background(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{"npm view hyperframes dist-tags.latest", "npm install --ignore-scripts --no-audit --no-fund", "npm audit signatures", "npm rebuild esbuild", "runner browser", "runner version"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	staging := filepath.Join(dir, stagingDirName)
	if f.cwds[1] != staging || f.cwds[2] != staging {
		t.Errorf("npm install and the signature audit must run in the staging dir, ran in %q", f.cwds[1:3])
	}
	if f.cwds[3] != dir {
		t.Errorf("npm rebuild esbuild runs on the swapped-in tree, ran in %q", f.cwds[3])
	}
	if !res.Reinstalled || res.Version != newest || res.BrowserPath != "/b/chrome-headless-shell" {
		t.Fatalf("result = %+v", res)
	}
	if v, _ := InstalledVersion(dir); v != newest {
		t.Errorf("live install = %q, want %s", v, newest)
	}
	if !exists(filepath.Join(dir, prevDirName, "marker-"+olderBuild)) || res.PreviousTree != filepath.Join(dir, prevDirName) {
		t.Errorf("the replaced tree must be kept for a rollback at %s (result says %q)", prevDirName, res.PreviousTree)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "package.json.prev")); string(b) != oldPkg {
		t.Errorf("the replaced package.json must be kept beside it, got %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) != setupPkg {
		t.Errorf("package.json was not written from the embedded copy: %q", b)
	}
	if exists(filepath.Join(dir, "package-lock.json")) {
		t.Error("no lockfile may be left in the install dir: a lock is a pin set")
	}
	if exists(staging) {
		t.Error("the staging dir must be gone after a swap")
	}
	if err := Check(runner, dir); err != nil {
		t.Errorf("after the install the route must not read as missing: %v", err)
	}
	var doc struct {
		Profile    string `json:"profile"`
		Components map[string]json.RawMessage
	}
	b, _ := os.ReadFile(manifest)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var hf struct {
		Current struct {
			ID          string `json:"id"`
			Version     string `json:"version"`
			InstalledAt string `json:"installed_at"`
		} `json:"current"`
	}
	if err := json.Unmarshal(doc.Components["hyperframes"], &hf); err != nil {
		t.Fatalf("components.hyperframes is not a record: %s", doc.Components["hyperframes"])
	}
	if doc.Profile != "p" || string(doc.Components["llama"]) != `"x"` ||
		hf.Current.ID != newest || hf.Current.Version != newest || hf.Current.InstalledAt != "2026-10-28T12:00:00Z" {
		t.Errorf("installed.json = %s", b)
	}
}

func TestAnInstallAtTheNewestIsKeptAndReverified(t *testing.T) {
	runner, dir := box(t, newest)
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{"npm view hyperframes dist-tags.latest", "npm ls --all", "npm audit signatures", "npm rebuild esbuild", "runner browser", "runner version"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	if res.Reinstalled || res.PreviousTree != "" || exists(filepath.Join(dir, prevDirName)) {
		t.Fatalf("a healthy install at the newest must be kept as it is: %+v", res)
	}
}

func TestANewerHandPlacedBuildIsNeverDowngraded(t *testing.T) {
	runner, dir := box(t, "99.0.0-fixture")
	write(t, filepath.Join(dir, "package.json"), setupPkg)
	f := newFake(t) // the registry's latest is older than what is installed
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Reinstalled || strings.Contains(strings.Join(f.calls, "|"), "npm install") {
		t.Fatalf("an install newer than the registry's latest must be kept: %q", f.calls)
	}
}

func TestAnUnreachableRegistryKeepsAHealthyInstall(t *testing.T) {
	runner, dir := box(t, newest)
	f := newFake(t)
	f.failOn = "npm view"
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Reinstalled || strings.Contains(strings.Join(f.calls, "|"), "npm install") {
		t.Fatalf("a healthy install must survive an unreachable registry: %q", f.calls)
	}
}

func TestAnInstallMissingADependencyIsReinstalled(t *testing.T) {
	runner, dir := box(t, newest)
	f := newFake(t)
	f.lsFails = true
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Reinstalled || !strings.Contains(strings.Join(f.calls, "|"), "npm install") {
		t.Fatalf("npm ls failing means the tree is broken and must be rebuilt: %q", f.calls)
	}
}

func TestAnInstallMissingTheCLIEntryIsReinstalled(t *testing.T) {
	runner, dir := box(t, newest)
	if err := os.Remove(filepath.Join(dir, "node_modules", "hyperframes", "bin", "hyperframes.mjs")); err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Reinstalled || !strings.HasPrefix(f.calls[1], "npm install") {
		t.Fatalf("a missing CLI entry must trigger a rebuild before anything else: %q", f.calls)
	}
}

func TestInstallRefusesADirectoryThatIsNotAHarnessInstall(t *testing.T) {
	runner, _ := box(t, "")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{"name":"someone-elses-app"}`)
	write(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "")
	f := newFake(t)
	_, err := Install(context.Background(), opts(runner, dir, f))
	if err == nil || !strings.Contains(err.Error(), "not a harness HyperFrames install") {
		t.Fatalf("a foreign directory must be refused, got %v", err)
	}
	if len(f.calls) != 0 || !exists(filepath.Join(dir, "node_modules", "left-pad", "index.js")) {
		t.Fatalf("nothing may run or be touched: %q", f.calls)
	}
	nonEmpty := t.TempDir()
	write(t, filepath.Join(nonEmpty, "notes.txt"), "x")
	if _, err := Install(context.Background(), opts(runner, nonEmpty, newFake(t))); err == nil {
		t.Fatal("a non-empty directory without a package.json must be refused")
	}
}

func TestAFailedStagingStepLeavesTheLiveInstallUntouched(t *testing.T) {
	for _, tc := range []struct {
		name, failOn, resolves, want string
	}{
		{"npm install fails", "npm install", newest, "the live install is unchanged"},
		{"signature audit fails", "npm audit", newest, "refusing to swap it in, the live install is unchanged"},
		{"npm resolves below the floor", "", "0.0.1-fixture", "below the floor " + floorBuild},
	} {
		runner, dir := box(t, olderBuild)
		f := newFake(t)
		f.failOn, f.resolves = tc.failOn, tc.resolves
		_, err := Install(context.Background(), opts(runner, dir, f))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if v, _ := InstalledVersion(dir); v != olderBuild || !exists(filepath.Join(dir, "node_modules", "marker-"+olderBuild)) {
			t.Errorf("%s: the live tree must be exactly as it was, now %q", tc.name, v)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) != oldPkg {
			t.Errorf("%s: the live package.json must be untouched, got %q", tc.name, b)
		}
		if exists(filepath.Join(dir, stagingDirName)) || exists(filepath.Join(dir, prevDirName)) {
			t.Errorf("%s: staging must be cleaned up and no rollback copy made", tc.name)
		}
		if strings.Contains(strings.Join(f.calls, "|"), "runner") {
			t.Errorf("%s: the runner must not run after a failed stage: %q", tc.name, f.calls)
		}
	}
}

func TestAFailedCheckAfterTheSwapPutsThePreviousInstallBack(t *testing.T) {
	for name, mut := range map[string]func(*fake){
		"rebuild fails":           func(f *fake) { f.failOn = "npm rebuild" },
		"browser ensure fails":    func(f *fake) { f.browser = `{"ok":false,"class":"BROWSER_MISSING","detail":"download refused"}` },
		"version is under floor":  func(f *fake) { f.version = `{"ok":true,"op":"version","version":"0.0.1","at_floor":false}` },
		"version reports failure": func(f *fake) { f.version = `{"ok":false,"class":"CLI_MISSING","detail":"the CLI reports a build below the floor"}` },
	} {
		runner, dir := box(t, olderBuild)
		f := newFake(t)
		mut(f)
		manifest := filepath.Join(t.TempDir(), "installed.json")
		o := opts(runner, dir, f)
		o.ManifestPath = manifest
		res, err := Install(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "the previous install was put back") {
			t.Errorf("%s: err = %v, want the rollback named", name, err)
		}
		if res.Reinstalled || res.Version != "" {
			t.Errorf("%s: a failed install must not report success: %+v", name, res)
		}
		if exists(manifest) {
			t.Errorf("%s: a failed install must not be recorded", name)
		}
		if v, _ := InstalledVersion(dir); v != olderBuild || !exists(filepath.Join(dir, "node_modules", "marker-"+olderBuild)) {
			t.Errorf("%s: the previous tree must be live again, now %q", name, v)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) != oldPkg {
			t.Errorf("%s: the previous package.json must be back, got %q", name, b)
		}
	}
}

func TestTheRunnerAlwaysGetsTheAbsoluteInstallDir(t *testing.T) {
	runner, dir := box(t, olderBuild)
	t.Chdir(filepath.Dir(dir))
	f := newFake(t)
	o := opts(runner, filepath.Base(dir), f) // a relative --dir
	res, err := Install(context.Background(), o)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	abs, _ := filepath.Abs(filepath.Base(dir))
	if res.Dir != abs {
		t.Errorf("result dir = %q, want %q", res.Dir, abs)
	}
	for i, call := range f.calls {
		if !strings.HasPrefix(call, "runner ") {
			continue
		}
		args := f.argv[i]
		if len(args) != 4 || args[0] != runner || args[2] != "--hyperframes-dir" || args[3] != abs || f.cwds[i] != abs {
			t.Errorf("%s: argv %q in %q, want [%s %s --hyperframes-dir %s] in the same absolute dir", call, args, f.cwds[i], runner, args[1], abs)
		}
	}
}

// TestCheck: the CLI must be present and at or above the floor beside the runner; any newer
// release passes, and a tree without a floors file enforces presence only.
func TestCheck(t *testing.T) {
	runner, dir := box(t, newest)
	if err := Check(runner, dir); err != nil {
		t.Errorf("an install above the floor: %v", err)
	}
	runner, dir = box(t, "0.9.9")
	if err := Check(runner, dir); err == nil || !strings.Contains(err.Error(), "holds hyperframes 0.9.9, below the floor "+floorBuild) ||
		!strings.Contains(err.Error(), "local-offload install hyperframes") {
		t.Errorf("an install under the floor must name both versions and the fix: %v", err)
	}
	runner, dir = box(t, "")
	if err := Check(runner, dir); err == nil || !strings.Contains(err.Error(), "cannot read the installed HyperFrames version") {
		t.Errorf("an absent CLI is refused (the runner refuses it too): %v", err)
	}
	runner, dir = box(t, "0.0.1")
	if err := os.Remove(filepath.Join(filepath.Dir(runner), "..", "setup", "floors.json")); err != nil {
		t.Fatal(err)
	}
	if err := Check(runner, dir); err != nil {
		t.Errorf("without a floors file only presence is enforced: %v", err)
	}
}

func TestLastJSONLineTakesTheLastObject(t *testing.T) {
	if got := lastJSONLine("{\"progress\":1}\nCOMPOSE-FAIL: X: y\r\n{\"ok\":false}\r\n\r\n"); got != `{"ok":false}` {
		t.Errorf("got %q", got)
	}
	if got := lastJSONLine("no json here"); got != "" {
		t.Errorf("got %q", got)
	}
}

// TestHelperProcess is not a test: execRun's test runs the test binary itself as the child.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HFINSTALL_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, `{"ok":false,"class":"CLI_MISSING","detail":"from stdout"}`)
	fmt.Fprintln(os.Stderr, `{"thrown":"an object dump on stderr"}`)
	os.Exit(1)
}

func TestExecRunParsesStdoutAndCarriesStderrInTheError(t *testing.T) {
	env := append(os.Environ(), "HFINSTALL_HELPER=1")
	out, err := execRun(context.Background(), t.TempDir(), env, os.Args[0], "-test.run=^TestHelperProcess$")
	if err == nil || !strings.Contains(err.Error(), "an object dump on stderr") {
		t.Fatalf("the error must carry stderr, got %v", err)
	}
	if strings.Contains(out, "thrown") {
		t.Fatalf("stdout must not carry stderr, got %q", out)
	}
	var v struct {
		OK    bool   `json:"ok"`
		Class string `json:"class"`
	}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
		return execRun(ctx, dir, env, os.Args[0], "-test.run=^TestHelperProcess$")
	}
	if err := runnerOp(context.Background(), run, env, "node", "r.mjs", t.TempDir(), "version", &v); err != nil {
		t.Fatalf("runnerOp: %v", err)
	}
	if v.OK || v.Class != "CLI_MISSING" {
		t.Fatalf("the runner's typed failure must survive a stderr line that starts with {: %+v", v)
	}
}

func TestEnvWithNodeFirst(t *testing.T) {
	if got := envWithNodeFirst("node"); strings.Join(got, "\n") != strings.Join(os.Environ(), "\n") {
		t.Error("a bare node must leave the environment alone")
	}
	node := filepath.Join(t.TempDir(), "nodejs", "node")
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	var path string
	for _, kv := range envWithNodeFirst(node) {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			path = v
		}
	}
	if !strings.HasPrefix(path, filepath.Dir(node)+string(os.PathListSeparator)) {
		t.Fatalf("PATH must start with the configured node's directory, got %q", path)
	}
}
