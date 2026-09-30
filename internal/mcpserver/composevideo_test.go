package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	shipped := shippedComposeTemplates(t)
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

// shippedComposeTemplates lists the vetted templates on disk: the directories of render/compose-templates
// that hold an index.html and do not start with "_" or ".". It is the same rule the runner and the
// mediacap route use to enumerate them.
func shippedComposeTemplates(t *testing.T) []string {
	t.Helper()
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
	sort.Strings(shipped)
	return shipped
}

// TestComposeVideoNamesNothingThatDoesNotShip is the other direction of the drift test above. The forward
// test passes when a shipped name appears anywhere in the description, so a name for a template that was
// renamed or removed would linger unseen. Here the "shipped: a, b, c;" list must be exactly the templates on
// disk, and every name in the template parameter's "e.g." example must ship.
func TestComposeVideoNamesNothingThatDoesNotShip(t *testing.T) {
	shipped := shippedComposeTemplates(t)
	names := func(list string) []string {
		var out []string
		for _, n := range strings.Split(list, ",") {
			out = append(out, strings.TrimSpace(n))
		}
		return out
	}
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_compose_video" {
			continue
		}
		m := regexp.MustCompile(`shipped: ([a-z0-9, -]+);`).FindStringSubmatch(tool.Description)
		if m == nil {
			t.Fatalf("the description has no \"shipped: a, b, c;\" list: %s", tool.Description)
		}
		listed := names(m[1])
		sort.Strings(listed)
		if strings.Join(listed, ",") != strings.Join(shipped, ",") {
			t.Errorf("the description's shipped list is %v, but the templates on disk are %v", listed, shipped)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		ex := regexp.MustCompile(`\(e\.g\. ([a-z0-9, -]+)\)`).FindStringSubmatch(schema.Properties["template"].Description)
		if ex == nil {
			t.Fatalf("the template parameter has no \"(e.g. a, b)\" example: %s", schema.Properties["template"].Description)
		}
		onDisk := map[string]bool{}
		for _, n := range shipped {
			onDisk[n] = true
		}
		for _, n := range names(ex[1]) {
			if !onDisk[n] {
				t.Errorf("the template parameter's example names %q, which does not ship (shipped: %s)", n, strings.Join(shipped, ", "))
			}
		}
		return
	}
	t.Fatal("offload_compose_video not advertised on tools/list")
}
