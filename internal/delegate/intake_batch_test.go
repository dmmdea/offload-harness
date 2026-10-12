// Per-subtask intake (register D-111, part 1): PrepareBatchWithCap prepares each subtask on
// its own, so one bad subtask costs the call that subtask and no other. The pins here are the
// engine side of the door tests in internal/mcpserver (delegate_batch_test.go): the valid
// contracts keep their input positions, a rejection is a contract-class defer that never
// touches the infrastructure counts, and the subtask cap is read off the input.

package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// batchSpec is a valid spec named by its goal; bad turns it into one that fails intake (a
// context file that is not in the root).
func batchSpec(goal string, bad bool) SubtaskSpec {
	s := SubtaskSpec{AgentContract: core.AgentContract{Goal: goal}}
	if bad {
		s.ContextPaths = []string{"not-there.txt"}
	}
	return s
}

func TestPrepareBatchWithCapKeepsInputPositionsAndRejections(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	specs := []SubtaskSpec{
		batchSpec("zero", true),
		batchSpec("one", false),
		batchSpec("two", true),
		{AgentContract: core.AgentContract{Goal: "three"}, ContextPaths: []string{"ok.txt"}},
	}
	b := PrepareBatchWithCap(specs, root, 0)
	if b.Total != 4 {
		t.Fatalf("Total = %d, want 4", b.Total)
	}
	if len(b.Contracts) != 2 || b.Contracts[0].Goal != "one" || b.Contracts[1].Goal != "three" {
		t.Fatalf("Contracts = %+v, want the valid subtasks, in input order", b.Contracts)
	}
	if len(b.Indices) != 2 || b.Indices[0] != 1 || b.Indices[1] != 3 {
		t.Fatalf("Indices = %v, want [1 3]: each valid contract keeps its input position", b.Indices)
	}
	if len(b.Rejections) != 2 || b.Rejections[0].Index != 0 || b.Rejections[1].Index != 2 {
		t.Fatalf("Rejections = %+v, want positions 0 and 2", b.Rejections)
	}
	if !strings.Contains(b.Rejections[0].Err.Error(), "not-there.txt") {
		t.Errorf("Rejections[0].Err = %v, want it to name the path", b.Rejections[0].Err)
	}
	if len(b.Contracts[1].Context) != 1 || b.Contracts[1].Context[0].Text != "fine" {
		t.Errorf("Contracts[1].Context = %+v, want ok.txt inlined: preparing one subtask must not depend on another", b.Contracts[1].Context)
	}
	if b.AllRejected() {
		t.Error("AllRejected = true with two valid subtasks")
	}
}

// TestPrepareBatchWithCapAgreesWithPrepareContractWithCapOnEverySubtask: the batch adds a
// way to report, never a different verdict. Each spec prepared alone gives the contract (or the
// error text) it gets inside the batch, so no rule of PrepareContractWithCap was copied or loosened.
func TestPrepareBatchWithCapAgreesWithPrepareContractWithCapOnEverySubtask(t *testing.T) {
	root := t.TempDir()
	specs := []SubtaskSpec{
		batchSpec("alpha", false),
		batchSpec("beta", true),
		{AgentContract: core.AgentContract{Goal: "gamma", Acceptance: []string{"contains:"}}}, // unparseable acceptance
		{AgentContract: core.AgentContract{Goal: "  "}},                                       // blank goal
		batchSpec("omega", false),
	}
	b := PrepareBatchWithCap(specs, root, 0)
	contract, rej := 0, 0
	for i, spec := range specs {
		c, err := PrepareContractWithCap(spec, root, 0)
		if err != nil {
			if rej >= len(b.Rejections) || b.Rejections[rej].Index != i || b.Rejections[rej].Err.Error() != err.Error() {
				t.Fatalf("spec %d alone: %v; the batch has %+v", i, err, b.Rejections)
			}
			rej++
			continue
		}
		if contract >= len(b.Contracts) || b.Indices[contract] != i || b.Contracts[contract].Goal != c.Goal {
			t.Fatalf("spec %d alone prepared as %q; the batch has %+v at %v", i, c.Goal, b.Contracts, b.Indices)
		}
		contract++
	}
	if contract != len(b.Contracts) || rej != len(b.Rejections) {
		t.Fatalf("the batch holds %d contracts and %d rejections, alone: %d and %d", len(b.Contracts), len(b.Rejections), contract, rej)
	}
}

