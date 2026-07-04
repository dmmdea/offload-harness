package grounding

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestCheck(t *testing.T) {
	src := "The RTX 3070 Mobile has 8GB of GDDR6 VRAM and runs Gemma-4 at 70-83 tokens per second."
	cases := []struct {
		name         string
		task         core.TaskType
		data         string
		wantGrounded bool
		wantOK       bool
	}{
		{"extract grounded", core.TaskExtract, `{"gpu_name":"RTX 3070 Mobile","vram_gb":8}`, true, true},
		{"extract ungrounded string", core.TaskExtract, `{"gpu_name":"RTX 4090"}`, false, true},
		{"extract ungrounded number", core.TaskExtract, `{"vram_gb":24}`, false, true},
		{"extract empty", core.TaskExtract, `{}`, false, false},
		{"summarize grounded numbers", core.TaskSummarize, `{"summary":"Runs at 70-83 tok/s with 8GB.","bullets":["GDDR6 VRAM"]}`, true, true},
		{"summarize invented number", core.TaskSummarize, `{"summary":"Runs at 999 tok/s.","bullets":[]}`, false, true},
		{"summarize no numbers", core.TaskSummarize, `{"summary":"It is a mobile GPU.","bullets":[]}`, true, true},
		{"classify n/a", core.TaskClassify, `{"label":"hardware","confidence":0.9}`, false, false},
		{"triage n/a", core.TaskTriage, `{"decision":"yes","reason":"x"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := Check(tc.task, src, []byte(tc.data))
			if ok != tc.wantOK || (ok && g != tc.wantGrounded) {
				t.Fatalf("Check = (grounded=%v, ok=%v); want (grounded=%v, ok=%v)", g, ok, tc.wantGrounded, tc.wantOK)
			}
		})
	}
}

func TestCheckFields(t *testing.T) {
	input := "Total amount 4200 for Acme Corp"
	data := []byte(`{"amount":"4200","party":"Acme Corp","tax":"9999"}`)
	bad, ok := CheckFields(core.TaskExtract, input, data)
	if !ok {
		t.Fatal("expected ok=true for extract")
	}
	if len(bad) != 1 || bad[0] != "tax" {
		t.Fatalf("expected [tax] ungrounded, got %v", bad)
	}
}
