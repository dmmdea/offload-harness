package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editTool(t *testing.T, dir string, pol *Policy) Tool {
	t.Helper()
	tools, err := WriteTools(dir, pol)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Name == "edit_file" {
			return tl
		}
	}
	t.Fatal("edit_file tool not registered")
	return Tool{}
}

func TestEditFileReplacesUniqueSnippet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nfoo bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := editTool(t, dir, NewPolicy(true, nil).WithWritePosture(true, false))
	out, err := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo bar","new_string":"baz qux"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "edited") {
		t.Fatalf("got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(b) != "hello world\nbaz qux\n" {
		t.Errorf("content = %q", string(b))
	}
}

func TestEditFileRefusesNonUnique(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\nx\n"), 0o644)
	edit := editTool(t, dir, NewPolicy(true, nil).WithWritePosture(true, false))
	out, _ := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"x","new_string":"y"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Fatalf("want refusal for non-unique match, got %q", out)
	}
}

func TestEditFileRefusedWithoutOverwritePosture(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("foo\n"), 0o644)
	edit := editTool(t, dir, NewPolicy(true, nil)) // no posture → overwrite denied
	out, _ := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo","new_string":"bar"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Fatalf("want broker refusal, got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(b) != "foo\n" {
		t.Errorf("file must be untouched, got %q", string(b))
	}
}

// TestEditFileWhitespaceTolerantFallback: a weak local model often reproduces
// old_string with slightly different per-line indentation than the file. When
// the EXACT match fails but a whitespace-normalized match is UNIQUE, edit_file
// should apply the replacement (using the file's actual bytes) and REPORT that
// the whitespace-tolerant path was used.
func TestEditFileWhitespaceTolerantFallback(t *testing.T) {
	dir := t.TempDir()
	// File uses a tab + trailing space; the model supplies plain spaces / no trailing space.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("func x() {\n\treturn 1 \n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := editTool(t, dir, NewPolicy(true, nil).WithWritePosture(true, false))
	// old_string differs only in leading/trailing whitespace per line.
	out, err := edit.Exec(context.Background(), `{"path":"a.go","old_string":"func x() {\n    return 1\n}","new_string":"func x() {\n\treturn 2\n}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "edited") {
		t.Fatalf("expected an edit via whitespace-tolerant fallback, got %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "whitespace") {
		t.Errorf("result must report the whitespace-tolerant path was used, got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if !strings.Contains(string(b), "return 2") {
		t.Errorf("replacement not applied: %q", string(b))
	}
}

// TestEditFileWhitespaceFallbackRefusesAmbiguous: the fallback must apply ONLY
// when the normalized match is UNIQUE. If normalizing whitespace makes the match
// ambiguous (>1), edit_file must refuse with the standard guidance — never guess.
func TestEditFileWhitespaceFallbackRefusesAmbiguous(t *testing.T) {
	dir := t.TempDir()
	// Two lines that are identical once whitespace is normalized, and neither
	// matches old_string EXACTLY (both have extra indentation) — so the exact
	// pass finds 0 and we enter the whitespace fallback, where the normalized
	// match is ambiguous (2×) and must be refused.
	orig := "  foo bar\n\tfoo bar\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := editTool(t, dir, NewPolicy(true, nil).WithWritePosture(true, false))
	out, _ := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo bar","new_string":"baz"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Fatalf("ambiguous normalized match must be refused, got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(b) != orig {
		t.Errorf("file must be untouched on ambiguous fallback: %q", string(b))
	}
}

// TestEditFileExactMatchStaysPrimary: when the exact match is present and
// unique, edit_file must take the exact path and NOT report a whitespace
// fallback (exact stays primary).
func TestEditFileExactMatchStaysPrimary(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nfoo bar\n"), 0o644)
	edit := editTool(t, dir, NewPolicy(true, nil).WithWritePosture(true, false))
	out, err := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo bar","new_string":"baz qux"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "edited") {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(strings.ToLower(out), "whitespace") {
		t.Errorf("exact match must not report a whitespace fallback: %q", out)
	}
}

// TestEditFileWhitespaceFallbackHonorsBroker: the whitespace-tolerant path must
// still go through the policy broker. Without overwrite posture (unattended),
// even a matched fallback edit must be denied and the file left untouched.
func TestEditFileWhitespaceFallbackHonorsBroker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("\tfoo bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := editTool(t, dir, NewPolicy(true, nil)) // no posture → overwrite denied
	out, _ := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo bar","new_string":"baz"}`)
	if !strings.Contains(out, "NOT performed") {
		t.Fatalf("broker must still deny even on the whitespace path, got %q", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(b) != "\tfoo bar\n" {
		t.Errorf("file must be untouched when broker denies: %q", string(b))
	}
}

// TestEditWriteDescriptionsGuideWeakModel: descriptions must steer a weak model
// to the right tool — write_file for CREATE, edit_file for changing an EXISTING
// file with unique surrounding context.
func TestEditWriteDescriptionsGuideWeakModel(t *testing.T) {
	tools, err := WriteTools(t.TempDir(), NewPolicy(true, nil))
	if err != nil {
		t.Fatal(err)
	}
	wf, ef := findTool(tools, "write_file"), findTool(tools, "edit_file")
	if wf == nil || ef == nil {
		t.Fatal("write_file / edit_file missing")
	}
	wd := strings.ToLower(wf.Description)
	if !strings.Contains(wd, "creat") || !strings.Contains(wd, "new file") {
		t.Errorf("write_file description should emphasize CREATE a new file: %q", wf.Description)
	}
	ed := strings.ToLower(ef.Description)
	if !strings.Contains(ed, "existing") {
		t.Errorf("edit_file description should emphasize an EXISTING file: %q", ef.Description)
	}
	if !strings.Contains(ed, "surrounding context") && !strings.Contains(ed, "unique") {
		t.Errorf("edit_file description should require unique surrounding context: %q", ef.Description)
	}
}

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
