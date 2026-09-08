package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 0.113.32 shipped a triple-blackwell template that llama-swap cannot load, and CI
// passed it 5/5.
//
// The generator that added the opt-in over-2-card seats anchored on the literal
// string "matrix:" and hit its FIRST occurrence — which was inside the comment
// "# RESIDENCY IS DECLARED WITH `matrix:` (llama-swap >= v202; ...)". It wedged the
// whole seat block into the middle of that comment line, leaving the comment's tail
// stranded at column 0 as a bogus top-level key and the real `matrix:` mapping
// duplicated below it.
//
// Nothing caught it because every assertion in setup/render.tests.ps1 is a REGEX
// over the config TEXT. A regex cannot tell a mapping from a comment, so a file that
// yaml.v3 — the very parser llama-swap uses — rejects outright sailed through as
// "ALL PASS". These two tests close that gap: the templates and their rendered
// output must actually PARSE, not merely contain the right substrings.
func TestEveryTemplateIsParseableYAML(t *testing.T) {
	for _, name := range templateNames(t) {
		t.Run(name, func(t *testing.T) {
			b := readTemplate(t, name)
			var doc map[string]any
			// yaml.v3 reports a duplicate top-level key as an error, which is the
			// exact shape of the 0.113.32 defect.
			if err := yaml.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s is not parseable YAML — llama-swap would refuse to start:\n%v", name, err)
			}
			for _, key := range []string{"models", "matrix"} {
				if _, ok := doc[key]; !ok {
					t.Errorf("%s parsed but declares no %q mapping", name, key)
				}
			}
			models, ok := doc["models"].(map[string]any)
			if !ok || len(models) == 0 {
				t.Fatalf("%s has no usable models mapping", name)
			}
			// A seat whose body did not survive parsing (e.g. swallowed into a
			// comment) shows up as a nil or non-mapping value rather than an error.
			for id, v := range models {
				m, ok := v.(map[string]any)
				if !ok {
					t.Errorf("%s: seat %q did not parse as a mapping (got %T)", name, id, v)
					continue
				}
				if _, ok := m["cmd"]; !ok {
					t.Errorf("%s: seat %q declares no cmd", name, id)
				}
			}
		})
	}
}

// TestRenderedConfigIsParseableYAML covers the other half: a template can be valid
// while token substitution produces something that is not. Rendering is what
// install.ps1 actually ships to the box, so it is what has to parse.
func TestRenderedConfigIsParseableYAML(t *testing.T) {
	for _, name := range templateNames(t) {
		t.Run(name, func(t *testing.T) {
			out, err := Render(string(readTemplate(t, name)), params())
			if err != nil {
				// Render legitimately refuses a tier/template mismatch; the
				// existing refusal tests own that behaviour. Only parse what
				// actually rendered.
				t.Skipf("%s does not render under the baseline params: %v", name, err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("%s rendered a config that is not parseable YAML:\n%v", name, err)
			}
			if _, ok := doc["models"]; !ok {
				t.Errorf("%s rendered without a models mapping", name)
			}
		})
	}
}

func templateNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "setup", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "llama-swap.") && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no llama-swap templates found — the gate would silently pass on nothing")
	}
	return names
}

func readTemplate(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
