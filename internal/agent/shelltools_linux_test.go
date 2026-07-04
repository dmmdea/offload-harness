//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// TestMain dispatches the OS-sandbox worker role before running tests: when this
// test binary is re-exec'd by sandbox.Run as the cage worker, it applies the cage
// and execs the command (never returning). A no-op for a normal test invocation.
func TestMain(m *testing.M) {
	sandbox.RunWorkerFromEnv()
	os.Exit(m.Run())
}

// TestRunShellInCageE2E is the model-independent live proof of the P4.6 grant: the
// run_shell tool runs a REAL command through the REAL cage (sandbox.Run) in WSL —
// it writes+reads a file in the RW worktree (proving the shell can build in place)
// and confirms it has NO network (the netns severs egress even from a real curl).
func TestRunShellInCageE2E(t *testing.T) {
	if ok, why := sandbox.Available(); !ok {
		t.Skipf("OS sandbox unavailable: %s", why)
	}
	wt := t.TempDir()
	tool := shellTool(NewPolicy(true, nil).WithShell(true), wt, filepath.Join(wt, ".scratch"), sandbox.Run)
	out, err := tool.Exec(context.Background(),
		`{"command":"echo cage-built > made.txt && cat made.txt && (curl -s --max-time 2 http://1.1.1.1/ >/dev/null 2>&1 && echo NET-OK || echo NET-BLOCKED)"}`)
	if err != nil {
		t.Fatalf("run_shell e2e: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "cage-built") {
		t.Errorf("shell should run and write+read in the RW worktree; got %q", out)
	}
	if !strings.Contains(out, "NET-BLOCKED") || strings.Contains(out, "NET-OK") {
		t.Errorf("SECURITY: a caged shell command must have NO network; got %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "made.txt")); !strings.Contains(string(b), "cage-built") {
		t.Errorf("the worktree file should persist after the caged run; got %q", b)
	}
}
