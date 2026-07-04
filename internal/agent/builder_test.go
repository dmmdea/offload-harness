package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func toolNames(tools []Tool) map[string]bool {
	m := map[string]bool{}
	for _, t := range tools {
		m[t.Name] = true
	}
	return m
}

func baseBuild() BuildConfig {
	return BuildConfig{PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: "."}
}

func TestBuildRequiresFields(t *testing.T) {
	cases := map[string]BuildConfig{
		"no planner":   {Model: "m", ReadRoot: "."},
		"no model":     {PlannerBase: "x", ReadRoot: "."},
		"no read root": {PlannerBase: "x", Model: "m"},
	}
	for name, cfg := range cases {
		if _, err := Build(cfg); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func TestBuildReadOnlyByDefault(t *testing.T) {
	res, err := Build(baseBuild())
	if err != nil {
		t.Fatal(err)
	}
	n := toolNames(res.Tools)
	if !n["list_dir"] || !n["read_file"] {
		t.Errorf("read tools missing: %v", n)
	}
	for _, bad := range []string{"write_file", "delete_file", "web_fetch", "run_shell"} {
		if n[bad] {
			t.Errorf("a read-only build must NOT register %q: %v", bad, n)
		}
	}
	if res.ShellGranted {
		t.Error("a read-only build must not grant shell")
	}
	if res.Loop == nil {
		t.Error("loop should be built")
	}
}

func TestBuildOffloadToolsWhenWired(t *testing.T) {
	cfg := baseBuild()
	cfg.Offload = func(context.Context, string, string, map[string]any) (string, error) { return "", nil }
	res, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !toolNames(res.Tools)["offload_summarize"] {
		t.Errorf("offload tools should be present when an offloader is wired: %v", toolNames(res.Tools))
	}
}

func TestBuildWriteAddsTools(t *testing.T) {
	wt := t.TempDir()
	cfg := baseBuild()
	cfg.ReadRoot, cfg.Worktree = wt, wt
	cfg.AllowWrite = true
	cfg.AuditPath = filepath.Join(t.TempDir(), "audit.jsonl") // OUTSIDE the worktree
	res, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n := toolNames(res.Tools)
	if !n["write_file"] || !n["delete_file"] {
		t.Errorf("write tools missing: %v", n)
	}
	if res.Worktree == "" {
		t.Error("worktree should be resolved when write is enabled")
	}
}

func TestBuildRejectsAuditInsideWorktree(t *testing.T) {
	wt := t.TempDir()
	cfg := baseBuild()
	cfg.ReadRoot, cfg.Worktree = wt, wt
	cfg.AllowWrite = true
	cfg.AuditPath = filepath.Join(wt, "audit.jsonl") // INSIDE the worktree → must reject
	if _, err := Build(cfg); err == nil || !strings.Contains(err.Error(), "inside the worktree") {
		t.Errorf("audit inside the worktree must be rejected; got err=%v", err)
	}
}

func TestBuildRejectsAskQueueInsideWorktree(t *testing.T) {
	wt := t.TempDir()
	cfg := baseBuild()
	cfg.ReadRoot, cfg.Worktree = wt, wt
	cfg.AllowWrite = true
	cfg.AuditPath = filepath.Join(t.TempDir(), "audit.jsonl") // outside (ok)
	cfg.AskQueuePath = filepath.Join(wt, "asks.jsonl")        // INSIDE → must reject
	_, err := Build(cfg)
	if err == nil || !strings.Contains(err.Error(), "ask-queue") || !strings.Contains(err.Error(), "inside the worktree") {
		t.Errorf("ask-queue inside the worktree must be rejected; got err=%v", err)
	}
}

func TestBuildFetchEmptyAllowlistNote(t *testing.T) {
	cfg := baseBuild()
	cfg.AllowFetch = true // no egress hosts => default-deny
	res, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !toolNames(res.Tools)["web_fetch"] {
		t.Error("web_fetch should be registered when fetch is enabled")
	}
	if !strings.Contains(strings.Join(res.Notes, " | "), "EMPTY") {
		t.Errorf("an empty egress allowlist should produce a note; got %v", res.Notes)
	}
}

func TestBuildShellFailClosed(t *testing.T) {
	wt := t.TempDir()
	cfg := baseBuild()
	cfg.ReadRoot, cfg.Worktree = wt, wt
	cfg.AllowShell = true
	cfg.AuditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	res, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Platform-robust: run_shell is registered IFF the OS cage was actually granted.
	if toolNames(res.Tools)["run_shell"] != res.ShellGranted {
		t.Errorf("run_shell presence (%v) must match ShellGranted (%v)", toolNames(res.Tools)["run_shell"], res.ShellGranted)
	}
	if !res.ShellGranted && !strings.Contains(strings.Join(res.Notes, " | "), "NOT granted") {
		t.Errorf("a fail-closed shell must note why it was not granted; got %v", res.Notes)
	}
}
