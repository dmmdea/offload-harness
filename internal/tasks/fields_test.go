// fields_test.go holds every builder to ONE field list per task (register
// D-129). Built.Grammar constrains a llama.cpp seat and
// gbnf.JSONSchema(Built.Fields) constrains a vLLM seat; if a builder set one
// and not the other — or set them from different slices — the same task would
// be shaped differently per engine, and its answers would stop being
// comparable across the fleet. That is not a hypothetical: `Fields` is new,
// and a builder added later is exactly where it gets forgotten.
package tasks

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
)

func TestBuiltFieldsMatchTheGrammar(t *testing.T) {
	cases := []struct {
		name string
		req  core.Request
		// grammared is false for the free-text tasks (vqa, ocr, the video
		// pair), which constrain nothing and must therefore carry no fields.
		grammared bool
	}{
		{"summarize", core.Request{Task: core.TaskSummarize, Input: "some text", Params: map[string]any{"max_points": 3}}, true},
		{"classify", core.Request{Task: core.TaskClassify, Input: "some text", Params: map[string]any{"labels": []string{"a", "b"}}}, true},
		{"triage", core.Request{Task: core.TaskTriage, Input: "some text", Params: map[string]any{"question": "is it?"}}, true},
		{"assess_image", core.Request{Task: core.TaskAssessImage, Params: map[string]any{"brief": "a beach"}}, true},
		{"extract", core.Request{Task: core.TaskExtract, Input: "some text", Params: map[string]any{
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
					"age":  map[string]any{"type": "integer"},
				},
				"required": []any{"name", "age"},
			},
		}}, true},
		{"vqa", core.Request{Task: core.TaskVQA, Params: map[string]any{"question": "what?"}}, false},
		{"ocr", core.Request{Task: core.TaskOCR}, false},
		{"video_describe", core.Request{Task: core.TaskVideoDescribe, Params: map[string]any{"question": "what?"}}, false},
		{"video_watch", core.Request{Task: core.TaskVideoWatch, Params: map[string]any{"question": "what?"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Build(tc.req)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !tc.grammared {
				if b.Grammar != "" || len(b.Fields) != 0 {
					t.Fatalf("a free-text task must constrain nothing: grammar %q, fields %+v", b.Grammar, b.Fields)
				}
				return
			}
			if len(b.Fields) == 0 {
				t.Fatalf("Grammar is set but Fields is empty — a vLLM seat has nothing to be constrained by (D-129)")
			}
			// The grammar the fields compile to must BE the grammar the
			// builder published. A thinking-wrapped grammar contains it
			// rather than equalling it (gbnf.WrapThinking rewrites the root),
			// so accept containment too.
			fromFields := gbnf.Object(b.Fields)
			if b.Grammar != fromFields && !strings.Contains(b.Grammar, fromFields) {
				t.Errorf("Built.Fields do not compile to Built.Grammar:\n fields -> %s\n grammar: %s", fromFields, b.Grammar)
			}
			// ...and the schema a vLLM seat gets must round-trip back to the
			// same fields, so neither engine sees a field the other does not.
			back := gbnf.FromJSONSchema(gbnf.JSONSchema(b.Fields))
			if gbnf.Object(back) != fromFields {
				t.Errorf("JSONSchema(Fields) does not round-trip to the same shape:\n got: %s\nwant: %s", gbnf.Object(back), fromFields)
			}
		})
	}
}
