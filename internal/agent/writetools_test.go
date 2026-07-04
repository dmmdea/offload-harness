package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileNewFileAllowed(t *testing.T) {
	wt := t.TempDir()
	tools, err := WriteTools(wt, NewPolicy(true, nil)) // unattended
	if err != nil {
		t.Fatal(err)
	}
	wf := findTool(tools, "write_file")
	if wf == nil {
		t.Fatal("write_file missing")
	}
	out, err := wf.Exec(context.Background(), `{"path":"sub/out.txt","content":"hello world"}`)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if !strings.Contains(out, "wrote") {
		t.Errorf("result = %q, want a 'wrote' confirmation", out)
	}
	b, err := os.ReadFile(filepath.Join(wt, "sub", "out.txt"))
	if err != nil || string(b) != "hello world" {
		t.Errorf("file not written correctly: %q / %v", b, err)
	}
}

func TestWriteFileOverwriteDeniedUnattended(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := WriteTools(wt, NewPolicy(true, nil))
	wf := findTool(tools, "write_file")
	out, err := wf.Exec(context.Background(), `{"path":"a.txt","content":"HACKED"}`)
	if err != nil {
		t.Fatalf("should not error (defer-not-crash): %v", err)
	}
	if !strings.Contains(out, "NOT performed") {
		t.Errorf("overwrite must be denied unattended, got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(wt, "a.txt"))
	if string(b) != "ORIGINAL" {
		t.Fatal("SECURITY: existing file was overwritten despite a deny")
	}
}

func TestWriteFileRejectsEscape(t *testing.T) {
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("ORIG"), 0o644)
	tools, _ := WriteTools(wt, NewPolicy(true, nil))
	wf := findTool(tools, "write_file")
	if _, err := wf.Exec(context.Background(), `{"path":"../secret.txt","content":"PWNED"}`); err == nil {
		t.Fatal("SECURITY: write escaping the worktree (..) was not rejected")
	}
	if _, err := wf.Exec(context.Background(), `{"path":`+jsonStr(filepath.Join(parent, "secret.txt"))+`,"content":"PWNED"}`); err == nil {
		t.Fatal("SECURITY: absolute write outside the worktree was not rejected")
	}
	if b, _ := os.ReadFile(filepath.Join(parent, "secret.txt")); string(b) != "ORIG" {
		t.Fatal("SECURITY: a file outside the worktree was modified")
	}
}

func TestWriteFileGitDenied(t *testing.T) {
	wt := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	tools, _ := WriteTools(wt, NewPolicy(false, nil)) // even ATTENDED: .git is unconditional deny
	wf := findTool(tools, "write_file")
	out, _ := wf.Exec(context.Background(), `{"path":".git/config","content":"x"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Errorf("write under .git must be denied, got %q", out)
	}
}

// REGRESSION: the fresh-context review wrote real files into .git/hooks via case/
// trailing-dot/.. forms. Every such attempt must be denied AND leave .git empty.
func TestWriteFileGitBypassAttemptsDenied(t *testing.T) {
	wt := t.TempDir()
	hooks := filepath.Join(wt, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	tools, _ := WriteTools(wt, NewPolicy(false, nil)) // even ATTENDED, .git is unconditional deny
	wf := findTool(tools, "write_file")
	for _, path := range []string{".GIT/hooks/h1", ".git./hooks/h2", "sub/../.GIT/hooks/h3", ".Git/hooks/h4"} {
		out, err := wf.Exec(context.Background(), `{"path":`+jsonStr(path)+`,"content":"#!/bin/sh\necho pwned"}`)
		if err == nil && !strings.Contains(out, "NOT performed") {
			t.Errorf("SECURITY: .git bypass %q was not denied: %q", path, out)
		}
	}
	// .git/hooks must contain nothing the agent planted
	entries, _ := os.ReadDir(hooks)
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("SECURITY: files landed in .git/hooks via a bypass: %v", names)
	}
}

func TestDeleteFileDeniedUnattended(t *testing.T) {
	wt := t.TempDir()
	keep := filepath.Join(wt, "a.txt")
	_ = os.WriteFile(keep, []byte("keep"), 0o644)
	tools, _ := WriteTools(wt, NewPolicy(true, nil))
	df := findTool(tools, "delete_file")
	if df == nil {
		t.Fatal("delete_file missing")
	}
	out, err := df.Exec(context.Background(), `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("defer-not-crash: %v", err)
	}
	if !strings.Contains(out, "NOT performed") {
		t.Errorf("delete must be denied unattended, got %q", out)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("SECURITY: file deleted despite a deny")
	}
}

func TestWriteToolsAuditsDecisions(t *testing.T) {
	wt := t.TempDir()
	audit := filepath.Join(wt, "..audit.jsonl")
	tools, _ := WriteTools(wt, NewPolicy(true, NewAuditLog(audit)))
	wf := findTool(tools, "write_file")
	_, _ = wf.Exec(context.Background(), `{"path":"new.txt","content":"x"}`) // allow
	if b, err := os.ReadFile(audit); err != nil || !strings.Contains(string(b), `"decision":"allow"`) {
		t.Errorf("write decision not audited: %q / %v", b, err)
	}
}
