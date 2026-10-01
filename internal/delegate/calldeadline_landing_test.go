package delegate

// A re-placement the call deadline cut before any node was asked (ADR 0065).
//
// attempt starts nothing once the deadline has passed, so a re-placement that reaches it late is
// published as the deadline's defer, marked Unplaced: nobody took the subtask. The loop that
// re-places a refused subtask filed every attempt that was not itself refused as the one some node
// TOOK, so the result said "re-placed after N refusal(s)" for a placement that never happened (the
// reason beside it says it had not been placed on a seat), and Summary.Replaced counted it. An
// attempt that ended the chain this way is filed the way exhausted() files a chain nobody took.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// replacementCutAtEntry runs one subtask through placeAndRun against nodes of which the first
// `refused` dispatches anywhere are refused at their address (404: re-placeable, not capacity), and
// lets the call's deadline pass in the one window between the last re-placement being chosen and
// dispatched (the beforeForced seam), so the attempt that follows is cut at entry. It returns the
// published result, the placements ledger and the number of dispatches the nodes saw.
func replacementCutAtEntry(t *testing.T, refused int64, nodes int) (PlacedResult, *placements, int64) {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var dispatches atomic.Int64
	tune := func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			if dispatches.Add(1) <= refused {
				return http.StatusNotFound
			}
			return 0
		}
	}
	var urls []string
	for _, id := range []string{"node-a", "node-b", "node-c"}[:nodes] {
		_, url := acceptingNode(t, id, "an answer", tune)
		urls = append(urls, url)
	}
	dl := newCallDeadline(t.Context(), &RunOptions{Deadline: time.Now().Add(time.Second)}, 1)
	ctx, cancel := context.WithDeadlineCause(t.Context(), dl.at, ErrCallDeadline)
	defer cancel()
	r := &runner{cfg: testCfg(t), route: "remote", remotes: urls, local: neverLocal(t), call: dl}
	var chosen atomic.Int64
	r.beforeForced = func(string) {
		if chosen.Add(1) == refused { // the re-placement after the last refusal
			time.Sleep(time.Until(dl.at) + 30*time.Millisecond)
		}
	}
	pl := newPlacements()
	pr := r.placeAndRun(ctx, 0, plainContract(), nil, time.Now(), 30, pl)
	return pr, pl, dispatches.Load()
}

// TestAReplacementTheCallDeadlineCutBeforeItBeganIsNotNarratedAsReplaced: node A refuses the first
// dispatch, the subtask is to be re-placed on node B, and the call's deadline passes before the
// re-placement begins. Nothing took it and nothing was re-placed: the published result says neither.
func TestAReplacementTheCallDeadlineCutBeforeItBeganIsNotNarratedAsReplaced(t *testing.T) {
	pr, pl, dispatches := replacementCutAtEntry(t, 1, 2)

	if dispatches != 1 || pl.attempts != 2 {
		t.Fatalf("dispatches %d attempts %d, want the refused dispatch and the cut re-placement that sent nothing (premise)", dispatches, pl.attempts)
	}
	if !pr.deadlineCut || !pr.Unplaced || !pr.Result.Deferred || !strings.HasPrefix(pr.Result.Reason, callDeadlinePrefix) {
		t.Fatalf("published = cut %v unplaced %v reason %q, want the deadline's defer for a subtask no node took", pr.deadlineCut, pr.Unplaced, pr.Result.Reason)
	}
	if pr.Replacements != 0 || pr.ReplacementNote != "" {
		t.Fatalf("replacements = %d note = %q, want neither: the single refusal had nowhere to go, as for exhausted()", pr.Replacements, pr.ReplacementNote)
	}
}

