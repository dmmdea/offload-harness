package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// plantFakeExe writes a minimal executable-looking file named base (with a .exe
// suffix on Windows) into dir, so exec.LookPath will resolve it when dir is on PATH.
// Returns the absolute path LookPath is expected to return.
func plantFakeExe(t *testing.T, dir, base string) string {
	t.Helper()
	name := base
	if runtime.GOOS == "windows" {
		name = base + ".exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("MZ fake"), 0o755); err != nil {
		t.Fatalf("plant fake exe: %v", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs planted exe: %v", err)
	}
	return abs
}

// TestRunToolDeniedWhenNotEnabled: with the shell/run capability off in the broker
// the tool must refuse WITHOUT invoking the sandbox runner.
func TestRunToolDeniedWhenNotEnabled(t *testing.T) {
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{}, nil
	}
	rt := runTool(NewPolicy(true, nil) /* shell/run NOT enabled */, "/wt", "/wt/.scratch", run)
	out, _ := rt.Exec(context.Background(), `{"command":"go","args":["version"]}`)
	if !strings.Contains(out, "NOT performed") {
		t.Errorf("run must be denied when the capability is off; got %q", out)
	}
	if called {
		t.Error("SECURITY: the sandbox runner was invoked for a denied run command")
	}
}

// TestRunToolAllowlistedDispatches: an allowlisted command passes the tool-layer
// gate and is dispatched as a DIRECT exec (Argv = [RESOLVED-path, args...], NO
// /bin/sh), RW in the worktree, with the allowlist forwarded as defense-in-depth.
// C7b: Argv[0] is the exec.LookPath-resolved absolute path (not the bare name).
func TestRunToolAllowlistedDispatches(t *testing.T) {
	resolved, lookErr := exec.LookPath("go")
	if lookErr != nil {
		t.Skipf("go not on PATH: %v", lookErr)
	}
	var gotSpec sandbox.Spec
	run := func(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
		gotSpec = spec
		return sandbox.Result{Stdout: "go version go1.26.3", ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	out, err := rt.Exec(context.Background(), `{"command":"go","args":["build","./..."]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "go version") {
		t.Errorf("result should carry exit + stdout; got %q", out)
	}
	// DIRECT exec: no shell interpreter, args are separate Argv elements. Argv[0] is
	// the LookPath-resolved absolute path (C7b), args unchanged.
	want := []string{resolved, "build", "./..."}
	if len(gotSpec.Argv) != len(want) {
		t.Fatalf("spec.Argv = %v, want %v (direct exec, no /bin/sh)", gotSpec.Argv, want)
	}
	for i := range want {
		if gotSpec.Argv[i] != want[i] {
			t.Fatalf("spec.Argv = %v, want %v", gotSpec.Argv, want)
		}
	}
	if !gotSpec.WorktreeWritable || gotSpec.Worktree != "/wt" {
		t.Errorf("run should be RW in the worktree; got writable=%v worktree=%q", gotSpec.WorktreeWritable, gotSpec.Worktree)
	}
	if len(gotSpec.AllowedExecutables) == 0 {
		t.Errorf("the allowlist must be forwarded to the sandbox as defense-in-depth; got empty")
	}
}

// TestRunToolNonAllowlistedRefusedAtToolLayer: a command NOT on the allowlist is
// refused at the TOOL layer (before any sandbox call), cross-platform. The sandbox
// runner must NOT be invoked. This is the primary gate (covers Linux too).
func TestRunToolNonAllowlistedRefusedAtToolLayer(t *testing.T) {
	for _, cmd := range []string{"rm", "curl", "bash", "sh", "powershell"} {
		called := false
		run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
			called = true
			return sandbox.Result{}, nil
		}
		rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
		out, _ := rt.Exec(context.Background(), `{"command":"`+cmd+`","args":["x"]}`)
		if !strings.Contains(out, "not on the runner allowlist") {
			t.Errorf("%q must be refused at the tool layer; got %q", cmd, out)
		}
		if called {
			t.Errorf("SECURITY: sandbox runner invoked for non-allowlisted %q", cmd)
		}
	}
}

// TestRunBaseNameMatching: the base-name allowlist matcher (a lower-level unit) is
// case-insensitive and strips a trailing ".exe", splitting on both separators — so
// "GO.EXE", "C:\\path\\go.exe", "/usr/bin/python3" all match, a look-alike does not.
// NOTE: at the TOOL layer path-bearing commands are now refused as paths BEFORE this
// matcher runs (see TestRunToolPathBearingCommandRefused); this test exercises the
// matcher itself, which stays path-tolerant so it is robust wherever it is reused.
func TestRunBaseNameMatching(t *testing.T) {
	pass := []string{"go", "GO.EXE", `C:\tools\go.exe`, "/usr/bin/python3", "Node.EXE", "GoFmt"}
	for _, cmd := range pass {
		if !runAllowlisted(cmd, runAllowedExecutables) {
			t.Errorf("%q should match the base-name allowlist (case-insensitive, .exe stripped)", cmd)
		}
	}
	if runAllowlisted("gopher", runAllowedExecutables) {
		t.Errorf("SECURITY: %q is a look-alike, not on the allowlist — must not match", "gopher")
	}
}

// TestRunToolBareAllowlistedPasses: a BARE allowlisted name that is on PATH passes the
// allowlist and dispatches (the end-to-end tool-layer path, resolved via LookPath).
func TestRunToolBareAllowlistedPasses(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not on PATH: %v", err)
	}
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	rt.Exec(context.Background(), `{"command":"go"}`)
	if !called {
		t.Error("bare allowlisted `go` (on PATH) should dispatch")
	}
}

// TestRunToolPathBearingCommandRefused: a `command` that carries any path
// separator (or is absolute) must be REFUSED at the tool layer BEFORE any sandbox
// call — this is the core close of the C7b allowlist path-bypass (a model can plant
// a payload named `go.exe` in the worktree and try to exec it via a path). The
// refusal is a normal tool result (defer-not-crash), and the runner must NOT fire.
func TestRunToolPathBearingCommandRefused(t *testing.T) {
	// These carry a path separator OR are absolute — all must be refused as paths,
	// regardless of whether the base name is on the allowlist.
	pathCmds := []string{
		`./go`,             // relative, leading ./
		`sub\go.exe`,       // relative with a Windows separator
		`sub/go`,           // relative with a POSIX separator
		`.\go.exe`,         // relative with .\
		`worktree\go.exe`,  // the exact attack from the finding
		`/usr/bin/go`,      // POSIX absolute
		`C:\tools\go.exe`,  // Windows absolute (IsAbs on Windows)
		`\\server\go.exe`,  // UNC / rooted
	}
	for _, cmd := range pathCmds {
		called := false
		run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
			called = true
			return sandbox.Result{ExitCode: 0}, nil
		}
		rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
		esc := strings.ReplaceAll(cmd, `\`, `\\`)
		out, err := rt.Exec(context.Background(), `{"command":"`+esc+`","args":["version"]}`)
		if err != nil {
			t.Errorf("%q: refusal must be a normal tool result, not a Go error; got err=%v", cmd, err)
		}
		if !strings.Contains(out, "must be a bare executable name") {
			t.Errorf("%q: path-bearing command must be refused as a path; got %q", cmd, out)
		}
		if called {
			t.Errorf("SECURITY: %q reached the sandbox runner — path-bypass NOT closed", cmd)
		}
	}
}

// TestRunToolBareNameNotOnPATHRefused: a bare, allowlisted name that is NOT present
// on the trusted system PATH is refused with "not found" — this is what stops a
// worktree-planted `go.exe` reached via the BARE name `go` (exec.LookPath on Windows
// does not search the CWD, so a planted binary is not resolvable and we refuse).
// We use a bare allowlisted name that no real toolchain provides ("cargo" is on the
// allowlist but is not installed on the CI/test box) — but to be host-independent we
// temporarily point PATH at an empty dir so LookPath cannot resolve anything.
func TestRunToolBareNameNotOnPATHRefused(t *testing.T) {
	// Empty out PATH so no bare name resolves — host-independent.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	out, err := rt.Exec(context.Background(), `{"command":"go","args":["version"]}`)
	if err != nil {
		t.Errorf("refusal must be a normal tool result, not a Go error; got err=%v", err)
	}
	if !strings.Contains(out, "not found on PATH") {
		t.Errorf("bare allowlisted name absent from PATH must be refused with not-found; got %q", out)
	}
	if called {
		t.Error("SECURITY: sandbox runner invoked for a command not resolvable on PATH")
	}
}

// TestRunToolResolvedPathPassedToSandbox: an allowlisted bare name that IS on PATH is
// resolved via exec.LookPath and the RESOLVED ABSOLUTE path is what gets passed to the
// sandbox as Argv[0] (so CreateProcessAsUser gets a trusted, resolvable path), while
// args are unchanged. Uses `go` (guaranteed present in this test env).
func TestRunToolResolvedPathPassedToSandbox(t *testing.T) {
	resolved, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go not on PATH: %v", err)
	}
	var gotSpec sandbox.Spec
	run := func(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
		gotSpec = spec
		return sandbox.Result{Stdout: "go version go1.26", ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	if _, err := rt.Exec(context.Background(), `{"command":"go","args":["version"]}`); err != nil {
		t.Fatal(err)
	}
	if len(gotSpec.Argv) < 1 || gotSpec.Argv[0] != resolved {
		t.Fatalf("Argv[0] should be the LookPath-resolved absolute path %q; got %v", resolved, gotSpec.Argv)
	}
	want := []string{resolved, "version"}
	if len(gotSpec.Argv) != len(want) {
		t.Fatalf("spec.Argv = %v, want %v (resolved exe + unchanged args)", gotSpec.Argv, want)
	}
	for i := range want {
		if gotSpec.Argv[i] != want[i] {
			t.Fatalf("spec.Argv = %v, want %v", gotSpec.Argv, want)
		}
	}
}

// TestRunToolResolvedInsideWorktreeRefused: defense in depth — if the trusted PATH
// somehow resolves the bare name to a binary INSIDE the worktree, refuse. We simulate
// by planting an executable-looking `go` in a temp "worktree" and putting that dir on
// PATH; LookPath then resolves to it, and the tool must refuse (not execute).
func TestRunToolResolvedInsideWorktreeRefused(t *testing.T) {
	wt := t.TempDir()
	planted := plantFakeExe(t, wt, "go")
	// Point PATH ONLY at the worktree so LookPath resolves the planted binary.
	t.Setenv("PATH", wt)
	if got, err := exec.LookPath("go"); err != nil || got != planted {
		t.Skipf("could not stage the worktree-on-PATH scenario (LookPath=%q err=%v)", got, err)
	}
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), wt, wt+"/.scratch", run)
	out, err := rt.Exec(context.Background(), `{"command":"go","args":["version"]}`)
	if err != nil {
		t.Errorf("refusal must be a normal tool result, not a Go error; got err=%v", err)
	}
	if !strings.Contains(out, "inside the worktree") {
		t.Errorf("a bare name resolving INTO the worktree must be refused (defense in depth); got %q", out)
	}
	if called {
		t.Error("SECURITY: sandbox runner invoked for a command that resolved inside the worktree")
	}
}

// TestRunToolEmptyCommand: an empty command is an error (not a dispatch).
func TestRunToolEmptyCommand(t *testing.T) {
	called := false
	run := func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		called = true
		return sandbox.Result{}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	_, err := rt.Exec(context.Background(), `{"command":"   "}`)
	if err == nil {
		t.Errorf("empty command should error")
	}
	if called {
		t.Error("empty command must not reach the runner")
	}
}

// TestRunToolsRegistersRun: the public constructor registers exactly the `run` tool.
func TestRunToolsRegistersRun(t *testing.T) {
	tools := RunTools(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch")
	if len(tools) != 1 || tools[0].Name != "run" {
		names := make([]string, len(tools))
		for i, tl := range tools {
			names[i] = tl.Name
		}
		t.Fatalf("RunTools should register exactly [run]; got %v", names)
	}
}
