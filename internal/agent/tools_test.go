package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
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

// (a) offset=10, limit=5 returns exactly those 5 lines with correct line numbers
// (starting at 10) plus the continuation hint pointing to the next line (Z=15).
func TestReadFileRangedOffsetLimit(t *testing.T) {
	root := t.TempDir()
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i+1)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	out, err := rf.Exec(context.Background(), `{"path":"f.txt","offset":10,"limit":5}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	want := "10: line10\n11: line11\n12: line12\n13: line13\n14: line14"
	if !strings.Contains(out, want) {
		t.Errorf("expected cat -n block %q in output, got:\n%s", want, out)
	}
	// exactly those 5 lines: line9 (before window) and line15 (after window) absent
	if strings.Contains(out, "line9\n") || strings.Contains(out, "9: line9") {
		t.Errorf("line before window leaked, got:\n%s", out)
	}
	if strings.Contains(out, "15: line15") {
		t.Errorf("line after window leaked, got:\n%s", out)
	}
	// continuation hint: X=10, Y=14, TOTAL=40, Z=15
	wantHint := "(showing lines 10–14 of 40; use offset=15 to continue)"
	if !strings.Contains(out, wantHint) {
		t.Errorf("expected continuation hint %q, got:\n%s", wantHint, out)
	}
}

// (b) reading past EOF (offset beyond the last line) returns the end-of-file marker.
func TestReadFilePastEOF(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("a\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	out, err := rf.Exec(context.Background(), `{"path":"f.txt","offset":100}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if out != "(end of file — 3 lines)" {
		t.Errorf("past-EOF marker = %q, want %q", out, "(end of file — 3 lines)")
	}
}

// (c) default (no offset/limit) reads from line 1 and numbers from 1; a small
// file within the 2000-line window emits no continuation hint.
func TestReadFileDefaultFromLineOne(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("first\nsecond\nthird"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	out, err := rf.Exec(context.Background(), `{"path":"f.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	want := "1: first\n2: second\n3: third"
	if !strings.Contains(out, want) {
		t.Errorf("default read expected %q, got:\n%s", want, out)
	}
	if strings.Contains(out, "showing lines") {
		t.Errorf("no continuation hint expected for a fully-read small file, got:\n%s", out)
	}
}

// (d) a line longer than 2000 chars gets " (line truncated)" and the cut lands on
// a rune boundary (never splitting a multibyte rune).
func TestReadFileLongLineTruncatedOnRuneBoundary(t *testing.T) {
	root := t.TempDir()
	// 3000 copies of a 3-byte rune '世' — well over the 2000-char cap.
	long := strings.Repeat("世", 3000)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	rf := findTool(tools, "read_file")
	out, err := rf.Exec(context.Background(), `{"path":"f.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, " (line truncated)") {
		t.Errorf("expected \" (line truncated)\" suffix on over-long line, got prefix:\n%.80s", out)
	}
	if !utf8.ValidString(out) {
		t.Error("truncation split a multibyte rune: output is not valid UTF-8")
	}
	// The truncated content is exactly 2000 runes of '世' (each 3 bytes).
	if !strings.Contains(out, strings.Repeat("世", 2000)+" (line truncated)") {
		t.Errorf("expected 2000-rune truncation on a rune boundary, got prefix:\n%.80s", out)
	}
}

// summarize_file (C4): reads a file within the root and digests it on the
// offload cascade, so the file's bytes never enter the transcript.

// (a) a large file goes in, the (fake) offloader returns a summary, and the tool
// returns that summary — asserting the offloader received the FILE'S content.
func TestSummarizeFileReadsFileAndReturnsSummary(t *testing.T) {
	root := t.TempDir()
	// a file LARGER than the 256 KB read cap, so the byte ceiling is exercised.
	big := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 8000) // ~352 KB
	if len(big) <= maxReadBytes {
		t.Fatalf("test fixture must exceed maxReadBytes (%d) to exercise the cap; got %d", maxReadBytes, len(big))
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotTask, gotInput string
	off := func(_ context.Context, task, input string, _ map[string]any) (string, error) {
		gotTask, gotInput = task, input
		return "a lazy dog and a quick fox, repeated", nil
	}
	tools, _ := ReadOnlyTools(root, off)
	sf := findTool(tools, "summarize_file")
	if sf == nil {
		t.Fatal("summarize_file tool missing when offloader provided")
	}
	out, err := sf.Exec(context.Background(), `{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("summarize_file: %v", err)
	}
	if gotTask != "summarize" {
		t.Errorf("offloader task = %q, want summarize", gotTask)
	}
	// the offloader must have received the file's actual content, capped to maxReadBytes
	if gotInput != big[:maxReadBytes] {
		t.Errorf("offloader input did not match the (byte-capped) file content: got %d bytes, want %d", len(gotInput), maxReadBytes)
	}
	if out != "a lazy dog and a quick fox, repeated" {
		t.Errorf("summarize_file returned %q, want the offloader's summary", out)
	}
}

// (b) when the offloader errors, summarize_file must NOT return a hard error — it
// returns a graceful fallback marker telling the agent to read_file ranged instead.
func TestSummarizeFileDefersOnOffloadError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("some content"), 0o644); err != nil {
		t.Fatal(err)
	}
	off := func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return "", errors.New("model down")
	}
	tools, _ := ReadOnlyTools(root, off)
	sf := findTool(tools, "summarize_file")
	out, err := sf.Exec(context.Background(), `{"path":"doc.txt"}`)
	if err != nil {
		t.Fatalf("summarize_file must defer, not error, on offload failure; got err=%v", err)
	}
	if !strings.Contains(out, "could not summarize") || !strings.Contains(out, "doc.txt") || !strings.Contains(out, "read_file") {
		t.Errorf("expected a graceful read_file fallback marker mentioning the path and the reason, got %q", out)
	}
	if !strings.Contains(out, "model down") {
		t.Errorf("fallback marker should include the offload failure reason, got %q", out)
	}
}

// (c) a ../ escape path must be rejected via the scope/os.Root confinement and
// the offloader must NOT be called (no file content leaves the root).
func TestSummarizeFileRejectsEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	off := func(_ context.Context, _, input string, _ map[string]any) (string, error) {
		called = true
		return input, nil // echo — would leak the file if ever called
	}
	tools, _ := ReadOnlyTools(root, off)
	sf := findTool(tools, "summarize_file")
	out, err := sf.Exec(context.Background(), `{"path":"../secret.txt"}`)
	if err == nil {
		t.Fatalf("expected escape rejection, got output %q", out)
	}
	if called {
		t.Fatal("SECURITY: offloader was called on an escaping path")
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Fatal("SECURITY: summarize_file leaked a file outside the worktree root")
	}
}

// (d) when no offloader is wired, summarize_file must NOT be registered (it needs
// the offload cascade, so it hides under the same offload != nil guard).
func TestSummarizeFileAbsentWhenOffloaderNil(t *testing.T) {
	tools, _ := ReadOnlyTools(t.TempDir(), nil)
	if findTool(tools, "summarize_file") != nil {
		t.Error("summarize_file must be absent when no offloader is wired")
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
