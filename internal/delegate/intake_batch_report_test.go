package delegate

// D-111 fix round: the numbers and the words of a call whose intake refused some subtasks.
//
// The engine is handed only the subtasks that passed intake, so everything it reports about
// them (progress events, an index into its results) is relative to that subset. The surface
// speaks in the caller's terms: MapProgress puts an event back on the subtask's input
// position and the call's real total, and AlsoRefused keeps a whole-call defer from hiding the
// subtasks intake already knew were malformed.

import (
	"testing"
)

// mixedBatch is the call [ok, refused, ok]: two contracts for the engine, one rejection at 1.
func mixedBatch() PreparedBatch {
	return PreparedBatch{
		Total:      3,
		Indices:    []int{0, 2},
		Rejections: []IntakeRejection{{Index: 1, Err: errString("context_paths: no-such-file.txt")}},
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestMapProgressSpeaksInTheCallersNumbers(t *testing.T) {
	var got []ProgressEvent
	fn := mixedBatch().MapProgress(func(ev ProgressEvent) { got = append(got, ev) })
	// The engine's second contract (index 1 of 2) is the caller's third subtask, and the one
	// it finished first (Done 1) is the second to be done once the refused one counts.
	fn(ProgressEvent{Kind: "started", Index: 1, Done: 0, Total: 2})
	fn(ProgressEvent{Kind: "finished", Index: 1, Done: 1, Total: 2, Outcome: "succeeded", Node: "n"})
	fn(ProgressEvent{Kind: "finished", Index: 0, Done: 2, Total: 2, Outcome: "succeeded"})
	want := []ProgressEvent{
		{Kind: "started", Index: 2, Done: 1, Total: 3},
		{Kind: "finished", Index: 2, Done: 2, Total: 3, Outcome: "succeeded", Node: "n"},
		{Kind: "finished", Index: 0, Done: 3, Total: 3, Outcome: "succeeded"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMapProgressLeavesACleanBatchAndANilCallbackAlone(t *testing.T) {
	if got := mixedBatch().MapProgress(nil); got != nil {
		t.Fatal("a nil callback must stay nil: a non-nil wrapper would tell the engine someone is listening")
	}
	clean := PreparedBatch{Total: 2, Indices: []int{0, 1}}
	var got ProgressEvent
	fn := clean.MapProgress(func(ev ProgressEvent) { got = ev })
	ev := ProgressEvent{Kind: "finished", Index: 1, Done: 2, Total: 2, Outcome: "succeeded"}
	fn(ev)
	if got != ev {
		t.Fatalf("a batch with no rejections changed the event: %+v -> %+v", ev, got)
	}
}

// An index the engine reports outside the batch (a chunk offset it should not produce) is kept
// as it came rather than panicking the engine's goroutine.
func TestMapProgressKeepsAnIndexOutsideTheBatch(t *testing.T) {
	var got ProgressEvent
	mixedBatch().MapProgress(func(ev ProgressEvent) { got = ev })(ProgressEvent{Kind: "started", Index: 7, Total: 2})
	if got.Index != 7 || got.Total != 3 {
		t.Fatalf("event = %+v, want the unknown index kept and the total corrected", got)
	}
}

func TestAlsoRefusedNamesTheRejectionsBesideTheEnginesError(t *testing.T) {
	b := mixedBatch()
	if got, want := b.AlsoRefused("delegate: route \"x\" not recognized", "subtask", nil),
		"delegate: route \"x\" not recognized; also refused at intake: subtask 1: context_paths: no-such-file.txt"; got != want {
		t.Errorf("AlsoRefused = %q, want %q", got, want)
	}
	// A page is named by its place in the url list, not by the contract's place.
	if got, want := b.AlsoRefused("boom", "source", []int{0, 4, 5}),
		"boom; also refused at intake: source 4: context_paths: no-such-file.txt"; got != want {
		t.Errorf("AlsoRefused with ids = %q, want %q", got, want)
	}
	if got := (PreparedBatch{Total: 1, Indices: []int{0}}).AlsoRefused("boom", "subtask", nil); got != "boom" {
		t.Errorf("AlsoRefused with nothing refused = %q, want the reason untouched", got)
	}
}
