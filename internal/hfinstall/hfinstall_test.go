package hfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const setupPkg = `{"name":"offload-harness-hyperframes","private":true,"dependencies":{"hyperframes":"0.8.114"}}`
const setupLock = `{"lockfileVersion":3,"packages":{"":{"dependencies":{"hyperframes":"0.8.114"}},"node_modules/hyperframes":{"version":"0.8.114"}}}`
const oldPkg = `{"name":"offload-harness-hyperframes","private":true,"dependencies":{"hyperframes":"0.8.108"}}`

// box lays out a runner pinning runnerPin and, when installed is set, an install of that
// version written the way an older release left it (its own package.json, a marker file).
func box(t *testing.T, runnerPin, installed string) (runner, dir string) {
	t.Helper()
	root := t.TempDir()
	runner = filepath.Join(root, "render", "compose-hyperframes.mjs")
	write(t, runner, "#!/usr/bin/env node\nexport const PINNED_VERSION = \""+runnerPin+"\";\nexport const X = 1;\n")
	dir = filepath.Join(root, "hyperframes")
	if installed != "" {
		installAt(t, dir, installed)
		pkg := oldPkg
		if installed == "0.8.114" {
			pkg = setupPkg
			write(t, filepath.Join(dir, "package-lock.json"), setupLock)
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

// fake records every command (name, args, cwd) and plays npm ci by installing the pin in
// the directory it runs in.
type fake struct {
	t        *testing.T
	calls    []string
	cwds     []string
	argv     [][]string
	failOn   string // a call prefix that fails
	lsFails  bool
	browser  string
	version  string
	pinOnNpm string
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
	case strings.HasPrefix(call, "npm ci"):
		installAt(f.t, dir, f.pinOnNpm)
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
	return &fake{t: t, pinOnNpm: "0.8.114",
		browser: `{"ok":true,"op":"browser","browser_path":"/b/chrome-headless-shell","chrome_version":"152.0.7977.30"}`,
		version: `{"ok":true,"op":"version","version":"0.8.114","pinned":"0.8.114","matches_pin":true}`}
}

func opts(runner, dir string, f *fake) Options {
	return Options{Dir: dir, Runner: runner, Node: "node", Npm: "npm",
		PackageJSON: []byte(setupPkg), PackageLock: []byte(setupLock), Run: f.run}
}

func TestAnOldPinIsStagedVerifiedThenSwappedIn(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.108")
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{"npm ci --ignore-scripts --no-audit --no-fund", "npm audit signatures", "npm rebuild esbuild", "runner browser", "runner version"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	staging := filepath.Join(dir, stagingDirName)
	if f.cwds[0] != staging || f.cwds[1] != staging {
		t.Errorf("npm ci and the signature audit must run in the staging dir, ran in %q", f.cwds[:2])
	}
	if f.cwds[2] != dir {
		t.Errorf("npm rebuild esbuild runs on the swapped-in tree, ran in %q", f.cwds[2])
	}
	if !res.Reinstalled || !res.MatchesPin || res.Version != "0.8.114" || res.BrowserPath != "/b/chrome-headless-shell" || res.ChromeVersion != "152.0.7977.30" {
		t.Fatalf("result = %+v", res)
	}
	if v, _ := InstalledVersion(dir); v != "0.8.114" {
		t.Errorf("live install = %q, want 0.8.114", v)
	}
	if !exists(filepath.Join(dir, prevDirName, "marker-0.8.108")) || res.PreviousTree != filepath.Join(dir, prevDirName) {
		t.Errorf("the replaced 0.8.108 tree must be kept for a rollback at %s (result says %q)", prevDirName, res.PreviousTree)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "package.json.prev")); string(b) != oldPkg {
		t.Errorf("the replaced package.json must be kept beside it, got %q", b)
	}
	for name, want := range map[string]string{"package.json": setupPkg, "package-lock.json": setupLock} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
			t.Errorf("%s was not written from the embedded copy: %q", name, b)
		}
	}
	if exists(staging) {
		t.Error("the staging dir must be gone after a swap")
	}
	if err := Drift(runner, dir); err != nil {
		t.Errorf("after the install the route must not read as drifted: %v", err)
	}
}

func TestAHealthyInstallIsKeptAndReverified(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.114")
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{"npm ls --all", "npm audit signatures", "npm rebuild esbuild", "runner browser", "runner version"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	if res.Reinstalled || res.PreviousTree != "" || exists(filepath.Join(dir, prevDirName)) {
		t.Fatalf("a healthy install must be kept as it is: %+v", res)
	}
}

func TestAnInstallMissingADependencyIsReinstalled(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.114")
	f := newFake(t)
	f.lsFails = true
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Reinstalled || !strings.Contains(strings.Join(f.calls, "|"), "npm ci") {
		t.Fatalf("npm ls failing means the tree is broken and must be rebuilt: %q", f.calls)
	}
}

func TestAnInstallMissingTheCLIEntryIsReinstalled(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.114")
	if err := os.Remove(filepath.Join(dir, "node_modules", "hyperframes", "bin", "hyperframes.mjs")); err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Reinstalled || !strings.HasPrefix(f.calls[0], "npm ci") {
		t.Fatalf("a missing CLI entry must trigger a rebuild before anything else: %q", f.calls)
	}
}

func TestInstallRefusesWhenTheRunnerPinsAnotherVersion(t *testing.T) {
	runner, dir := box(t, "0.8.108", "0.8.108")
	f := newFake(t)
	_, err := Install(context.Background(), opts(runner, dir, f))
	if err == nil || !strings.Contains(err.Error(), "pins hyperframes 0.8.108 but this binary carries 0.8.114") {
		t.Fatalf("a binary/render-tree mismatch must refuse up front, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may run before the refusal: %q", f.calls)
	}
	if v, _ := InstalledVersion(dir); v != "0.8.108" {
		t.Errorf("the working install must be left alone, now %q", v)
	}
	if exists(filepath.Join(dir, "package-lock.json")) || exists(filepath.Join(dir, stagingDirName)) {
		t.Error("nothing may be written before the refusal")
	}
}

func TestInstallRefusesADirectoryThatIsNotAHarnessInstall(t *testing.T) {
	runner, _ := box(t, "0.8.114", "")
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
	for _, tc := range []struct{ failOn, want string }{
		{"npm ci", "the live install is unchanged"},
		{"npm audit", "refusing to swap it in, the live install is unchanged"},
	} {
		runner, dir := box(t, "0.8.114", "0.8.108")
		f := newFake(t)
		f.failOn = tc.failOn
		_, err := Install(context.Background(), opts(runner, dir, f))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.failOn, err, tc.want)
		}
		if v, _ := InstalledVersion(dir); v != "0.8.108" || !exists(filepath.Join(dir, "node_modules", "marker-0.8.108")) {
			t.Errorf("%s: the live 0.8.108 tree must be exactly as it was, now %q", tc.failOn, v)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) != oldPkg {
			t.Errorf("%s: the live package.json must be untouched, got %q", tc.failOn, b)
		}
		if exists(filepath.Join(dir, stagingDirName)) || exists(filepath.Join(dir, prevDirName)) {
			t.Errorf("%s: staging must be cleaned up and no rollback copy made", tc.failOn)
		}
		if strings.Contains(strings.Join(f.calls, "|"), "runner") {
			t.Errorf("%s: the runner must not run after a failed stage: %q", tc.failOn, f.calls)
		}
	}
}

