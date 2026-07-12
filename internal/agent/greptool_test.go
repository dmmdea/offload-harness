package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// (a) A unique string is found with the correct relative path and L{n} line number.
func TestSearchFilesFindsUniqueString(t *testing.T) {
	root := t.TempDir()
	body := "package main\n\nfunc main() {\n\tuniqueNeedleXYZ := 1\n\t_ = uniqueNeedleXYZ\n}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := ReadOnlyTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	sf := findTool(tools, "search_files")
	if sf == nil {
		t.Fatal("search_files tool missing")
	}
	out, err := sf.Exec(context.Background(), `{"pattern":"uniqueNeedleXYZ"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected file path in output, got %q", out)
	}
	// the needle is defined on line 4
	if !strings.Contains(out, "L4:") {
		t.Errorf("expected line number L4 in output, got %q", out)
	}
	if !strings.Contains(out, "uniqueNeedleXYZ") {
		t.Errorf("expected matched line text in output, got %q", out)
	}
}

// (b) More than 100 matches are capped at exactly 100 with the "more matches" marker.
func TestSearchFilesCapsAt100(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&b, "hit line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	sf := findTool(tools, "search_files")
	out, err := sf.Exec(context.Background(), `{"pattern":"hit line"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	got := strings.Count(out, "L") // count L{n}: match lines; use a tighter check below
	_ = got
	// Count actual match lines rendered ("  L{n}: ").
	matchLines := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "L") && strings.Contains(ln, ":") {
			matchLines++
		}
	}
	if matchLines != 100 {
		t.Errorf("expected exactly 100 match lines, got %d\n%s", matchLines, out)
	}
	if !strings.Contains(out, "more matches available") {
		t.Errorf("expected the 'more matches available' marker when cap hit, got %q", out)
	}
}

// (c) A glob filter of *.go excludes a matching .txt file.
func TestSearchFilesGlobExcludesNonGo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.go"), []byte("var target = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip.txt"), []byte("var target = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	sf := findTool(tools, "search_files")
	out, err := sf.Exec(context.Background(), `{"pattern":"target","glob":"*.go"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	if !strings.Contains(out, "keep.go") {
		t.Errorf("expected keep.go in output, got %q", out)
	}
	if strings.Contains(out, "skip.txt") {
		t.Errorf("glob *.go should have excluded skip.txt, got %q", out)
	}
}

// (d) A path-escape attempt ("../") is rejected via the scope/os.Root confinement.
func TestSearchFilesRejectsEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// a secret one level above the worktree root
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	sf := findTool(tools, "search_files")
	out, err := sf.Exec(context.Background(), `{"pattern":"TOPSECRET","path":"../"}`)
	if err == nil {
		t.Fatalf("expected path-escape rejection, got output %q", out)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Fatal("SECURITY: search_files leaked content outside the worktree root")
	}
}

// (e) No match returns a clean string, not an error.
func TestSearchFilesNoMatchIsCleanString(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil)
	sf := findTool(tools, "search_files")
	out, err := sf.Exec(context.Background(), `{"pattern":"willNotBeFound12345"}`)
	if err != nil {
		t.Fatalf("no-match must not be an error, got %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no matches") {
		t.Errorf("expected a clean 'no matches' string, got %q", out)
	}
}
