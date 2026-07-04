package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/local-offload/internal/sandbox"
)

const (
	shellTimeout   = 120 * time.Second // wall-clock cap per command (the cage's pid namespace is torn down on expiry)
	shellOutputCap = 64 * 1024         // COMBINED stdout+stderr budget returned to the model (context guard)
	shellStderrCap = 16 * 1024         // stderr's slice of that budget (errors are the most diagnostic)
)

// shellRunner runs a command inside the OS sandbox. The seam lets tests inject a
// fake — the real cage (sandbox.Run) is Linux-only and re-execs the binary into
// namespaces, so it can't run under the unit-test host.
type shellRunner func(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error)

// ShellTools builds the P4.6 run_shell tool: it executes a command inside the OS
// sandbox (no network, filesystem confined to the worktree (RW) + scratch,
// dangerous syscalls blocked, i386 ABI closed). It is registered ONLY when the
// caller has opted in AND sandbox.Available() is true; it is gated AGAIN at
// runtime by the deny→ask→allow broker, which audits every command. worktree is
// the RW working directory; scratch is a RW temp dir.
func ShellTools(pol *Policy, worktree, scratch string) []Tool {
	return []Tool{shellTool(pol, worktree, scratch, sandbox.Run)}
}

// shellTool is the internal constructor with an injectable runner (for tests).
func shellTool(pol *Policy, worktree, scratch string, run shellRunner) Tool {
	return Tool{
		ToolSpec: ToolSpec{
			Name:        "run_shell",
			Description: "Run a shell command (/bin/sh -c) inside an OS sandbox: NO network, filesystem confined to the worktree (writable) plus a scratch dir, dangerous syscalls blocked. Use it for builds, tests, and file manipulation. Returns the exit code, stdout, and stderr.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the shell command line to run"}},"required":["command"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			var in struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Command) == "" {
				return "", fmt.Errorf("run_shell requires a command")
			}
			// Broker gate — opt-in capability + an audit record of every command
			// (a.Path is the command line). defer-not-crash on a non-Allow decision.
			if d, reason := pol.Decide(Action{Kind: ActShell, Path: in.Command}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}
			ctx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			res, err := run(ctx, sandbox.Spec{
				Argv:             []string{"/bin/sh", "-c", in.Command},
				Worktree:         worktree,
				WorktreeWritable: true,
				Scratch:          scratch,
				ABIFloor:         1,
			})
			if res.Refused {
				// the CAGE refused to start (setup failure) — not a command exit.
				return fmt.Sprintf("NOT performed (cage refused): %s", strings.TrimSpace(res.Stderr)), nil
			}
			if err != nil && res.ExitCode == 0 && res.Signal == 0 {
				// the cage failed to launch the command at all (e.g. clone/exec error,
				// or the OS sandbox is unavailable on this build) — surface as data.
				return fmt.Sprintf("run_shell could not run: %v", err), nil
			}
			return formatShellResult(res), nil
		},
	}
}

// formatShellResult renders the sandbox Result as exit + stdout + stderr. The two
// streams share ONE combined budget (shellOutputCap) so a chatty command cannot
// blow the model's context: stderr takes up to shellStderrCap, stdout takes the
// remainder. Each slice is truncated rune-safely.
func formatShellResult(res sandbox.Result) string {
	errs := clipTo(res.Stderr, shellStderrCap)
	out := clipTo(res.Stdout, shellOutputCap-len(errs))
	var b strings.Builder
	fmt.Fprintf(&b, "exit=%d", res.ExitCode)
	if res.Signal != 0 {
		fmt.Fprintf(&b, " (killed by signal %d)", res.Signal)
	}
	b.WriteString("\n--- stdout ---\n")
	b.WriteString(out)
	b.WriteString("\n--- stderr ---\n")
	b.WriteString(errs)
	return b.String()
}

// clipTo rune-safely truncates s to at most n bytes, appending a marker if cut.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n…(truncated)"
}
