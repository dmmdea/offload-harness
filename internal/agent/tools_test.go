package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// jsonStr JSON-encodes s (handles Windows backslashes safely for embedding in a JSON arg).
func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func TestReadFileWithinRootOK(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := ReadOnlyTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	rf := findTool(tools, "read_file")
	if rf == nil {
		t.Fatal("read_file tool missing")
	}
	out, err := rf.Exec(context.Background(), `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("read_file content = %q", out)
	}
}

func TestReadFileRejectsParentEscape(t *testing.T) {
	root := t.TempDir()
	// a secret one level above the worktree root
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	out, err := rf.Exec(context.Background(), `{"path":"../secret.txt"}`)
	if err == nil {
		t.Fatalf("expected escape rejection, got output %q", out)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Fatal("SECURITY: read_file leaked a file outside the worktree root")
	}
}

func TestReadFileRejectsAbsoluteOutside(t *testing.T) {
	root := t.TempDir()
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	abs := filepath.Join(filepath.Dir(root), "secret.txt")
	_, err := rf.Exec(context.Background(), `{"path":`+jsonStr(abs)+`}`)
	if err == nil {
		t.Fatal("expected absolute-path-outside-root rejection")
	}
}

// REGRESSION (fresh-context review proved this live): a non-privileged Windows
// directory junction inside the root must NOT read files outside the root.
// Go's EvalSymlinks doesn't traverse junctions; os.Root does and fails closed.
func TestReadFileRejectsWindowsJunctionEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junction escape is a Windows-specific vector")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	jx := filepath.Join(root, "jx")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", jx, parent).CombinedOutput(); err != nil {
		t.Skipf("could not create junction (mklink /J): %v — %s", err, out)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	got, err := rf.Exec(context.Background(), `{"path":"jx\\secret.txt"}`)
	if err == nil {
		t.Fatalf("SECURITY: junction escape NOT rejected, read %q", got)
	}
	if strings.Contains(got, "TOPSECRET") {
		t.Fatal("SECURITY: read_file leaked a file outside the root via a junction")
	}
}

// A symlinked path out of the root must be rejected (portable; skips where the
// OS/user can't create symlinks).
func TestReadFileRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("SEKRET"), 0o644)
	if err := os.Symlink(parent, filepath.Join(root, "ln")); err != nil {
		t.Skipf("symlink creation unsupported here: %v", err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	got, err := rf.Exec(context.Background(), `{"path":"ln/secret.txt"}`)
	if err == nil || strings.Contains(got, "SEKRET") {
		t.Fatalf("SECURITY: symlink escape not rejected (got=%q err=%v)", got, err)
	}
}

// Drive-relative paths ("C:secret.txt") are NOT absolute per filepath.IsAbs on
// Windows; they must still be rejected (volume-qualified).
func TestPathRejectsDriveRelativeWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-relative paths are Windows-specific")
	}
	tools, _ := ReadOnlyTools(t.TempDir(), nil)
	rf := findTool(tools, "read_file")
	if _, err := rf.Exec(context.Background(), `{"path":"C:secret.txt"}`); err == nil {
		t.Fatal("drive-relative C:secret.txt was not rejected")
	}
}

func TestListDirWithinRoot(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	_ = os.Mkdir(filepath.Join(root, "sub"), 0o755)
	tools, _ := ReadOnlyTools(root, nil)
	ld := findTool(tools, "list_dir")
	if ld == nil {
		t.Fatal("list_dir tool missing")
	}
	out, err := ld.Exec(context.Background(), `{"path":"."}`)
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub") {
		t.Errorf("list_dir = %q", out)
	}
}

func TestListDirRejectsEscape(t *testing.T) {
	root := t.TempDir()
	tools, _ := ReadOnlyTools(root, nil)
	ld := findTool(tools, "list_dir")
	if _, err := ld.Exec(context.Background(), `{"path":"../.."}`); err == nil {
		t.Fatal("expected list_dir escape rejection")
	}
}

func TestOffloadToolInvokesOffloaderAndReturnsResult(t *testing.T) {
	var gotTask, gotInput string
	off := func(_ context.Context, task, input string, _ map[string]any) (string, error) {
		gotTask, gotInput = task, input
		return `{"summary":"ok"}`, nil
	}
	tools, _ := ReadOnlyTools(t.TempDir(), off)
	st := findTool(tools, "offload_summarize")
	if st == nil {
		t.Fatal("offload_summarize tool missing when offloader provided")
	}
	out, err := st.Exec(context.Background(), `{"text":"a long doc"}`)
	if err != nil {
		t.Fatalf("offload_summarize: %v", err)
	}
	if gotTask != "summarize" || gotInput != "a long doc" {
		t.Errorf("offloader called with task=%q input=%q", gotTask, gotInput)
	}
	if !strings.Contains(out, `"summary":"ok"`) {
		t.Errorf("offload result = %q", out)
	}
}

func TestOffloadToolPropagatesError(t *testing.T) {
	off := func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return "", errors.New("model down")
	}
	tools, _ := ReadOnlyTools(t.TempDir(), off)
	st := findTool(tools, "offload_summarize")
	if _, err := st.Exec(context.Background(), `{"text":"x"}`); err == nil {
		t.Fatal("expected offloader error to propagate to the tool")
	}
}

func TestNoOffloadToolsWhenOffloaderNil(t *testing.T) {
	tools, _ := ReadOnlyTools(t.TempDir(), nil)
	if findTool(tools, "offload_summarize") != nil {
		t.Error("offload tools must be absent when no offloader is wired")
	}
	// read-only file tools must still be present
	if findTool(tools, "read_file") == nil || findTool(tools, "list_dir") == nil {
		t.Error("file tools must be present regardless")
	}
}

// P0 is read-only: no write/shell/net tools may be registered.
func TestNoMutatingToolsRegistered(t *testing.T) {
	tools, _ := ReadOnlyTools(t.TempDir(), func(_ context.Context, _, _ string, _ map[string]any) (string, error) { return "", nil })
	banned := []string{"write_file", "edit_file", "shell", "exec", "run", "fetch", "http", "delete", "rm"}
	for _, tl := range tools {
		for _, b := range banned {
			if strings.Contains(strings.ToLower(tl.Name), b) {
				t.Errorf("P0 must register no mutating tool, found %q", tl.Name)
			}
		}
	}
}
