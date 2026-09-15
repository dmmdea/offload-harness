//go:build windows

package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The Windows cage must hand its child an EXPLICIT environment, not the
// parent's.
//
// Until 0.117.7 CreateProcessAsUser was called with a nil lpEnvironment, which
// means "inherit the caller's block" — so a `run` child on Windows received the
// delegator's entire environment: GITHUB_TOKEN, MEM0_API_KEY, the fleet auth
// token, every credential a shell had exported into the harness. The Linux cage
// has always built a three-entry env by hand (sandbox_linux.go: PATH, HOME,
// TMPDIR) and the two sides simply disagreed. Reads are less contained on
// Windows than on Linux, but "less contained" was never a licence to HAND the
// child the secrets — an env block is not something the child has to go and
// find.
//
// This test is the adversarial one: it exports a canary into the PARENT and
// makes the child print its whole environment back.

// canaryVars are the secrets the parent exports and the child must never see.
// Two shapes on purpose: a name nobody would allowlist, and the real NAMES of
// credentials this repo actually handles. The VALUES are deliberately not
// credential-shaped — the repo's pre-push leak scanner reads added lines, and a
// fixture that looks like a token is a fixture that cannot be pushed. The test
// asserts on the value it set, so the shape of that value is free.
var canaryVars = map[string]string{
	"OFFLOAD_SANDBOX_CANARY": "canary-9f2b7c41-must-not-cross-the-cage",
	"GITHUB_TOKEN":           "canary-not-a-real-token-must-not-cross-the-cage",
	"MEM0_API_KEY":           "canary-not-a-real-key-must-not-cross-the-cage",
}

// childEnvDump runs `cmd /c set` inside the cage and returns its stdout: every
// variable the child actually received, one NAME=VALUE per line.
func childEnvDump(t *testing.T) (string, Spec) {
	t.Helper()
	for k, v := range canaryVars {
		t.Setenv(k, v)
	}
	wt := t.TempDir()
	spec := Spec{
		Argv:               []string{cmdExe(t), "/c", "set"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            filepath.Join(wt, ".scratch"),
		AllowedExecutables: []string{"cmd"},
	}
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("caged `set` failed: %v (stderr=%s)", err, res.Stderr)
	}
	if res.Refused {
		t.Fatalf("`set` must not be refused: %q", res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		t.Fatalf("`set` produced no output; nothing was proven (stderr=%q, exit=%d)", res.Stderr, res.ExitCode)
	}
	return res.Stdout, spec
}

// TestWindowsChildDoesNotInheritParentSecrets: a canary exported in the parent
// must be ABSENT from the child's environment — by name and by value.
func TestWindowsChildDoesNotInheritParentSecrets(t *testing.T) {
	dump, _ := childEnvDump(t)
	up := strings.ToUpper(dump)
	for name, value := range canaryVars {
		if strings.Contains(up, strings.ToUpper(name)+"=") {
			t.Errorf("the caged child inherited %s — CreateProcess must be given an explicit environment block, never nil.\nchild env:\n%s", name, dump)
		}
		if strings.Contains(dump, value) {
			t.Errorf("the caged child received the VALUE of %s", name)
		}
	}
}

// TestWindowsChildGetsTheAllowlistedEnvironment: the other direction — the cage
// must not be so tight that a process cannot start. Every allowlisted variable
// is present, and the derived ones point INTO the run's own scratch rather than
// at the operator's real profile or temp dir.
func TestWindowsChildGetsTheAllowlistedEnvironment(t *testing.T) {
	dump, spec := childEnvDump(t)
	got := map[string]string{}
	for _, line := range strings.Split(dump, "\n") {
		if name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			got[strings.ToUpper(strings.TrimSpace(name))] = value
		}
	}
	for _, name := range []string{"SYSTEMROOT", "PATH", "TEMP", "TMP", "USERPROFILE"} {
		if got[name] == "" {
			t.Errorf("the caged child is missing %s; a Windows process needs it to start.\nchild env:\n%s", name, dump)
		}
	}
	scratch := strings.ToLower(spec.Scratch)
	for _, name := range []string{"TEMP", "TMP", "USERPROFILE", "HOME"} {
		if strings.ToLower(got[name]) != scratch {
			t.Errorf("%s = %q, want the run's scratch %q — the child must not be pointed at the operator's own profile/temp", name, got[name], spec.Scratch)
		}
	}
	// The block is a CLOSED set: nothing outside the allowlist plus the derived
	// names may appear. This is what makes the test fail on a future variable
	// quietly added back by inheritance rather than by decision.
	allowed := map[string]bool{}
	for _, n := range EnvAllowlist() {
		allowed[strings.ToUpper(n)] = true
	}
	for name := range got {
		// cmd.exe sets a handful of its own (PROMPT, =ExitCode, =C:) after it
		// starts; those are the shell's, not inherited from the parent.
		if name == "" || strings.HasPrefix(name, "=") || name == "PROMPT" {
			continue
		}
		if !allowed[name] {
			t.Errorf("unexpected variable %q in the caged child's environment — the block must be the allowlist and nothing else.\nchild env:\n%s", name, dump)
		}
	}
}
