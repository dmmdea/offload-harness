package hfinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const setupPkg = `{"name":"x","private":true,"dependencies":{"hyperframes":"0.8.114"}}`
const setupLock = `{"lockfileVersion":3}`

// box lays out a runner pinning runnerPin and an (optional) install holding installed.
func box(t *testing.T, runnerPin, installed string) (runner, dir string) {
	t.Helper()
	root := t.TempDir()
	runner = filepath.Join(root, "render", "compose-hyperframes.mjs")
	write(t, runner, "#!/usr/bin/env node\nexport const PINNED_VERSION = \""+runnerPin+"\";\nexport const X = 1;\n")
	dir = filepath.Join(root, "hyperframes")
	if installed != "" {
		installAt(t, dir, installed)
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

// fake records every command and plays npm ci by installing the pin.
type fake struct {
	t        *testing.T
	calls    []string
	failOn   string // a call prefix that fails
	browser  string // the browser op's stdout
	version  string // the version op's stdout
	pinOnNpm string
}

func (f *fake) run(_ context.Context, dir, name string, args ...string) (string, error) {
	call := filepath.Base(name) + " " + strings.Join(args, " ")
	if strings.HasSuffix(name, ".mjs") || strings.Contains(strings.Join(args, " "), "--hyperframes-dir") {
		call = "runner " + args[1]
	}
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return "npm ERR! simulated", errors.New("exit status 1")
	}
	switch {
	case strings.HasPrefix(call, "npm ci"):
		installAt(f.t, dir, f.pinOnNpm)
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

func TestInstallFromAnOldPinRunsEveryStepInOrder(t *testing.T) {
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
	if !res.Reinstalled || !res.MatchesPin || res.Version != "0.8.114" || res.BrowserPath != "/b/chrome-headless-shell" || res.ChromeVersion != "152.0.7977.30" {
		t.Fatalf("result = %+v", res)
	}
	for name, want := range map[string]string{"package.json": setupPkg, "package-lock.json": setupLock} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
			t.Errorf("%s was not written from the embedded copy: %q", name, b)
		}
	}
	if err := Drift(runner, dir); err != nil {
		t.Errorf("after the install the route must not read as drifted: %v", err)
	}
}

func TestInstallAtThePinSkipsNpmCiButStillVerifies(t *testing.T) {
	runner, dir := box(t, "0.8.114", "0.8.114")
	f := newFake(t)
	res, err := Install(context.Background(), opts(runner, dir, f))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Reinstalled || strings.Contains(strings.Join(f.calls, "|"), "npm ci") {
		t.Fatalf("an install already at the pin must not be replaced: %q", f.calls)
	}
	if f.calls[0] != "npm audit signatures" {
		t.Errorf("the signature audit runs on every install, got %q", f.calls)
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
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err == nil {
		t.Error("the lock must not be written before the refusal")
	}
}

func TestInstallFailsClosedOnEveryStep(t *testing.T) {
	for _, tc := range []struct{ failOn, want string }{
		{"npm ci", "npm ci (hyperframes 0.8.114) failed"},
		{"npm audit", "refusing to bind the compose lane"},
		{"npm rebuild", "npm rebuild esbuild failed"},
	} {
		runner, dir := box(t, "0.8.114", "")
		f := newFake(t)
		f.failOn = tc.failOn
		_, err := Install(context.Background(), opts(runner, dir, f))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.failOn, err, tc.want)
		}
		if strings.Contains(strings.Join(f.calls, "|"), "runner version") {
			t.Errorf("%s: a failed step must stop the install before the runner ops: %q", tc.failOn, f.calls)
		}
	}
}

func TestInstallSurfacesTheRunnersTypedFailures(t *testing.T) {
	runner, dir := box(t, "0.8.114", "")
	f := newFake(t)
	f.browser = `{"ok":false,"class":"BROWSER_MISSING","detail":"download refused"}`
	if _, err := Install(context.Background(), opts(runner, dir, f)); err == nil || !strings.Contains(err.Error(), "BROWSER_MISSING: download refused") {
		t.Fatalf("browser failure: %v", err)
	}
	runner, dir = box(t, "0.8.114", "")
	f = newFake(t)
	f.version = `{"ok":false,"class":"CLI_MISSING","detail":"the CLI reports 0.8.108"}`
	if _, err := Install(context.Background(), opts(runner, dir, f)); err == nil || !strings.Contains(err.Error(), "CLI_MISSING") {
		t.Fatalf("version failure: %v", err)
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

func TestLastJSONLine(t *testing.T) {
	if got := lastJSONLine("COMPOSE-FAIL: X: y\r\n{\"ok\":false}\r\n\r\n"); got != `{"ok":false}` {
		t.Errorf("got %q", got)
	}
	if got := lastJSONLine("no json here"); got != "" {
		t.Errorf("got %q", got)
	}
}
