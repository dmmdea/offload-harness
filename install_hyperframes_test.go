package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dmmdea/offload-harness/internal/hfinstall"
)

// TestEmbeddedHyperframesSetupIsTheCommittedOne: `install hyperframes` installs from the
// copy inside the binary, so that copy must be byte-for-byte the committed setup files and
// pin exactly what the runner shipped beside it enforces.
func TestEmbeddedHyperframesSetupIsTheCommittedOne(t *testing.T) {
	for name, embedded := range map[string][]byte{"package.json": hyperframesPackageJSON, "package-lock.json": hyperframesPackageLock} {
		onDisk, err := os.ReadFile(filepath.Join("setup", "hyperframes", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(onDisk, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("embedded %s differs from setup/hyperframes/%s", name, name)
		}
	}
	pin, err := hfinstall.PackagePin(hyperframesPackageJSON)
	if err != nil {
		t.Fatal(err)
	}
	runnerPin, err := hfinstall.RunnerPin(filepath.Join("render", "compose-hyperframes.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if pin != runnerPin {
		t.Fatalf("the binary would install hyperframes %s but the runner beside it pins %s", pin, runnerPin)
	}
	lockPin, err := hfinstall.LockPin(hyperframesPackageLock)
	if err != nil || lockPin != pin {
		t.Fatalf("the embedded lock resolves hyperframes %q (%v), the embedded package.json pins %s", lockPin, err, pin)
	}
}

func TestNpmBesidePrefersTheConfiguredNodesOwnNpm(t *testing.T) {
	dir := t.TempDir()
	node, npm := filepath.Join(dir, "node"), filepath.Join(dir, "npm")
	if runtime.GOOS == "windows" {
		node, npm = filepath.Join(dir, "node.exe"), filepath.Join(dir, "npm.cmd")
	}
	for _, p := range []string{node, npm} {
		if err := os.WriteFile(p, []byte(""), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := npmBeside(node); got != npm {
		t.Errorf("npmBeside(%q) = %q, want the sibling %q", node, got, npm)
	}
	if got := npmBeside("node"); got != "" {
		t.Errorf("a bare node lets PATH decide, got %q", got)
	}
}

func TestSamePath(t *testing.T) {
	a := filepath.Join(t.TempDir(), "hyperframes", "x")
	if !samePath(a, filepath.ToSlash(a)) || !samePath(a, a+string(filepath.Separator)) {
		t.Errorf("slash style and a trailing separator must not matter: %q", a)
	}
	if samePath(a, a+"y") {
		t.Error("different paths compared equal")
	}
	if runtime.GOOS == "windows" && !samePath(`C:\Users\X\hf`, `c:/users/x/HF`) {
		t.Error("Windows paths compare case-insensitively")
	}
}
