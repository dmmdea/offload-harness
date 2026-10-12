// intake_batch.go is the per-subtask half of the delegator-side intake (register D-111, part 1).
//
// PrepareContractWithCap prepares ONE subtask and fails with ONE error. A surface that
// prepared a whole call in a loop and returned at the first error refused every valid
// sibling with it: a caller that listed one scratch file that was not there lost the
// answer to the other subtask too, though nothing was wrong with that one. PrepareBatchWithCap
// prepares each subtask on its own and keeps the two outcomes apart, so the surface can run
// what is valid and report what is not, each at its own position.
//
// A rejected subtask is the CALLER's mistake (a missing file, a context past the cap, a
// goal left blank), not a broken stack. It is published as a contract-class defer
// (core.DeferClassContract): it counts in Summary.Deferred, never in Summary.Infrastructure
// or LostToStack, and it is never placed on a node, written to the ledger or sent to a seat.
// The subtask cap is not this function's: a surface checks it on the INPUT
// (CheckSubtaskCount) before it prepares anything, so rejections cannot buy a call a place
// under the cap.

package delegate

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// IntakeRejection is one subtask the intake refused: Index is its position in the call's
// input, Err the reason PrepareContractWithCap gave.
type IntakeRejection struct {
	Index int
	Err   error
}

// PreparedBatch is the outcome of preparing a whole call, subtask by subtask.
type PreparedBatch struct {
	// Total is the number of subtasks the call carried: len(Contracts) + len(Rejections).
	Total int
	// Contracts are the subtasks that passed intake, in input order.
	Contracts []core.AgentContract
	// Indices[k] is the input position of Contracts[k].
	Indices []int
	// Rejections are the subtasks that did not, in input order.
	Rejections []IntakeRejection
}

// PrepareBatchWithCap prepares every spec as PrepareContractWithCap does (the same minting,
// clamps, context_paths confinement under readRoot and validation at capBytes) and returns
// the valid contracts with their original positions beside the per-position rejections.
// Preparing one spec never depends on another, so a rejection changes nothing about the
// rest: PrepareContractWithCap(spec, ...) succeeds or fails alike alone or in a batch.
func PrepareBatchWithCap(specs []SubtaskSpec, readRoot string, capBytes int) PreparedBatch {
	b := PreparedBatch{Total: len(specs)}
	for i, spec := range specs {
		c, err := PrepareContractWithCap(spec, readRoot, capBytes)
		if err != nil {
			b.Rejections = append(b.Rejections, IntakeRejection{Index: i, Err: err})
			continue
		}
		b.Contracts = append(b.Contracts, c)
		b.Indices = append(b.Indices, i)
	}
	return b
}

// AllRejected reports a call with nothing valid in it: there is nothing to run, and the
// surface answers with the flat defer (FlatReason) it always gave.
func (b PreparedBatch) AllRejected() bool {
	return len(b.Contracts) == 0 && len(b.Rejections) > 0
}

// text words one rejection as "<label> <id>: <cause>", the form every surface has
// always used ("subtask 2: ...", "source 1: ..."). ids maps an input position to the number
// the caller knows the item by (offload_research names a page by its place in the url list,
// not by its place among the usable ones); nil means the input position.
func (r IntakeRejection) text(label string, ids []int) string {
	id := r.Index
	if r.Index < len(ids) {
		id = ids[r.Index]
	}
	return fmt.Sprintf("%s %d: %v", label, id, r.Err)
}

// FlatReason is the reason of the whole-call defer: every rejection, in input order, each
// under its own label and number. A call with one bad subtask reads exactly as it did when
// that was the only thing a call could say ("subtask 0: ...").
func (b PreparedBatch) FlatReason(label string, ids []int) string {
	parts := make([]string, 0, len(b.Rejections))
	for _, r := range b.Rejections {
		parts = append(parts, r.text(label, ids))
	}
	return strings.Join(parts, "; ")
}

