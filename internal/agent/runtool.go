package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// runAllowedExecutables is the base-name allowlist of programs the `run` tool may
// launch. Unlike run_shell (which runs an arbitrary /bin/sh command line, so the
// allowlist could only ever gate the interpreter — meaningless), `run` execs the
// program DIRECTLY, so this list is the REAL control: it gates the actual
// executable. Enforced at the TOOL layer, cross-platform, BEFORE any sandbox call
// (so it protects Linux too, where the cage ignores AllowedExecutables). Kept to
// the build/test/dev toolchain — extend here as new tools are needed.
var runAllowedExecutables = []string{"go", "gofmt", "python", "python3", "pytest", "npm", "node", "cargo", "git"}

// RunTools builds the C7b `run` tool: it runs an ALLOWLISTED program DIRECTLY (no
// shell) inside the OS sandbox — Argv = [command, args...], never /bin/sh -c, so
// the executable allowlist is a meaningful control. It is registered only when the
// caller opts in (--allow-run) AND sandbox.Available() is true; it is gated AGAIN
// at runtime by the deny→ask→allow broker (audited). worktree is the RW working
// directory; scratch is a RW temp dir.
func RunTools(pol *Policy, worktree, scratch string) []Tool {
	return []Tool{runTool(pol, worktree, scratch, sandbox.Run)}
}

