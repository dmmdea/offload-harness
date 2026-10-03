package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// THE LINKER MUST KEEP THE SIGNATURE. `gpu doctor` tells a binary that understands the
// lease format from one that does not by the literal FormatSignature inside it, so a
// build that dropped the literal (dead-code removal) would make every release fail its
// own audit, and one that kept it only by accident could stop doing so unnoticed. This
// builds the real product binary and audits it, exactly as the doctor will on a host.
func TestBuiltBinaryCarriesTheFormatSignatureAndBuildMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the product binary")
	}
	exe := filepath.Join(t.TempDir(), "local-offload")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	r := gpulease.Audit(gpulease.AuditOptions{Files: []string{exe}})
	if len(r.Items) != 1 || !r.Items[0].Aware {
		t.Fatalf("the built binary must carry the format signature: %+v", r)
	}
	if r.Items[0].Version != version {
		t.Fatalf("the built binary must carry its build marker %q, got %q", version, r.Items[0].Version)
	}
}
