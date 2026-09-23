package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/sandbox"
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

// Every tool that decoded with `_ = json.Unmarshal`: a wrong type in one of
// its STRING fields (path, content, command, url, query, …) is refused with
// the schema-mismatch error, before any policy, filesystem or network step.
// String fields are never coerced: that is the class where a coercion could
// change content. One argument object carries a non-string for every string
// field name these tools use; the coercible numeric/bool/list fields are
// covered by TestCoercibleArgumentsAreAccepted.
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

	bad := `{"path":1,"content":1,"old_string":1,"new_string":1,"pattern":1,"plan":1,"command":1,"url":1,"query":1,"name":1,"method":1,"repo":1}`
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

// Round-2 review: strictness is only needed where content is at stake. On a
// TYPE error, ONE coercion pass driven by the target struct's field types
// accepts the shapes small models send for non-string fields — a numeric
// string for an int/float, "true"/"false" for a bool, a single string for a
// []string — and re-decodes strictly. A string field is never coerced.
func TestDecodeToolArgsCoercesNonStringFields(t *testing.T) {
	var in struct {
		Private   bool     `json:"private"`
		MaxPoints int      `json:"max_points"`
		Ratio     float64  `json:"ratio"`
		Args      []string `json:"args"`
		Path      string   `json:"path"`
	}
	err := decodeToolArgs("t", `{"private":"TRUE","max_points":"3","ratio":"1.5","args":"./...","path":"a.txt","unknown":7}`, &in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !in.Private || in.MaxPoints != 3 || in.Ratio != 1.5 || !reflect.DeepEqual(in.Args, []string{"./..."}) || in.Path != "a.txt" {
		t.Fatalf("coerced = %+v", in)
	}
	// Untagged fields match case-insensitively, as encoding/json does.
	var gh struct{ Private bool }
	if err := decodeToolArgs("t", `{"private":"false"}`, &gh); err != nil || gh.Private {
		t.Fatalf("untagged bool: %+v err=%v", gh, err)
	}
}

func TestDecodeToolArgsStillRefuses(t *testing.T) {
	type target struct {
		Path      string   `json:"path"`
		MaxPoints int      `json:"max_points"`
		Private   bool     `json:"private"`
		Args      []string `json:"args"`
	}
	for _, args := range []string{
		`{"path":5}`,            // a number where a string is declared: never coerced
		`{"path":true}`,         // a bool where a string is declared
		`{"path":["a"]}`,        // an array where a string is declared
		`{"path":{}}`,           // an object where a string is declared
		`{"max_points":"x"}`,    // a string that is not a number
		`{"max_points":"3.5"}`,  // not an integer
		`{"max_points":"null"}`, // a string "null" is not a number: no silent drop
		`{"private":"yes"}`,     // not a bool literal
		`{"args":5}`,            // a number where a list is declared
		`{"args":"a","path":5}`, // one coercible field does not excuse a string-field mismatch
	} {
		var in target
		err := decodeToolArgs("t", args, &in)
		if err == nil || !strings.Contains(err.Error(), argsMismatch) {
			t.Errorf("%s: err=%v, want the schema-mismatch refusal", args, err)
		}
	}
}

func TestCoercibleArgumentsAreAccepted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nbravo\ncharlie\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotParams map[string]any
	offload := func(_ context.Context, _ string, _ string, params map[string]any) (string, error) {
		gotParams = params
		return "summary", nil
	}
	ro, err := ReadOnlyTools(dir, offload, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := mustTool(t, ro, "read_file").Exec(context.Background(), `{"path":"a.txt","offset":"2","limit":"1"}`)
	if err != nil || !strings.Contains(out, "bravo") || strings.Contains(out, "alpha") || strings.Contains(out, "charlie") {
		t.Fatalf("read_file offset/limit as strings: out=%q err=%v", out, err)
	}
	if _, err := mustTool(t, ro, "summarize_file").Exec(context.Background(), `{"path":"a.txt","max_points":"3"}`); err != nil {
		t.Fatalf("summarize_file max_points as a string: %v", err)
	}
	if gotParams["max_points"] != 3 {
		t.Errorf("summarize_file sent params %v, want max_points 3", gotParams)
	}
}

func TestRunCoercesASingleStringArgs(t *testing.T) {
	resolved, lookErr := exec.LookPath("go")
	if lookErr != nil {
		t.Skipf("go not on PATH: %v", lookErr)
	}
	var got sandbox.Spec
	run := func(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
		got = spec
		return sandbox.Result{ExitCode: 0}, nil
	}
	rt := runTool(NewPolicy(true, nil).WithShell(true), "/wt", "/wt/.scratch", run)
	if _, err := rt.Exec(context.Background(), `{"command":"go","args":"./..."}`); err != nil {
		t.Fatalf("run with a single-string args: %v", err)
	}
	if want := []string{resolved, "./..."}; !reflect.DeepEqual(got.Argv, want) {
		t.Fatalf("Argv = %v, want %v", got.Argv, want)
	}
}