// runTool is the internal constructor with an injectable runner (for tests): the
// real cage (sandbox.Run) can't run under the unit-test host, so tests inject a
// fake and assert the built Spec.
func runTool(pol *Policy, worktree, scratch string, run shellRunner) Tool {
	allowed := strings.Join(runAllowedExecutables, ", ")
	return Tool{
		ToolSpec: ToolSpec{
			Name: "run",
			Description: "Run an ALLOWLISTED program directly (no shell) inside an OS sandbox. Provide the program in `command` (one of: " + allowed + ") and its arguments as a `args` array — arguments are passed literally, with NO shell interpretation (no pipes, globs, redirection, or &&). Use it for builds and tests, e.g. command=\"go\" args=[\"test\",\"./...\"]. Returns the exit code, stdout, and stderr. On native Windows the command is confined by a Job Object (kill/memory/time limits) and a low-integrity token: DURING the run the worktree is temporarily set to low integrity (so a low-integrity process could write into it for that window) and reverted afterward; writes OUTSIDE the worktree are blocked, but reads and network are NOT confined on native Windows (unlike the Linux cage). A command not on the allowlist is refused without running. Off by default.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the program to run (must be on the runner allowlist)"},"args":{"type":"array","items":{"type":"string"},"description":"arguments passed literally to the program (no shell interpretation)"}},"required":["command"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			var in struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			cmd := strings.TrimSpace(in.Command)
			if cmd == "" {
				return "", fmt.Errorf("run requires a command")
			}

			// C7b PATH-BYPASS GUARD — closes the allowlist bypass where the allowlist
			// gates only the base NAME while the model-supplied path is executed
			// VERBATIM. Because the `build` profile ships both write_file and run, a
			// model (or injected content) could plant a payload named `go.exe` in the
			// worktree and exec it via a path whose base name ("go") passes the
			// allowlist. All three checks below run BEFORE the base-name allowlist and
			// are defer-not-crash (a normal refusal tool result, not a Go error).

			// (a) Reject path-bearing commands: any path separator or an absolute path.
			// This blocks `worktree\go.exe`, `./go`, `sub/go`, and absolute paths — the
			// model must give a BARE executable name, which we then resolve ourselves
			// against the trusted PATH.
			if strings.ContainsAny(cmd, `/\`) || filepath.IsAbs(cmd) {
				return `NOT run: command must be a bare executable name (e.g. "go"), not a path`, nil
			}

			// TOOL-LAYER ALLOWLIST — the PRIMARY gate, cross-platform, BEFORE any
			// sandbox call (the Linux cage ignores AllowedExecutables, so this is the
			// only allowlist that protects Linux). defer-not-crash: a non-allowlisted
			// command is a normal refusal tool result, not an error. Checked on the
			// BARE name (path forms already refused above).
			if !runAllowlisted(cmd, runAllowedExecutables) {
				return fmt.Sprintf("NOT run: %s is not on the runner allowlist (allowed: %s)", cmd, allowed), nil
			}

			// (b) Resolve the bare name against the TRUSTED system PATH. exec.LookPath
			// on Windows does NOT search the current directory, so a worktree-planted
			// binary is not resolvable via a bare name. On error → refuse.
			resolved, lookErr := exec.LookPath(cmd)
			if lookErr != nil {
				return fmt.Sprintf("NOT run: %s not found on PATH", cmd), nil
			}

			// (c) Defense in depth — refuse if the resolved executable lives INSIDE the
			// worktree (guards the edge case where the worktree is somehow on PATH).
			if inside, _ := pathInside(worktree, resolved); inside {
				return fmt.Sprintf("NOT run: resolved executable %s is inside the worktree", resolved), nil
			}

			// Broker gate — opt-in capability + an audit record of every command
			// (records the direct-exec command line). defer-not-crash on non-Allow.
			auditLine := strings.TrimSpace(cmd + " " + strings.Join(in.Args, " "))
			if d, reason := pol.Decide(Action{Kind: ActShell, Path: auditLine}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}

			ctx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			// DIRECT exec: Argv = [RESOLVED-abs-path, args...], NO /bin/sh -c. Passing
			// the LookPath-resolved absolute path (not the bare name) gives
			// CreateProcessAsUser a resolvable, trusted lpApplicationName and removes any
			// ambiguity about which binary runs. args are unchanged. The sandbox's own
			// AllowedExecutables check is defense-in-depth behind the tool-layer gate
			// (it base-names Argv[0], so the resolved path still matches the allowlist).
			argv := append([]string{resolved}, in.Args...)
			res, err := run(ctx, sandbox.Spec{
				Argv:               argv,
				Worktree:           worktree,
				WorktreeWritable:   true,
				Scratch:            scratch,
				ABIFloor:           1,
				AllowedExecutables: runAllowedExecutables,
			})
			if res.Refused {
				// the CAGE refused to start (setup failure / defense-in-depth allowlist)
				// — not a command exit.
				return fmt.Sprintf("NOT performed (cage refused): %s", strings.TrimSpace(res.Stderr)), nil
			}
			if err != nil && res.ExitCode == 0 && res.Signal == 0 {
				// the cage failed to launch the command at all (exec error, or the OS
				// sandbox is unavailable on this build) — surface as data.
				return fmt.Sprintf("run could not run: %v", err), nil
			}
			return formatShellResult(res), nil
		},
	}
}

// runAllowlisted reports whether cmd's base name (case-insensitive, trailing
// ".exe" stripped) is on list. This is the tool-layer control that makes the `run`
// allowlist meaningful — it gates the real executable, not a shell interpreter.
// It splits on BOTH path separators so it works cross-platform (a Windows-style
// "C:\\tools\\go.exe" is base-named correctly even on a Linux build, and vice
// versa), rather than relying on the OS-specific filepath.Base.
func runAllowlisted(cmd string, list []string) bool {
	base := runBaseName(cmd)
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		a = strings.TrimSuffix(a, ".exe")
		if a != "" && a == base {
			return true
		}
	}
	return false
}

// runBaseName extracts the lower-cased base name of a program path, splitting on
// both '/' and '\\' (OS-independent), and dropping a trailing ".exe".
func runBaseName(cmd string) string {
	s := cmd
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(s, ".exe")
}

// pathInside reports whether target resolves to a location inside root (the tool's
// worktree). Both are made absolute; the comparison uses filepath.Rel and rejects any
// result that escapes root (".."), so a sibling like "<root>-evil" does NOT count as
// inside. Used as C7b defense-in-depth: refuse to exec a binary that lives under the
// worktree even if the trusted PATH somehow resolved to it. An error resolving either
// path returns (false, err) — the caller treats a non-inside result as "allowed to
// continue", which is correct here because the primary guards (bare-name + LookPath)
// already blocked the worktree via PATH; this is only a belt-and-suspenders layer.
func pathInside(root, target string) (bool, error) {
	if root == "" {
		return false, nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		// Different volumes on Windows (e.g. C: vs D:) → cannot be inside.
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	// Inside iff rel does not start by climbing out of root.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}
