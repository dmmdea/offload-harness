package tasks

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The serving stack reuses a KV prefix: measured on <node-b> 2026-07-27 against
// gemma-4-e4b, a 2037-token prompt re-sent with the SAME leading instructions and a
// DIFFERENT payload re-prefilled only 41 tokens (cache_n 1996), turning a 7.1 s call
// into 0.30 s. That saving exists only while the stable text comes FIRST and the
// variable document LAST.
//
// It is one `fmt.Sprintf` argument order away from being lost silently — nothing
// would fail, every call would just quietly pay full prefill again. These tests are
// the guard.

func inputOf(task core.TaskType, input string, params map[string]any) (Built, error) {
	return Build(core.Request{Task: task, Input: input, Params: params})
}

var textTasks = []struct {
	name   string
	task   core.TaskType
	params map[string]any
}{
	{"summarize", core.TaskSummarize, map[string]any{"max_points": 3}},
	{"classify", core.TaskClassify, map[string]any{"labels": []any{"bug", "feature"}}},
	{"triage", core.TaskTriage, map[string]any{"question": "Is this an error?"}},
	{"extract", core.TaskExtract, map[string]any{"schema": map[string]any{
		"properties": map[string]any{"name": map[string]any{"type": "string"}}}}},
}

// TestVariableInputComesLast: the payload must be the SUFFIX of the user message, so
// everything before it is a reusable prefix across calls that share a task shape.
func TestVariableInputComesLast(t *testing.T) {
	const doc = "ZZZ-DISTINCTIVE-PAYLOAD-ZZZ the quick brown fox jumps over the lazy dog."
	for _, tc := range textTasks {
		built, err := inputOf(tc.task, doc, tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.HasSuffix(built.User, doc) {
			t.Errorf("%s: the variable input is not the suffix of the user message — every call "+
				"will re-prefill from the first differing token.\nuser=%q", tc.name, built.User)
		}
		if strings.Count(built.User, doc) != 1 {
			t.Errorf("%s: the input appears %d times; repeating it moves the divergence point earlier",
				tc.name, strings.Count(built.User, doc))
		}
	}
}

// TestSystemPromptCarriesNoPerCallContent: the system message is the outermost part
// of the prefix. If any per-call value leaks into it, the cache diverges at token ~0
// and nothing downstream can recover the reuse.
func TestSystemPromptCarriesNoPerCallContent(t *testing.T) {
	for _, tc := range textTasks {
		a, err := inputOf(tc.task, "first document, about invoices", tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		b, err := inputOf(tc.task, "an entirely different document about weather", tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if a.System != b.System {
			t.Errorf("%s: system prompt varies with the input — the prefix diverges immediately\n a=%q\n b=%q",
				tc.name, a.System, b.System)
		}
		if a.System == "" {
			t.Errorf("%s: empty system prompt", tc.name)
		}
	}
}

// TestStablePrefixIsSubstantial: the reuse is only worth having if the shared prefix
// is more than a couple of tokens. This is a floor, not a target.
func TestStablePrefixIsSubstantial(t *testing.T) {
	const doc = "payload"
	for _, tc := range textTasks {
		built, err := inputOf(tc.task, doc, tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		prefix := len(built.System) + len(strings.TrimSuffix(built.User, doc))
		if prefix < 80 {
			t.Errorf("%s: only %d bytes of stable prefix — too little to be worth caching", tc.name, prefix)
		}
	}
}