// TestAChainTheCallDeadlineCutBeforeTheNextPlacementKeepsWhatWasRefused: two nodes refused and the
// call's deadline passed before the third was asked. One re-placement happened (and was refused);
// the note says what was refused and that no node took it, in the words exhausted() uses.
func TestAChainTheCallDeadlineCutBeforeTheNextPlacementKeepsWhatWasRefused(t *testing.T) {
	pr, pl, dispatches := replacementCutAtEntry(t, 2, 3)

	if dispatches != 2 || pl.attempts != 3 {
		t.Fatalf("dispatches %d attempts %d, want two refused dispatches and the cut third placement (premise)", dispatches, pl.attempts)
	}
	if !pr.deadlineCut || !pr.Unplaced {
		t.Fatalf("published = cut %v unplaced %v, want the deadline's defer for a subtask no node took", pr.deadlineCut, pr.Unplaced)
	}
	if pr.Replacements != 1 || !strings.HasPrefix(pr.ReplacementNote, "2 refusal(s) and no node took it — ") || strings.Contains(pr.ReplacementNote, "re-placed after") {
		t.Fatalf("replacements = %d note = %q, want one re-placement and the two refusals worded for a subtask nobody ran", pr.Replacements, pr.ReplacementNote)
	}
}

// TestALandingIsNarratedAsReplacedOnlyWhenSomethingTookIt: landedAfterWait files the history of the
// attempt that ended a refusal chain without being refused. Built with the real cut, so the fixtures
// are what the engine publishes. Only an attempt the deadline cut before any node was asked changes;
// everything that was placed keeps the wording and the count it had.
func TestALandingIsNarratedAsReplacedOnlyWhenSomethingTookIt(t *testing.T) {
	r := &runner{cfg: testCfg(t), route: "remote", local: neverLocal(t), call: pastDeadline(1)}
	cutAtEntry := r.attempt(t.Context(), 0, plainContract(), nil)
	if !cutAtEntry.deadlineCut || !cutAtEntry.Unplaced {
		t.Fatalf("attempt after the deadline = cut %v unplaced %v, want the deadline's unplaced defer (fixture)", cutAtEntry.deadlineCut, cutAtEntry.Unplaced)
	}
	cutWhileRunning := r.cutByDeadline(PlacedResult{Node: "node-b", Seat: "remote-seat", ranLocal: true, Err: "canceled: context deadline exceeded"})
	if !cutWhileRunning.deadlineCut || cutWhileRunning.Unplaced {
		t.Fatalf("cut of a run = cut %v unplaced %v, want a cut that is not Unplaced (fixture)", cutWhileRunning.deadlineCut, cutWhileRunning.Unplaced)
	}
	// What the composite box's own decision files when it refuses the layer of a re-placement: the seat
	// was asked, and nothing ran. Not the deadline's.
	decided := PlacedResult{Node: "this-box", Unplaced: true, Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassContract, Reason: "no layer window holds the contract"}}

	one, two := []string{"node-a: refused"}, []string{"node-a: refused", "node-b: refused"}
	for _, tc := range []struct {
		name         string
		pr           PlacedResult
		refusals     []string
		replacements int
		note         string // prefix; "" = none
	}{
		{"cut before any node was asked, one refusal", cutAtEntry, one, 0, ""},
		{"cut before any node was asked, two refusals", cutAtEntry, two, 1, "2 refusal(s) and no node took it — node-a: refused; node-b: refused"},
		{"cut while it was running", cutWhileRunning, one, 1, "re-placed after 1 refusal(s) — node-a: refused"},
		{"deferred by the local seat's own decision", decided, two, 2, "re-placed after 2 refusal(s) — node-a: refused; node-b: refused"},
		{"taken by a node", PlacedResult{Node: "node-b", Result: localOK()}, one, 1, "re-placed after 1 refusal(s) — node-a: refused"},
		{"no refusals behind it", PlacedResult{Node: "node-b", Result: localOK()}, nil, 0, ""},
		{"cut with no refusals behind it", cutAtEntry, nil, 0, ""},
	} {
		got := r.landedAfterWait(tc.pr, 3*time.Second, tc.refusals)
		if got.Replacements != tc.replacements || got.ReplacementNote != tc.note {
			t.Errorf("%s: replacements = %d note = %q, want %d and %q", tc.name, got.Replacements, got.ReplacementNote, tc.replacements, tc.note)
		}
		if !got.waited || got.CapacityWaitSec != 3 {
			t.Errorf("%s: waited %v capacity_wait_sec %v, want the wait recorded on the result whatever became of the attempt", tc.name, got.waited, got.CapacityWaitSec)
		}
	}
}
