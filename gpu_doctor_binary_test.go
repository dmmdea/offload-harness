package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The marker is embedded with a terminator, so a literal the linker lays next to it cannot become part of the version it
// reads back ("0.179.0interrupted"). The audit is run on a file that has exactly that neighbour.
func TestTheBuildMarkerIsReadBackWhateverLiteralFollowsIt(t *testing.T) {
	if !strings.HasSuffix(embeddedBuildMarker, "\x00") || strings.HasSuffix(buildMarker, "\x00") {
		t.Fatalf("the embedded marker is NUL-terminated and the shown one is not: %q / %q", embeddedBuildMarker, buildMarker)
	}
	if buildMarker != gpulease.BuildMarkerPrefix+version {
		t.Fatalf("the shown marker is the prefix and the version: %q", buildMarker)
	}
	for _, next := range []string{"interrupted %s; never started", "9 lives", "-rc1", "Zeta", "+meta"} {
		path := filepath.Join(t.TempDir(), "fake.exe")
		if runtime.GOOS != "windows" {
			path = filepath.Join(t.TempDir(), "fake")
		}
		if err := os.WriteFile(path, []byte("MZ-----"+gpulease.FormatSignature+"-----"+embeddedBuildMarker+next+"-----"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := gpulease.Audit(gpulease.AuditOptions{Files: []string{path}})
		if len(r.Items) != 1 || !r.Items[0].Aware || r.Items[0].Version != version {
			t.Errorf("a literal %q laid right after the marker must not change the version read back: %+v", next, r.Items)
		}
	}
}

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
