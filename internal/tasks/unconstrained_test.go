package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/validator"
)

func textReqs() map[string]core.Request {
	return map[string]core.Request{
		"classify": {Task: core.TaskClassify, Input: "The invoice is overdue by ten days.",
			Params: map[string]any{"labels": []string{"billing", "support", "sales"}}},
		"extract": {Task: core.TaskExtract, Input: "Ada Lovelace, born 1815.",
			Params: map[string]any{"schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}, "born": map[string]any{"type": "integer"}},
				"required":   []any{"name", "born"},
			}}},
		"summarize": {Task: core.TaskSummarize, Input: "Some text to summarize.", Params: map[string]any{"max_points": 3}},
		"triage":    {Task: core.TaskTriage, Input: "Some text to triage.", Params: map[string]any{"question": "Is it urgent?"}},
	}
}

// A seat that takes a grammar must get byte-for-byte what Build always produced: BuildFor with the
// zero Caps is Build, so every grammar seat's prompt, grammar and prompt-prefix fingerprint (which
// hashes System and the grammar) are unchanged.
func TestBuildForZeroCapsIsBuildByteForByte(t *testing.T) {
	for name, req := range textReqs() {
		want, err := Build(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := BuildFor(req, Caps{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.System != want.System || got.User != want.User || got.Grammar != want.Grammar || got.MaxTokens != want.MaxTokens {
			t.Errorf("%s: BuildFor(zero Caps) differs from Build", name)
		}
		if got.Strict != nil {
			t.Errorf("%s: a grammar seat's build must carry no Strict schema", name)
		}
	}
}

// Golden: the grammar-seat prompt for each structured task, pinned by digest of System, User and
// Grammar. The digests were taken from Build as it stood before the unconstrained path existed
// (base ee4e23c9), so an edit that leaks the shape instruction into Build, and through it into
// every normal seat's prompt-prefix fingerprint and cache key, fails here even if BuildFor and
// Build drift together. They were re-taken once, for register C-80, when gbnf's whitespace rule
// became llama.cpp's bounded one: with the old rule swapped back into the grammar the original
// digests reproduce exactly, so that rule is the only byte that moved (and every grammar seat's
// prompt-prefix fingerprint moved with it, once).
func TestGrammarSeatPromptGolden(t *testing.T) {
	want := map[string]string{
		"classify":  "cbadb0f198dc0a3dc15e2669f5f4b937f98b5006477354640ef0b93afdeb0711",
		"extract":   "6ded91a297e35c88008816e3b086c0d85f7904997be75e3c669e3c4ef93f251c",
		"summarize": "54c2607ba00db595afd8d6c3c185cfd112644ae6afa6108018a02f5fe75e8d0e",
		"triage":    "5015af69d913a31e709aacd08fbbd7dfd051598cf5dbefcf02fef52576aa3f03",
	}
	for name, req := range textReqs() {
		b, err := Build(req)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(b.System + "\x00" + b.User + "\x00" + b.Grammar))
		if got := hex.EncodeToString(h[:]); got != want[name] {
			t.Errorf("%s: grammar-seat prompt changed: digest %s, want %s", name, got, want[name])
		}
	}
}

func TestForUnconstrainedClearsGrammarAndStatesTheShape(t *testing.T) {
	b, err := BuildFor(textReqs()["classify"], Caps{Unconstrained: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Grammar != "" {
		t.Errorf("an unconstrained seat must get no grammar, got %q", b.Grammar)
	}
	normal, _ := Build(textReqs()["classify"])
	if b.User != normal.User {
		t.Errorf("the user prompt (input, exemplars) must be untouched")
	}
	for _, want := range []string{`"label" (a string, exactly one of "billing", "support", "sales")`, `"confidence" (a number)`,
		"Do not add any other key", `{"label":"billing","confidence":0.5}`, "no markdown fences"} {
		if !strings.Contains(b.System, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, b.System)
		}
	}
	if !strings.HasPrefix(b.System, normal.System) {
		t.Errorf("the shape instruction must be appended to the normal system prompt")
	}
	if len(b.Fields) == 0 || b.Strict == nil {
		t.Errorf("Fields and Strict must be set: %+v", b)
	}
	// Deterministic: it rides in the prompt-prefix fingerprint.
	b2, _ := BuildFor(textReqs()["classify"], Caps{Unconstrained: true})
	if b2.System != b.System {
		t.Errorf("the shape instruction is not deterministic")
	}
}

func TestStrictSchemaRejectsWhatAGrammarWouldHaveForbidden(t *testing.T) {
	cls, _ := BuildFor(textReqs()["classify"], Caps{Unconstrained: true})
	ext, _ := BuildFor(textReqs()["extract"], Caps{Unconstrained: true})
	for _, tc := range []struct {
		name   string
		built  Built
		reply  string
		accept bool
	}{
		{"classify ok", cls, `{"label":"billing","confidence":0.9}`, true},
		{"classify foreign object", cls, `{"foo":1}`, false},
		{"classify summarize-shaped object", cls, `{"summary":"x"}`, false},
		{"classify label outside the set", cls, `{"label":"refund","confidence":0.9}`, false},
		{"classify missing confidence", cls, `{"label":"billing"}`, false},
		{"classify extra key", cls, `{"label":"billing","confidence":0.9,"why":"x"}`, false},
		{"classify wrong type", cls, `{"label":"billing","confidence":"high"}`, false},
		{"classify confidence 7", cls, `{"label":"billing","confidence":7}`, false},
		{"classify confidence -5", cls, `{"label":"billing","confidence":-5}`, false},
		{"classify confidence 1.0001", cls, `{"label":"billing","confidence":1.0001}`, false},
		{"classify confidence 0.93", cls, `{"label":"billing","confidence":0.93}`, true},
		{"classify confidence 0 and 1", cls, `{"label":"billing","confidence":1}`, true},
		{"extract ok", ext, `{"name":"Ada Lovelace","born":1815}`, true},
		{"extract missing field", ext, `{"name":"Ada Lovelace"}`, false},
		{"extract extra key", ext, `{"name":"Ada","born":1815,"age":210}`, false},
		{"extract wrong type", ext, `{"name":"Ada","born":"1815"}`, false},
	} {
		err := validator.Validate([]byte(tc.reply), tc.built.Strict)
		if tc.accept && err != nil {
			t.Errorf("%s: should be accepted, got %v", tc.name, err)
		}
		if !tc.accept && err == nil {
			t.Errorf("%s: must be rejected", tc.name)
		}
	}
}

// A free-text task carries no field list, so nothing is rewritten for it.
func TestForUnconstrainedLeavesFreeTextTasksAlone(t *testing.T) {
	req := core.Request{Task: core.TaskVQA, Params: map[string]any{"question": "what?"}}
	want, _ := Build(req)
	got, _ := BuildFor(req, Caps{Unconstrained: true})
	if got.System != want.System || got.Strict != nil {
		t.Errorf("a free-text task must be unchanged: %+v", got)
	}
}

func TestShapeInstructionQuotesEnumValues(t *testing.T) {
	b, _ := BuildFor(core.Request{Task: core.TaskClassify, Input: "x",
		Params: map[string]any{"labels": []string{`say "hi"`, "b"}}}, Caps{Unconstrained: true})
	if !strings.Contains(b.System, `"say \"hi\""`) {
		t.Errorf("an enum value with a quote must be JSON-escaped in the instruction:\n%s", b.System)
	}
	var probe map[string]any
	ex := b.System[strings.LastIndex(b.System, "{"):]
	if err := json.Unmarshal([]byte(ex), &probe); err != nil {
		t.Errorf("the example must itself be valid JSON: %v (%s)", err, ex)
	}
}

// A nested extract schema (an array of objects, an object) used to compile to a list of strings
// and a string, so the shape instruction told an unconstrained seat to write strings and the
// Strict schema rejected the objects the contract's own schema asks for (register F43). The
// instruction now states the keys of every nested object, and Strict is the nested shape.
func TestUnconstrainedExtractStatesAndEnforcesANestedSchema(t *testing.T) {
	req := core.Request{Task: core.TaskExtract, Input: "Two labels.",
		Params: map[string]any{"schema": map[string]any{
			"type":     "object",
			"required": []any{"title", "labels", "meta"},
			"properties": map[string]any{
				"title": map[string]any{"type": "string"},
				"labels": map[string]any{"type": "array", "items": map[string]any{
					"type": "object", "required": []any{"name", "score"},
					"properties": map[string]any{"name": map[string]any{"type": "string"}, "score": map[string]any{"type": "number"}}}},
				"meta": map[string]any{"type": "object", "required": []any{"source"},
					"properties": map[string]any{"source": map[string]any{"type": "string"}}},
			}}}}
	b, err := BuildFor(req, Caps{Unconstrained: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"labels" (an array whose every element is an object with exactly these keys, each once, in this order: "name" (a string), "score" (a number))`,
		`"meta" (an object with exactly these keys, each once, in this order: "source" (a string))`,
		`{"title":"...","labels":[{"name":"...","score":0.5}],"meta":{"source":"..."}}`,
	} {
		if !strings.Contains(b.System, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, b.System)
		}
	}
	for _, tc := range []struct {
		name   string
		reply  string
		accept bool
	}{
		{"nested ok", `{"title":"t","labels":[{"name":"x","score":1},{"name":"y","score":2.5}],"meta":{"source":"s"}}`, true},
		{"no labels", `{"title":"t","labels":[],"meta":{"source":"s"}}`, true},
		{"labels as strings", `{"title":"t","labels":["x"],"meta":{"source":"s"}}`, false},
		{"meta as a string", `{"title":"t","labels":[],"meta":"s"}`, false},
		{"label missing a key", `{"title":"t","labels":[{"name":"x"}],"meta":{"source":"s"}}`, false},
		{"label with an extra key", `{"title":"t","labels":[{"name":"x","score":1,"z":0}],"meta":{"source":"s"}}`, false},
		{"label score as a string", `{"title":"t","labels":[{"name":"x","score":"1"}],"meta":{"source":"s"}}`, false},
	} {
		err := validator.Validate([]byte(tc.reply), b.Strict)
		if tc.accept && err != nil {
			t.Errorf("%s: should be accepted, got %v", tc.name, err)
		}
		if !tc.accept && err == nil {
			t.Errorf("%s: must be rejected", tc.name)
		}
	}
}
