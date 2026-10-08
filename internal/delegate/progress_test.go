package delegate

// The engine half of progress reporting (progress.go, ADR 0065): the one-phrase
// verdict a finished subtask is reported with, and the rebasing that makes a
// batched call count against the whole call. The MCP doors turn these into
// notifications; nothing here decides anything.

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestOutcomeWordSpeaksTheSummaryBucketsVocabulary: the word a client reads for a
// finished subtask is the bucket the summary will count it in. A failure or a failed
// verification reported as "succeeded" would tell the client its work was fine while the
// final summary says otherwise.
func TestOutcomeWordSpeaksTheSummaryBucketsVocabulary(t *testing.T) {
	for name, tc := range map[string]struct {
		pr   PlacedResult
		want string
	}{
		"a finished answer":       {PlacedResult{Result: localOK()}, "succeeded"},
		"a failure":               {PlacedResult{Err: "dispatch refused"}, "failed"},
		"a failed verification":   {PlacedResult{Result: localOK(), AcceptanceFailures: []string{"contains:widget"}}, "failed verification"},
		"a classed defer":         {PlacedResult{Result: cancelledLoop()}, "deferred (budget)"},
		"a defer with no class":   {PlacedResult{Result: core.AgentWireResult{Deferred: true}}, "deferred"},
		"a failure beats a defer": {PlacedResult{Err: "x", Result: cancelledLoop()}, "failed"},
	} {
		if got := outcomeWord(tc.pr); got != tc.want {
			t.Errorf("%s: outcomeWord = %q, want %q", name, got, tc.want)
		}
	}
}

// TestShiftedProgressCountsAgainstTheWholeCall: RunBatched runs a list longer than one
// batch as consecutive chunks, and a client counting "n of 9" must not see each chunk start
// over. The second chunk's first subtask is the call's ninth (here the chunk before it
// held eight): its index, the running done count and the total are all rebased.
func TestShiftedProgressCountsAgainstTheWholeCall(t *testing.T) {
	var got ProgressEvent
	fn := shiftedProgress(func(ev ProgressEvent) { got = ev }, 8, 9)
	fn(ProgressEvent{Kind: "finished", Index: 0, Done: 1, Total: 1, Node: "node-a", Outcome: "succeeded"})
	want := ProgressEvent{Kind: "finished", Index: 8, Done: 9, Total: 9, Node: "node-a", Outcome: "succeeded"}
	if got != want {
		t.Fatalf("rebased event = %+v, want %+v", got, want)
	}
}
