package servingtmpl

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEveryTemplateModelUnloadsAfterFiveIdleMinutes pins the operator rule of
// 2026-09-08/10: no model stays loaded past five idle minutes, on any tier, any
// engine. The live boxes complied (every entry ttl 300) while the templates a fresh
// install renders still carried ttl: 600 on the memory-stack residents in eight
// templates and NO ttl at all on the resident-roster template — so every new box
// would have violated the rule on day one. A template-level gate that fails.
func TestEveryTemplateModelUnloadsAfterFiveIdleMinutes(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "setup", "templates", "llama-swap.*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Models map[string]map[string]any `yaml:"models"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		if len(doc.Models) == 0 {
			t.Fatalf("%s: no models map", filepath.Base(p))
		}
		for name, m := range doc.Models {
			ttl, ok := m["ttl"]
			if !ok {
				t.Errorf("%s: model %q has no ttl — it would stay loaded forever", filepath.Base(p), name)
				continue
			}
			if v, isInt := ttl.(int); !isInt || v != 300 {
				t.Errorf("%s: model %q ttl = %v, want 300 (operator rule: 5 idle minutes, every engine)", filepath.Base(p), name, ttl)
			}
		}
	}
}
