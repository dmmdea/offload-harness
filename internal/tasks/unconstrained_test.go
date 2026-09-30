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
// Build drift together.
func TestGrammarSeatPromptGolden(t *testing.T) {
	want := map[string]string{
		"classify":  "4a9b2fd2eb1fa6f1151eaa666f9e5b27e5364596baa7a3dc6f32fa449c78fd96",
		"extract":   "c6db82b9248f2dde47ff25a512527adc97104c9b0919fb36494372c6da6c13e0",
		"summarize": "8be46b03cbd7d15f0a7b81f50166ecc43cdeb8796a65c3875983f97baef0df97",
		"triage":    "a302175e415dfe6fadcf8c10fbc64188dc22a7f52206ca6ec86ad62fe32db9b5",
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
