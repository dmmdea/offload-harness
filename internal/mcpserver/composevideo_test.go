package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestComposeVideoAdvertised pins offload_compose_video on tools/list: the one-input
// schema the pipeline consumes, the closed enums, and a description that states the
// class and trust posture an agent must know BEFORE calling (CPU-class, no GPU lock,
// local only, trusted compositions only, typed defers, never cloud).
func TestComposeVideoAdvertised(t *testing.T) {
	var found bool
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_compose_video" {
			continue
		}
		found = true
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		s := string(schema)
		for _, want := range []string{`"template"`, `"variables"`, `"html"`, `"project_dir"`, `"composition"`, `"out"`,
			`"format"`, `"fps"`, `"quality"`, `"resolution"`, `"workers"`, `"strict"`, `"snapshots"`,
			`"enum":["mp4","webm","mov","png-sequence","gif"]`, `"enum":["draft","standard","high"]`} {
			if !strings.Contains(s, want) {
				t.Errorf("offload_compose_video schema lacks %s: %s", want, s)
			}
		}
		if strings.Contains(s, `"required"`) {
			t.Error("the one-of-three input rule is the pipeline's to enforce; no single field is required")
		}
		for _, want := range []string{"CPU-class", "NO GPU lock", "local only", "never cloud", "TRUSTED CODE ONLY",
			"without a sandbox", "deferred:true", "BAD_INPUT", "LINT_ERRORS", "title-card", "lower-third", "measured"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("offload_compose_video description lacks %q", want)
			}
		}
	}
	if !found {
		t.Fatal("offload_compose_video not advertised on tools/list")
	}
}

// TestComposeVideoNamesEveryShippedTemplate keeps the advertised list in step with what ships. The
// description says which vetted templates exist ("shipped: ..."), and an agent chooses a template from
// that sentence, so a template that ships but is not named there is one nobody will pick. The list is
// read from render/compose-templates, the same directory the runner and the mediacap route enumerate.
func TestComposeVideoNamesEveryShippedTemplate(t *testing.T) {
	dir := filepath.Join("..", "..", "render", "compose-templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var shipped []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || strings.HasPrefix(n, "_") || strings.HasPrefix(n, ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, n, "index.html")); err == nil {
			shipped = append(shipped, n)
		}
	}
	if len(shipped) == 0 {
		t.Fatalf("no vetted templates found under %s", dir)
	}
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_compose_video" {
			continue
		}
		for _, name := range shipped {
			if !strings.Contains(tool.Description, name) {
				t.Errorf("offload_compose_video description does not name the shipped template %q (shipped: %s)", name, strings.Join(shipped, ", "))
			}
		}
		return
	}
	t.Fatal("offload_compose_video not advertised on tools/list")
}
