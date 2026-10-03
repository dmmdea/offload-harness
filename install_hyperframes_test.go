package main

import (
	"bytes"
	"os"
	"path/filepath"
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
}
