package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestAnswersAgree(t *testing.T) {
	cases := []struct {
		name        string
		task        core.TaskType
		candidate   string
		final       string
		wantAgreed  bool
		wantOK      bool
	}{
		{"classify same label", core.TaskClassify, `{"label":"spam","confidence":0.6}`, `{"label":"spam","confidence":0.9}`, true, true},
		// FIX 2: the raw entry candidate may be fenced/prose-wrapped; parser.Extract
		// cleans it symmetrically with how final.Data was produced, so a valid
		// agreement is no longer silently dropped (was (_, false) before the fix).
		{"classify fenced candidate", core.TaskClassify, "```json\n{\"label\":\"billing\"}\n```", `{"label":"billing"}`, true, true},
		{"triage prose-wrapped candidate", core.TaskTriage, "Here is my answer: {\"decision\":\"yes\",}", `{"decision":"yes"}`, true, true},
		{"classify diff label", core.TaskClassify, `{"label":"ham"}`, `{"label":"spam"}`, false, true},
		{"classify case-insensitive", core.TaskClassify, `{"label":"Spam"}`, `{"label":"spam "}`, true, true},
		{"triage decision agree", core.TaskTriage, `{"decision":"yes","reason":"x"}`, `{"decision":"yes"}`, true, true},
		{"triage decision disagree", core.TaskTriage, `{"decision":"no"}`, `{"decision":"unsure"}`, false, true},
		{"summarize not class-pinned", core.TaskSummarize, `{"summary":"a"}`, `{"summary":"b"}`, false, false},
		{"extract not class-pinned", core.TaskExtract, `{"name":"x"}`, `{"name":"x"}`, false, false},
		{"unparseable candidate", core.TaskClassify, `{not json`, `{"label":"spam"}`, false, false},
		{"unparseable final", core.TaskClassify, `{"label":"spam"}`, `garbage`, false, false},
		{"empty candidate field", core.TaskClassify, `{"confidence":0.5}`, `{"label":"spam"}`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agreed, ok := answersAgree(c.task, c.candidate, []byte(c.final))
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && agreed != c.wantAgreed {
				t.Fatalf("agreed=%v want %v", agreed, c.wantAgreed)
			}
		})
	}
}

func TestJSONStringField(t *testing.T) {
	if got := jsonStringField([]byte(`{"label":"spam"}`), "label"); got != "spam" {
		t.Fatalf("present: got %q", got)
	}
	if got := jsonStringField([]byte(`{"label":"spam"}`), "decision"); got != "" {
		t.Fatalf("absent: got %q", got)
	}
	if got := jsonStringField([]byte(`{"confidence":0.5}`), "confidence"); got != "" {
		t.Fatalf("non-string: got %q", got)
	}
	if got := jsonStringField([]byte(`not json`), "label"); got != "" {
		t.Fatalf("unparseable: got %q", got)
	}
}
