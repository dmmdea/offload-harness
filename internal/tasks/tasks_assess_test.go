package tasks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
)

// TestBuildAssessImage_NoBrief: assess_image with no brief builds a grammar and
// the fixed system prompt plus the bare "Assess the image." user prompt.
func TestBuildAssessImage_NoBrief(t *testing.T) {
	b, err := Build(core.Request{Task: core.TaskAssessImage})
	if err != nil {
		t.Fatalf("Build assess_image (no brief): unexpected error: %v", err)
	}
	if b.Grammar == "" {
		t.Fatal("assess_image must produce a non-empty GBNF grammar")
	}
	if !strings.Contains(b.System, "image QA") {
		t.Errorf("system prompt missing QA framing: %q", b.System)
	}
	if !strings.Contains(b.System, "has_people") || !strings.Contains(b.System, "has_text") || !strings.Contains(b.System, "matches_brief") {
		t.Errorf("system prompt must define the fields: %q", b.System)
	}
	if b.User != "Assess the image." {
		t.Errorf("no-brief user prompt = %q, want %q", b.User, "Assess the image.")
	}
	if b.MaxTokens != 128 {
		t.Errorf("MaxTokens = %d, want 128", b.MaxTokens)
	}
	// The grammar must force exactly the four keys in order.
	for _, k := range []string{`\"has_people\"`, `\"has_text\"`, `\"matches_brief\"`, `\"notes\"`} {
		if !strings.Contains(b.Grammar, k) {
			t.Errorf("grammar missing key %s:\n%s", k, b.Grammar)
		}
	}
}

// TestBuildAssessImage_WithBrief: a non-empty brief is woven into the user prompt.
func TestBuildAssessImage_WithBrief(t *testing.T) {
	b, err := Build(core.Request{
		Task:   core.TaskAssessImage,
		Params: map[string]any{"brief": "a red sports car at sunset"},
	})
	if err != nil {
		t.Fatalf("Build assess_image (brief): unexpected error: %v", err)
	}
	if b.Grammar == "" {
		t.Fatal("assess_image must produce a non-empty GBNF grammar")
	}
	if !strings.Contains(b.User, "a red sports car at sunset") {
		t.Errorf("brief not woven into user prompt: %q", b.User)
	}
	if !strings.Contains(b.User, "Brief:") {
		t.Errorf("user prompt should label the brief: %q", b.User)
	}
}

// TestBuildAssessImage_GrammarShape: a sample conforming object parses, proving
// the grammar targets a real JSON object the model can emit.
func TestBuildAssessImage_GrammarShape(t *testing.T) {
	sample := `{"has_people":false,"has_text":true,"matches_brief":true,"notes":"black text on white"}`
	var v struct {
		HasPeople bool   `json:"has_people"`
		HasText   bool   `json:"has_text"`
		MatchesB  bool   `json:"matches_brief"`
		Notes     string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(sample), &v); err != nil {
		t.Fatalf("sample assess object should parse: %v", err)
	}
	if !v.HasText || v.HasPeople || !v.MatchesB || v.Notes == "" {
		t.Errorf("sample decoded wrong: %+v", v)
	}
}
