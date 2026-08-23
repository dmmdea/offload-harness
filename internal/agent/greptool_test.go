package agent

import (
	"context"
	"encoding/json"
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
	tools, err := ReadOnlyTools(root, nil, nil)
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
	tools, _ := ReadOnlyTools(root, nil, nil)
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
	tools, _ := ReadOnlyTools(root, nil, nil)
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
	tools, _ := ReadOnlyTools(root, nil, nil)
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
	tools, _ := ReadOnlyTools(root, nil, nil)
	sf := findTool(tools, "search_files")
	out, err := sf.Exec(context.Background(), `{"pattern":"willNotBeFound12345"}`)
	if err != nil {
		t.Fatalf("no-match must not be an error, got %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no matches") {
		t.Errorf("expected a clean 'no matches' string, got %q", out)
	}
}

// A case-only miss must tell the planner HOW to retry, and that retry must actually
// work. A live ampere-6 run failed a docs lookup on every model/profile combination
// because the planner searched "rate limit" against "Rate limiting", then lengthened
// its query instead of loosening it. The hint has to land at the point of failure.
func TestSearchFilesNoMatchSuggestsCaseInsensitiveRetry(t *testing.T) {
	root := t.TempDir()
	body := "Rate limiting: the gateway allows 250 requests per minute per client.\n"
	if err := os.WriteFile(filepath.Join(root, "limits.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _ := ReadOnlyTools(root, nil, nil)
	sf := findTool(tools, "search_files")

	// The exact query a planner produced live — wrong case AND wrong word form.
	out, err := sf.Exec(context.Background(), `{"pattern":"rate limit"}`)
	if err != nil {
		t.Fatalf("no-match must not be an error, got %v", err)
	}
	if !strings.Contains(out, "(?i)rate limit") {
		t.Errorf("no-match output must name the case-insensitive retry, got %q", out)
	}

	// The suggested retry must genuinely find it — on whichever backend runs
	// (ripgrep when on PATH, else the Go walk). Both honor the (?i) inline flag.
	out, err = sf.Exec(context.Background(), `{"pattern":"(?i)rate limit"}`)
	if err != nil {
		t.Fatalf("case-insensitive retry: %v", err)
	}
	if !strings.Contains(out, "limits.md") || !strings.Contains(out, "250") {
		t.Errorf("suggested retry must find the line it was suggested for, got %q", out)
	}

	// Do not re-suggest (?i) to a pattern that ALREADY folds case — a suggestion
	// that cannot change the result burns one of the planner's MaxSameTool calls.
	// A substring test for "(?i)" gets the scoped/combined forms wrong, so each
	// spelling is pinned here.
	for _, alreadyFolding := range []string{
		`(?i)willNotBeFound12345`,
		`(?i:willNotBeFound12345)`,
		`(?is)willNotBeFound12345`,
		`nope|(?i)willNotBeFound12345`,
	} {
		args, _ := json.Marshal(map[string]string{"pattern": alreadyFolding})
		out, err := sf.Exec(context.Background(), string(args))
		if err != nil {
			t.Fatalf("no-match must not be an error for %q, got %v", alreadyFolding, err)
		}
		if strings.Contains(out, "retry with") || strings.Contains(out, "(?i)(?i") {
			t.Errorf("must not suggest a case-insensitive retry for already-folding %q, got %q",
				alreadyFolding, out)
		}
	}

	// Inverse: a literal "(?i)" inside a character class is genuinely
	// case-sensitive and MUST still get the hint.
	args, _ := json.Marshal(map[string]string{"pattern": `[(?i)]willNotBeFound12345`})
	out, err = sf.Exec(context.Background(), string(args))
	if err != nil {
		t.Fatalf("no-match must not be an error, got %v", err)
	}
	if !strings.Contains(out, "retry with") {
		t.Errorf("a case-sensitive pattern containing a literal (?i) class must still get the hint, got %q", out)
	}
}

// The schema is a hand-escaped JSON literal inside a Go raw string: one stray
// quote ships a malformed tool spec to the model while every other test stays
// green. Pin that it parses and that it still states the case-sensitivity
// contract the no-match hint depends on.
func TestSearchFilesSchemaIsValidJSONAndStatesCaseSensitivity(t *testing.T) {
	tools, err := ReadOnlyTools(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sf := findTool(tools, "search_files")
	if sf == nil {
		t.Fatal("search_files tool missing")
	}
	var schema map[string]any
	if err := json.Unmarshal(sf.Schema, &schema); err != nil {
		t.Fatalf("search_files schema is not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	pat, _ := props["pattern"].(map[string]any)
	desc, _ := pat["description"].(string)
	if !strings.Contains(desc, "(?i)") {
		t.Errorf("pattern schema must document the (?i) case-insensitive prefix, got %q", desc)
	}
	if !strings.Contains(sf.Description, "(?i)") {
		t.Errorf("tool description must document the (?i) case-insensitive prefix, got %q", sf.Description)
	}
}
