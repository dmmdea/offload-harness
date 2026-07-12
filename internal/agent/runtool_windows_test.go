//go:build windows

package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// TestRunToolEndToEndWindows drives the REAL Windows sandbox (not the injected
// fake): an allowlisted `go version` runs directly (no shell) inside the Job
// Object + low-integrity token and returns its output with exit 0. This proves the
// tool-layer allowlist, the direct-exec Argv, and the Windows cage integrate
// end-to-end on this box.
func TestRunToolEndToEndWindows(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not on PATH: %v", err)
	}
	wt := t.TempDir()
	scratch := wt + `\.scratch`

	rt := runTool(NewPolicy(true, nil).WithShell(true), wt, scratch, sandbox.Run)
	// Pass the BARE name "go": path-bearing commands are now refused (C7b), so the
	// tool must resolve "go" against the trusted PATH via exec.LookPath and pass the
	// resolved absolute path to CreateProcessAsUser. Asserts exit 0 with output.
	out, execErr := rt.Exec(context.Background(), `{"command":"go","args":["version"]}`)
	if execErr != nil {
		t.Fatalf("run exec error: %v", execErr)
	}
	if strings.Contains(out, "NOT run") || strings.Contains(out, "NOT performed") {
		t.Fatalf("allowlisted `go version` should have run; got %q", out)
	}
	if !strings.Contains(out, "exit=0") {
		t.Errorf("`go version` should exit 0; got %q", out)
	}
	if !strings.Contains(out, "go version") {
		t.Errorf("stdout should carry `go version` output; got %q", out)
	}
}
