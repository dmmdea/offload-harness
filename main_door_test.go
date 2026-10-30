package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// mainSplitFiles are the source files the former root main.go now lives in: main.go keeps the
// subcommand dispatch and the large run* bodies, the rest are the topical files split out of it.
// A source-scanning pin that used to read main.go alone reads this set, so a declaration moving
// between them does not blind or redden it.
var mainSplitFiles = []string{
	"main.go", "cli_support.go", "task_cmd.go", "generate_image_cmd.go", "generate_video_cmd.go",
	"generate_audio_cmd.go", "delegate_cmd.go", "fleet_serve_cmd.go", "doctor_cmd.go", "report_cmd.go",
	"learn_cmd.go", "confhead_cmd.go",
}

// mainSplitSource is the text of every file in mainSplitFiles, one after the other (CRLF folded to LF).
func mainSplitSource(t testing.TB) string {
	t.Helper()
	var sb strings.Builder
	for _, name := range mainSplitFiles {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sb.WriteString(strings.ReplaceAll(string(b), "\r\n", "\n"))
		sb.WriteString("\n")
	}
	return sb.String()
}

// parseMainSplit parses every file in mainSplitFiles into one synthetic file whose declarations are
// theirs together, so a scan that looked a top-level function up in main.go finds it wherever it lives.
func parseMainSplit(t testing.TB, fset *token.FileSet) *ast.File {
	t.Helper()
	merged := &ast.File{Name: ast.NewIdent("main")}
	for _, name := range mainSplitFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		merged.Decls = append(merged.Decls, f.Decls...)
	}
	return merged
}

// Register A-102: every CLI command that runs an offload task stamps its door
// ("cli:<subcommand>") on the request, so its ledger row names the surface
// that admitted the call. The guard is a SOURCE lint for the same reason the
// MCP one is: the value of the door column is that it is total, and one new
// subcommand shipping without a door puts door-less cascade rows back in the
// ledger with nothing failing.
func TestEveryCLIRequestCarriesADoor(t *testing.T) {
	found := 0
	for _, name := range mainSplitFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read source: %v", err)
		}
		lines := strings.Split(string(src), "\n")
		for i, ln := range lines {
			if !strings.Contains(ln, "core.Request{") {
				continue
			}
			found++
			window := strings.Join(lines[i:min(i+9, len(lines))], "\n")
			if !strings.Contains(window, `Door:`) {
				t.Errorf("%s:%d builds a core.Request with no cli: Door: %s", name, i+1, strings.TrimSpace(ln))
				continue
			}
			// The door must be a CLI door — the generic text-task handler derives
			// it from the subcommand the user typed, which is the same vocabulary.
			if !strings.Contains(window, `Door: "cli:`) && !strings.Contains(window, `Door:  "cli:`) &&
				!strings.Contains(window, `Door:   "cli:`) && !strings.Contains(window, `Door: "cli:" +`) {
				t.Errorf("%s:%d stamps a door that is not a cli: door: %s", name, i+1, strings.TrimSpace(ln))
			}
		}
	}
	if found == 0 {
		t.Fatal("no core.Request literal found in main.go or its split files — the guard stopped guarding anything")
	}
}
