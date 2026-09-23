package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PR #462 review, item 1: every tool decoded its arguments with
// `_ = json.Unmarshal(...)`. A SYNTAX error leaves the struct zero (Unmarshal
// validates first), and the tools' required-field checks catch that. A TYPE
// error does not: Unmarshal fills every field it can and reports the first
// mismatch, so `{"path":"x","old_string":"foo","new_string":123}` reached
// edit_file with NewString "" and deleted "foo" from the file, reporting
// success. Every tool must refuse on ANY decode error, touching nothing.

// argsMismatch is the phrase decodeToolArgs puts in its refusal.
const argsMismatch = "arguments do not match the tool's schema"

func mustTool(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s not registered", name)
	return Tool{}
}

func writeToolsFor(t *testing.T, dir string) []Tool {
	t.Helper()
	tools, err := WriteTools(dir, NewPolicy(true, nil).WithWritePosture(true, true))
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func TestEditFileWithANonStringNewStringTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	orig := []byte("keep foo here\n")
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	edit := mustTool(t, writeToolsFor(t, dir), "edit_file")
	out, err := edit.Exec(context.Background(), `{"path":"a.txt","old_string":"foo","new_string":123}`)
	if err == nil {
		t.Fatalf("edit_file accepted a numeric new_string: %q", out)
	}
	if !strings.Contains(err.Error(), argsMismatch) {
		t.Errorf("error %q does not say the arguments do not match the schema", err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, orig) {
		t.Errorf("file changed on a refused call: %q, want %q", got, orig)
	}
}

func TestWriteFileWithANonStringContentWritesNothing(t *testing.T) {
	dir := t.TempDir()
	write := mustTool(t, writeToolsFor(t, dir), "write_file")
	out, err := write.Exec(context.Background(), `{"path":"new.txt","content":{"text":"hi"}}`)
	if err == nil {
		t.Fatalf("write_file accepted an object content: %q", out)
	}
	if !strings.Contains(err.Error(), argsMismatch) {
		t.Errorf("error %q does not say the arguments do not match the schema", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "new.txt")); serr == nil {
		t.Errorf("write_file created the file on a refused call")
	}
}

func TestWriteFileOverwriteWithANonStringContentLeavesTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	orig := []byte("original\n")
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	write := mustTool(t, writeToolsFor(t, dir), "write_file")
	if _, err := write.Exec(context.Background(), `{"path":"a.txt","content":42}`); err == nil {
		t.Fatalf("write_file accepted a numeric content")
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, orig) {
		t.Errorf("file changed on a refused call: %q, want %q", got, orig)
	}
}

func TestUpdatePlanWithANonStringPlanIsRefused(t *testing.T) {
	dir := t.TempDir()
	plan := updatePlanTool(dir)
	if _, err := plan.Exec(context.Background(), `{"plan":["a","b"]}`); err == nil || !strings.Contains(err.Error(), argsMismatch) {
		t.Fatalf("update_plan err = %v, want the schema-mismatch refusal", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, ".agent")); serr == nil {
		t.Errorf("update_plan touched the worktree on a refused call")
	}
}

// Every tool that decoded with `_ = json.Unmarshal`: a type mismatch on one
// of its own fields is refused with the schema-mismatch error, before any
// policy, filesystem or network step. One argument object carries a wrong
// type for every field name these tools use.
func TestEveryToolRefusesTypeMismatchedArguments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	offload := func(context.Context, string, string, map[string]any) (string, error) {
		t.Error("offload reached on a refused call")
		return "", nil
	}
	ro, err := ReadOnlyTools(dir, offload, nil)
	if err != nil {
		t.Fatal(err)
	}
	search, err := SearchTool(dir)
	if err != nil {
		t.Fatal(err)
	}
	pol := NewPolicy(true, nil).WithWritePosture(true, true)
	var all []Tool
	all = append(all, ro...)
	all = append(all, search, updatePlanTool(dir))
	all = append(all, writeToolsFor(t, dir)...)
	all = append(all, RunTools(pol, dir, t.TempDir())...)
	all = append(all, ShellTools(pol, dir, t.TempDir())...)
	all = append(all, FetchTools(pol)...)
	all = append(all, SearchTools(pol)...)
	all = append(all, GitHubTools(pol, "test-token", "o/r", dir)...)

	bad := `{"path":1,"content":1,"old_string":1,"new_string":1,"pattern":1,"plan":1,"command":1,"url":1,"query":1,"name":1,"method":1,"repo":1,"offset":"x","max_points":"x"}`
	want := []string{"list_dir", "read_file", "summarize_file", "search_files", "update_plan",
		"write_file", "delete_file", "edit_file", "run", "run_shell", "web_fetch", "web_search",
		"github_api", "github_create_repo", "github_upload_file"}
	seen := map[string]bool{}
	for _, tl := range all {
		seen[tl.Name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Logf("%s not registered on this platform; skipped", name)
			continue
		}
		tl := mustTool(t, all, name)
		out, err := tl.Exec(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), argsMismatch) {
			t.Errorf("%s: out=%q err=%v, want the schema-mismatch refusal", name, out, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "x\n" {
		t.Errorf("a refused call changed a.txt: %q", got)
	}
}

// CONTROL: empty arguments stay a zero-valued call (how a no-argument call
// arrives on some engines), so list_dir with "" still lists the root.
func TestEmptyToolArgumentsStayAZeroCall(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ro, err := ReadOnlyTools(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := mustTool(t, ro, "list_dir").Exec(context.Background(), "")
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("list_dir with empty args: out=%q err=%v", out, err)
	}
}
