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
// copy inside the binary, so that copy must be byte-for-byte the committed setup files, and
// the floor the binary enforces must be the one floors.json names.
func TestEmbeddedHyperframesSetupIsTheCommittedOne(t *testing.T) {
	for rel, embedded := range map[string][]byte{
		filepath.Join("setup", "hyperframes", "package.json"): hyperframesPackageJSON,
		filepath.Join("setup", "floors.json"):                 hyperframesFloorsJSON,
	} {
		onDisk, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(onDisk, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("embedded %s differs from the committed file", rel)
		}
	}
	if floor, err := hfinstall.FloorFromJSON(hyperframesFloorsJSON); err != nil || floor == "" {
		t.Fatalf("the embedded floors.json names no hyperframes floor: %q, %v", floor, err)
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