func TestPrepareBatchWithCapAllRejected(t *testing.T) {
	b := PrepareBatchWithCap([]SubtaskSpec{batchSpec("a", true), batchSpec("b", true)}, t.TempDir(), 0)
	if !b.AllRejected() {
		t.Fatal("AllRejected = false for a call with nothing valid in it")
	}
	if got := PrepareBatchWithCap(nil, t.TempDir(), 0); got.AllRejected() {
		t.Error("AllRejected = true for an empty call: there is nothing rejected, the door's own 'at least one subtask' answers")
	}
}

func TestFlatReasonNamesEveryRejectionUnderItsOwnNumber(t *testing.T) {
	b := PrepareBatchWithCap([]SubtaskSpec{batchSpec("a", true), batchSpec("fine", false), batchSpec("c", true)}, t.TempDir(), 0)
	got := b.FlatReason("subtask", nil)
	for _, want := range []string{"subtask 0: ", "subtask 2: ", "; "} {
		if !strings.Contains(got, want) {
			t.Errorf("FlatReason = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "subtask 1") {
		t.Errorf("FlatReason = %q names the valid subtask 1", got)
	}
	// A call with one rejection reads as it always did: no separator, the bare "subtask N: cause".
	one := PrepareBatchWithCap([]SubtaskSpec{batchSpec("fine", false), batchSpec("c", true)}, t.TempDir(), 0)
	if r := one.FlatReason("subtask", nil); !strings.HasPrefix(r, "subtask 1: ") || strings.Contains(r, "; ") {
		t.Errorf("FlatReason for one rejection = %q, want exactly \"subtask 1: <cause>\"", r)
	}
	// A research page is named by its place in the url list, not by its place among the usable ones.
	pages := b.FlatReason("source", []int{4, 5, 7})
	if !strings.Contains(pages, "source 4: ") || !strings.Contains(pages, "source 7: ") || strings.Contains(pages, "source 0") {
		t.Errorf("FlatReason with ids = %q, want source 4 and source 7", pages)
	}
}

func placed(output string) PlacedResult {
	return PlacedResult{Node: "n", Seat: "s", Result: core.AgentWireResult{Output: output}}
}

// TestSpliceFilesRejectionsAtTheirOwnPositionsAsContractDefers: one result per subtask sent,
// in input order; the rejection is a defer that no node ran, counted as Deferred and nowhere else.
func TestSpliceFilesRejectionsAtTheirOwnPositionsAsContractDefers(t *testing.T) {
	b := PrepareBatchWithCap([]SubtaskSpec{batchSpec("a", true), batchSpec("b", false), batchSpec("c", true), batchSpec("d", false)}, t.TempDir(), 0)
	got, sum := b.Splice([]PlacedResult{placed("B"), placed("D")}, Summary{Succeeded: 2, Retried: 1}, "subtask", nil)
	if len(got) != 4 {
		t.Fatalf("%d results, want 4", len(got))
	}
	for i, wantOutput := range []string{"", "B", "", "D"} {
		if got[i].Result.Output != wantOutput {
			t.Errorf("results[%d].Output = %q, want %q", i, got[i].Result.Output, wantOutput)
		}
	}
	for _, i := range []int{0, 2} {
		r := got[i]
		if !r.Result.Deferred || r.Result.DeferClass != core.DeferClassContract || !r.Unplaced || r.Err != "" {
			t.Errorf("results[%d] = %+v, want a contract defer no node ran", i, r)
		}
		if want := "subtask " + string(rune('0'+i)) + ": "; !strings.HasPrefix(r.Result.Reason, want) {
			t.Errorf("results[%d].Reason = %q, want prefix %q", i, r.Result.Reason, want)
		}
		if r.Node != "" || r.Seat != "" || r.Result.SchemaVersion != core.AgentWireSchemaVersion {
			t.Errorf("results[%d] = %+v: no node, no seat, current wire version", i, r)
		}
	}
	if sum.Succeeded != 2 || sum.Deferred != 2 || sum.Infrastructure != 0 || sum.LostToStack != 0 || sum.Failed != 0 || sum.Retried != 1 {
		t.Errorf("sum = %+v, want succeeded 2, deferred 2, infrastructure 0, the engine's other counts untouched", sum)
	}
	if BrokenStackDefer(got[0].Result.DeferClass) {
		t.Error("a rejection reads as a broken stack")
	}
}

func TestSpliceWithNoRejectionsReturnsTheEngineOutcomeUntouched(t *testing.T) {
	b := PrepareBatchWithCap([]SubtaskSpec{batchSpec("a", false), batchSpec("b", false)}, t.TempDir(), 0)
	in := []PlacedResult{placed("A"), placed("B")}
	got, sum := b.Splice(in, Summary{Succeeded: 2}, "subtask", nil)
	if len(got) != 2 || got[0].Result.Output != "A" || got[1].Result.Output != "B" || sum != (Summary{Succeeded: 2}) {
		t.Fatalf("got %+v / %+v, want the engine's results and summary unchanged", got, sum)
	}
}

// TestSpliceStopsAtTheFirstValidSubtaskTheEngineNeverReturned: RunBatched can return an error
// with the results it had collected. The merge ends where the results end, and a rejection
// beyond that point is counted with the Skipped rest instead of being published ahead of results
// that do not exist.
func TestSpliceStopsAtTheFirstValidSubtaskTheEngineNeverReturned(t *testing.T) {
	// 0 rejected, 1 valid (has a result), 2 valid (no result), 3 rejected (after the cut).
	b := PrepareBatchWithCap([]SubtaskSpec{batchSpec("a", true), batchSpec("b", false), batchSpec("c", false), batchSpec("d", true)}, t.TempDir(), 0)
	got, sum := b.Splice([]PlacedResult{placed("B")}, Summary{Succeeded: 1, Skipped: 1}, "subtask", nil)
	if len(got) != 2 || !got[0].Result.Deferred || got[1].Result.Output != "B" {
		t.Fatalf("got %+v, want the rejection at 0 and the one result at 1, and nothing after the cut", got)
	}
	if sum.Deferred != 1 || sum.Skipped != 2 {
		t.Errorf("sum = %+v, want deferred 1 (the rejection ahead of the cut) and skipped 2 (the engine's 1 plus the rejection beyond it)", sum)
	}
}

// TestCheckSubtaskCountIsTheRefusalRunWithGives: the same words, so a surface that checks the
// input first answers a call over the cap exactly as the engine would have.
func TestCheckSubtaskCountIsTheRefusalRunWithGives(t *testing.T) {
	if err := CheckSubtaskCount(MaxSubtasks); err != nil {
		t.Fatalf("CheckSubtaskCount(%d) = %v, want nil at the cap", MaxSubtasks, err)
	}
	err := CheckSubtaskCount(MaxSubtasks + 1)
	if err == nil {
		t.Fatalf("CheckSubtaskCount(%d) = nil, want a refusal over the cap", MaxSubtasks+1)
	}
	contracts := make([]core.AgentContract, MaxSubtasks+1)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "g"}
	}
	_, _, runErr := RunWith(t.Context(), testCfg(t), nil, contracts, "local", nil, nil)
	if runErr == nil || runErr.Error() != err.Error() {
		t.Errorf("RunWith said %v, CheckSubtaskCount said %v: the words must be one", runErr, err)
	}
	if errors.Unwrap(err) != nil {
		t.Errorf("CheckSubtaskCount wraps %v; it is a plain refusal", errors.Unwrap(err))
	}
}