func TestAFailedCheckAfterTheSwapPutsThePreviousInstallBack(t *testing.T) {
	for name, mut := range map[string]func(*fake){
		"rebuild fails":           func(f *fake) { f.failOn = "npm rebuild" },
		"browser ensure fails":    func(f *fake) { f.browser = `{"ok":false,"class":"BROWSER_MISSING","detail":"download refused"}` },
		"version does not match":  func(f *fake) { f.version = `{"ok":true,"op":"version","version":"0.8.114","matches_pin":false}` },
		"version reports failure": func(f *fake) { f.version = `{"ok":false,"class":"CLI_MISSING","detail":"the CLI reports 0.8.108"}` },
	} {
		runner, dir := box(t, "0.8.114", "0.8.108")
		f := newFake(t)
		mut(f)
		res, err := Install(context.Background(), opts(runner, dir, f))
		if err == nil || !strings.Contains(err.Error(), "the previous install was put back") {
			t.Errorf("%s: err = %v, want the rollback named", name, err)
		}
		if res.MatchesPin || res.Reinstalled {
			t.Errorf("%s: a failed install must not report success: %+v", name, res)
		}
		if v, _ := InstalledVersion(dir); v != "0.8.108" || !exists(filepath.Join(dir, "node_modules", "marker-0.8.108")) {
			t.Errorf("%s: the previous tree must be live again, now %q", name, v)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) != oldPkg {
			t.Errorf("%s: the previous package.json must be back, got %q", name, b)
		}
	}
}

func TestTheRunnerAlwaysGetsTheAbsoluteInstallDir(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.108")
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

func TestDrift(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.114")
	if err := Drift(runner, dir); err != nil {
		t.Errorf("matching versions: %v", err)
	}
	runner, dir = box(t, "0.8.114", "0.8.108")
	if err := Drift(runner, dir); err == nil || !strings.Contains(err.Error(), "holds hyperframes 0.8.108, not the runner's pin 0.8.114") ||
		!strings.Contains(err.Error(), "local-offload install hyperframes") {
		t.Errorf("a drifted install must name both versions and the fix: %v", err)
	}
	runner, dir = box(t, "0.8.114", "")
	if err := Drift(runner, dir); err == nil || !strings.Contains(err.Error(), "cannot read the installed HyperFrames version") {
		t.Errorf("an unreadable install is drift (the runner refuses it too): %v", err)
	}
	write(t, runner, "// a runner without a pin line\n")
	if err := Drift(runner, dir); err != nil {
		t.Errorf("an unreadable runner pin is not drift: %v", err)
	}
}

func TestRunnerPinReadsTheShippedRunner(t *testing.T) {
	pin, err := RunnerPin(filepath.Join("..", "..", "render", "compose-hyperframes.mjs"))
	if err != nil {
		t.Fatalf("RunnerPin on the shipped runner: %v", err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "hyperframes", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := PackagePin(b); pin != want {
		t.Fatalf("runner pin %q != setup package.json pin %q", pin, want)
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

func TestPackageAndLockPins(t *testing.T) {
	if p, err := PackagePin([]byte(setupPkg)); err != nil || p != "0.8.114" {
		t.Errorf("PackagePin = %q, %v", p, err)
	}
	if p, err := LockPin([]byte(setupLock)); err != nil || p != "0.8.114" {
		t.Errorf("LockPin = %q, %v", p, err)
	}
	runner, dir := box(t, "0.8.114", "")
	o := opts(runner, dir, newFake(t))
	o.PackageLock = []byte(strings.ReplaceAll(setupLock, `"version":"0.8.114"`, `"version":"0.8.113"`))
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "embedded lock resolves") {
		t.Fatalf("a lock that disagrees with package.json must refuse, got %v", err)
	}
}