// AlsoRefused words a whole-call defer that came from somewhere other than intake (the engine
// refused the call, so nothing ran and there is no result to splice a rejection into) so that it
// still names the subtasks intake refused: "<reason>; also refused at intake: subtask 1: ...". A
// caller who fixes the engine's complaint and resubmits meets nothing it was not told. A call with
// nothing refused gets reason back untouched.
func (b PreparedBatch) AlsoRefused(reason, label string, ids []int) string {
	if len(b.Rejections) == 0 {
		return reason
	}
	return reason + "; also refused at intake: " + b.FlatReason(label, ids)
}

// MapProgress turns the callback the engine reports through into one that speaks in the
// caller's numbers. The engine is handed only Contracts, so its events number subtasks among
// the valid ones and total only those: for input [ok, bad, ok] the third subtask would be
// announced as "2 of 2". The mapped event carries the subtask's input position (where its result
// sits in the answer), the call's full Total, and a Done count that includes the subtasks intake
// refused, which are finished before anything runs. A nil callback stays nil (the engine reports
// nothing then), and a batch with no rejections returns fn itself: its numbers were already right.
func (b PreparedBatch) MapProgress(fn func(ProgressEvent)) func(ProgressEvent) {
	if fn == nil || len(b.Rejections) == 0 {
		return fn
	}
	return func(ev ProgressEvent) {
		if ev.Index >= 0 && ev.Index < len(b.Indices) {
			ev.Index = b.Indices[ev.Index]
		}
		ev.Total = b.Total
		ev.Done += len(b.Rejections)
		fn(ev)
	}
}

// rejectedResult is what the caller receives for a subtask the intake refused: a contract
// class defer with the rejection as its reason, on no node and no seat. Unplaced marks it as
// a result no node ran (a surface that tells "the node answered and deferred" from "no node
// was exercised" reads that flag), and the placement says so in words.
func rejectedResult(reason string) PlacedResult {
	return PlacedResult{
		Unplaced:        true,
		PlacementReason: "not placed: refused at intake, before any node or seat was chosen",
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassContract,
			Reason:        reason,
		},
	}
}

// Splice puts the rejected subtasks back among the results of the valid ones, so the call
// answers one result per input subtask, in input order. results and sum are what the engine
// returned for Contracts; the merged summary counts each rejection as Deferred and nothing
// else, because the fleet is healthy and the caller has a contract to fix
// (Summary.Infrastructure). label and ids word the reasons as in FlatReason.
//
// results normally holds one entry per valid contract. When it holds fewer (RunBatched
// returned an error with the results it had collected), the merge ends at the first valid
// subtask with no result: what lies beyond it was never attempted, and a rejection there is
// counted as Skipped with the rest instead of being published ahead of results that do not
// exist.
func (b PreparedBatch) Splice(results []PlacedResult, sum Summary, label string, ids []int) ([]PlacedResult, Summary) {
	if len(b.Rejections) == 0 {
		return results, sum
	}
	out := make([]PlacedResult, 0, b.Total)
	rej, valid := 0, 0
	for pos := 0; pos < b.Total; pos++ {
		if rej < len(b.Rejections) && b.Rejections[rej].Index == pos {
			out = append(out, rejectedResult(b.Rejections[rej].text(label, ids)))
			sum.Deferred++
			rej++
			continue
		}
		if valid >= len(results) {
			// First valid subtask without a result: it and everything after it was never
			// attempted. The rejections before it are published above; the ones still
			// ahead go to Skipped, with the valid subtasks that were cut.
			sum.Skipped += len(b.Rejections) - rej
			return out, sum
		}
		out = append(out, results[valid])
		valid++
	}
	return out, sum
}

// CheckSubtaskCount is the subtask cap of a plain call (MaxSubtasks, 8) read off the number
// of subtasks the caller SENT: the same refusal, in the same words, RunWith gives for a
// call over the cap. A surface that prepares subtasks one by one calls it first, so that a
// call of nine with one bad subtask is refused for its size, as a call of nine good ones is.
func CheckSubtaskCount(n int) error {
	return checkSubtaskLimit(n, maxSubtasks)
}

// checkSubtaskLimit is the bound itself, shared by RunWith/RunBatched (their limit) and
// CheckSubtaskCount so the words cannot drift.
func checkSubtaskLimit(n, limit int) error {
	if n > limit {
		return fmt.Errorf("delegate: %d subtasks exceeds the max of %d", n, limit)
	}
	return nil
}
