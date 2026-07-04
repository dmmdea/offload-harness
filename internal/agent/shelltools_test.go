package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload/internal/sandbox"
)

func TestPolicyShellGate(t *testing.T) {
	// default: shell denied (capability off)
	for _, unattended := range []bool{false, true} {
		if d, _ := NewPolicy(unattended, nil).Decide(Action{Kind: ActShell, Path: "ls -la"}); d != Deny {
			t.Errorf("default ActShell (unattended=%v) = %q, want deny", unattended, d)
		}
	}
	// opted in: ALLOW (audited; never Ask — the OS sandbox is the containment), on
	// both attended and unattended runs.
	for _, unattended := range []bool{false, true} {
		p := NewPolicy(unattended, nil).WithShell(true)
		if d, _ := p.Decide(Action{Kind: ActShell, Path: "go test ./..."}); d != Allow {
			t.Errorf("opted-in ActShell (unattended=%v) = %q, want allow", unattended, d)
		}
	}
}

func TestShellToolDeniedWhenNotEnabled(t *testing.T) {
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{}, nil
	}
	wf := shellTool(NewPolicy(true, nil) /* shell NOT enabled */, "/wt", "/wt/.scratch", run)
	out, _ := wf.Exec(context.Background(), `{"command":"rm -rf /"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Errorf("shell must be denied when the capability is off; got %q", out)
	}
	if called {
		t.Error("SECURITY: the cage runner was invoked for a denied shell command")
	}
}

func TestShellToolRunsWhenEnabled(t *testing.T) {
	var gotSpec sandbox.Spec
	run := func(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
		gotSpec = spec
		return sandbox.Result{Stdout: "hello-out", ExitCode: 0}, nil
	}
	wf := shellTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	out, err := wf.Exec(context.Background(), `{"command":"echo hello-out"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "hello-out") {
		t.Errorf("result should carry exit + stdout; got %q", out)
	}
	if len(gotSpec.Argv) != 3 || gotSpec.Argv[0] != "/bin/sh" || gotSpec.Argv[1] != "-c" || gotSpec.Argv[2] != "echo hello-out" {
		t.Errorf("spec.Argv = %v, want [/bin/sh -c echo hello-out]", gotSpec.Argv)
	}
	if !gotSpec.WorktreeWritable || gotSpec.Worktree != "/wt" {
		t.Errorf("shell should run RW in the worktree; got writable=%v worktree=%q", gotSpec.WorktreeWritable, gotSpec.Worktree)
	}
}

func TestShellToolCageRefused(t *testing.T) {
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Result{Refused: true, Stderr: "[[SANDBOX-REFUSED]] sandbox worker: landlock floor not met"}, context.DeadlineExceeded
	}
	wf := shellTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	out, _ := wf.Exec(context.Background(), `{"command":"echo hi"}`)
	if !strings.Contains(out, "cage refused") {
		t.Errorf("a cage refusal should surface as 'cage refused'; got %q", out)
	}
}
