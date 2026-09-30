package delegate

// A refusal chain produced after the whole-call deadline (ADR 0065 decision 2).
//
// exhausted() builds the outcome of a subtask NO node took: a failure that names every node
// that refused and why the delegator stopped asking. It is produced in placeAndRun's loop and
// in the capacity wait, outside finish and settle, the two places the deadline's answer is
// applied, so a chain closed after the deadline had passed was published as "placement
// refused" (and flagged a one-subtask call as an error) for a call that only ran out of time,
// often because the very fleet read that would have found a node was the thing the deadline
// ended. exhaustedSettled is the one way out of those loops now; the run-level tests that
// drive it through real fleet reads are in calldeadline_holds_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Until the deadline has passed the chain is exhausted() exactly, and it writes no row: the
// last refused attempt's own row stands as the record, as it always did.
func TestExhaustedSettledIsExhaustedUntilTheCallDeadlinePasses(t *testing.T) {
	last := PlacedResult{Node: "node-a", Seat: "remote-seat", JobID: "agd-last", ranBase: "http://192.0.2.1:1",
		refused: true, refusalStatus: 404, Err: "dispatch: status 404"}
	refusals := []string{"node-a: dispatch: status 404"}
	const why = "no further eligible remote was available, and route=remote never falls back to local"
	for name, call := range map[string]*callDeadline{
		"no deadline":            nil,
		"a deadline still ahead": newCallDeadline(t.Context(), deadlineIn(time.Hour), 1),
	} {
		cfg := testCfg(t)
		r := &runner{cfg: cfg, call: call}
		got := r.exhaustedSettled(core.AgentContract{Goal: "say done"}, last, refusals, why, time.Now())
		if want := exhausted(last, refusals, why); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: outcome = %+v, want exactly what exhausted() builds (%+v)", name, got, want)
		}
		if _, err := os.Stat(filepath.Join(cfg.BaseDir(), "delegation-log")); err == nil {
			t.Errorf("%s: a row was written for a chain the deadline did not end: the refused attempt's row stands alone", name)
		}
	}
}

// After the deadline the chain is the deadline's outcome, whatever shape of refused attempt it
// was built from. What the attempt left on the result says where THAT attempt was (the local
// seat's capacity defer, a job a node acked and then took back, a lease's sentinel), not where
// the subtask is: nothing ran it, so the cut names no node and no seat, never narrates "was
// still running on the local seat" or "was still on node-a", leaves no intent behind, and is
// recorded once under a job id of its own.
func TestExhaustedSettledCutsEveryShapeOfRefusedAttemptAsNeverPlaced(t *testing.T) {
	const why = "no further eligible remote was available, and route=remote never falls back to local"
	cases := []struct {
		name     string
		last     PlacedResult
		refusals []string
		quoted   string // what the cut must still quote of the chain, behind the deadline's marker
	}{
		{
			name: "a node refused at dispatch",
			last: PlacedResult{Node: "node-a", Seat: "remote-seat", JobID: "agd-last", ranBase: "http://192.0.2.1:1",
				refused: true, refusalStatus: 404, Err: "dispatch: status 404"},
			refusals: []string{"node-a: dispatch: status 404"},
			quoted:   "placement refused",
		},
		{
			name: "a node took the job back after acking it",
			last: PlacedResult{Node: "node-a", Seat: "remote-seat", JobID: "agd-last", ranBase: "http://192.0.2.1:1",
				intentRecorded: true, withdrawn: true, refused: true, refusalStatus: 503, queuedWait: 40 * time.Second,
				Err: "queue deadline after 40s: the node accepted the job but never started it; the job was withdrawn from the node, which will never run it"},
			refusals: []string{"node-a: queue deadline after 40s: the job was withdrawn from the node"},
			quoted:   "placement refused",
		},
		{
			name: "the local seat deferred it as capacity",
			last: PlacedResult{Node: "this-box", Seat: "local-seat", JobID: "agd-last", ranLocal: true,
				Result: core.AgentWireResult{NodeID: "this-box", Seat: "local-seat", Deferred: true,
					DeferClass: core.DeferClassCapacity, Reason: "local seat line full"}},
			refusals: []string{"this-box (local seat): deferred (capacity): local seat line full"},
			quoted:   "reported capacity",
		},
		{
			name: "a lease's sentinel, with nothing ever dispatched",
			last: PlacedResult{waitCapacity: true, pendingReason: "the local seat is reserved by a text lease",
				PlacementReason: "the local seat is reserved by a text lease"},
			refusals: []string{"the local seat is reserved by a text lease"},
			quoted:   "placement refused",
		},
	}
	for _, tc := range cases {
		cfg := testCfg(t)
		r := &runner{cfg: cfg, call: pastDeadline(1)}
		pr := r.exhaustedSettled(core.AgentContract{Goal: "say done"}, tc.last, tc.refusals, why, time.Now().Add(-250*time.Millisecond))

		res := pr.Result
		if !pr.deadlineCut || pr.Err != "" || !res.Deferred || res.DeferClass != core.DeferClassBudget {
			t.Errorf("%s: outcome cut %v err %q deferred %v class %q, want the call-deadline budget defer", tc.name, pr.deadlineCut, pr.Err, res.Deferred, res.DeferClass)
			continue
		}
		if !strings.HasPrefix(res.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(res.Reason, "had not been placed on a seat") || !strings.Contains(res.Reason, tc.quoted) {
			t.Errorf("%s: reason = %q, want the deadline's opening, that nothing was placed, and %q quoted", tc.name, res.Reason, tc.quoted)
		}
		if strings.Contains(res.Reason, "was still running") || strings.Contains(res.Reason, "was still on") || strings.Contains(res.Reason, "it was cancelled") {
			t.Errorf("%s: reason = %q narrates a run the subtask never had", tc.name, res.Reason)
		}
		if !pr.Unplaced || pr.Node != "" || pr.Seat != "" || res.NodeID != "" || res.Seat != "" {
			t.Errorf("%s: unplaced %v node %q seat %q (wire %q/%q), want an unplaced result that names no node and no seat", tc.name, pr.Unplaced, pr.Node, pr.Seat, res.NodeID, res.Seat)
		}
		if !strings.HasPrefix(pr.JobID, "agd-") || pr.JobID == tc.last.JobID {
			t.Errorf("%s: job id %q, want a freshly minted one (the refused attempt's %q belongs to its own row)", tc.name, pr.JobID, tc.last.JobID)
		}
		if pr.intentRecorded || pr.orphanable || pr.withdrawn || pr.nodeNeverRan != "" || fleetJobIDOf(pr) != "" {
			t.Errorf("%s: the cut carries the refused attempt's intent or fleet job (%+v): no node holds this id", tc.name, pr)
		}
		if pr.wallMs < 250 {
			t.Errorf("%s: wall %d ms, want at least the 250 ms since the span began", tc.name, pr.wallMs)
		}
		var rows int
		for _, line := range corpusLines(t, cfg) {
			if line.JobID != pr.JobID {
				continue
			}
			rows++
			if !line.Deferred || line.Result == nil || !strings.HasPrefix(line.Result.Reason, deadlinePrefix+"1 unfinished") {
				t.Errorf("%s: the corpus row = %+v, want the call-deadline defer", tc.name, line)
			}
		}
		if rows != 1 {
			t.Errorf("%s: %d corpus row(s) under the closing job id, want exactly 1", tc.name, rows)
		}
	}
}

// replacementNode's "no further eligible remote was available" is a claim about the fleet that
// a read ending at the deadline cannot support: it would accuse nodes that were never asked. Once
// the call's deadline has passed the sentence says what it can, and without it the claim stands.
func TestAReplacementReadAfterTheCallDeadlineDoesNotClaimNoNodeWasEligible(t *testing.T) {
	const claim = "no further eligible remote was available"
	for name, tc := range map[string]struct {
		call      *callDeadline
		wantClaim bool
	}{
		"no deadline":          {nil, true},
		"the deadline passed":  {pastDeadline(1), false},
		"the deadline is away": {newCallDeadline(t.Context(), deadlineIn(time.Hour), 1), true},
	} {
		r := &runner{cfg: testCfg(t), route: "remote", call: tc.call}
		_, why, ok := r.replacementNode(t.Context(), remoteContract(), newPlacements(), 1)
		if ok {
			t.Fatalf("%s: replacementNode found a node in an empty fleet", name)
		}
		if got := strings.Contains(why, claim); got != tc.wantClaim {
			t.Errorf("%s: why = %q, claims no eligible remote = %v, want %v", name, why, got, tc.wantClaim)
		}
		if !tc.wantClaim && !strings.Contains(why, "deadline") {
			t.Errorf("%s: why = %q, want it to say the call's deadline had passed", name, why)
		}
	}
}

// Every place a subtask's placement ends with nobody having taken it goes through
// exhaustedSettled. Six of them close a chain (the re-placement loop's three, the capacity
// wait's two and the decided seat's). Two are reached through a fleet read the deadline can end
// (the selection that names no node, and the wait switched off) and have run-level tests; the
// other four are the budget floor's closings, where nothing slow sits between the last attempt
// and the check, so no fixture can put the deadline inside that window. Those are held by the
// invariant itself: a bare exhausted() call anywhere in the package's own code is a return that
// skips the deadline's answer. The count of calls inside the helper is what keeps this from
// passing vacuously.
func TestEveryExhaustedPlacementPassesThroughTheDeadlineCut(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no package files found (%v)", err)
	}
	fset := token.NewFileSet()
	var inHelper int
	var bare []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "exhausted" {
					if fn.Name.Name == "exhaustedSettled" {
						inHelper++
					} else {
						bare = append(bare, fset.Position(call.Pos()).String())
					}
				}
				return true
			})
		}
	}
	if inHelper != 1 {
		t.Fatalf("exhaustedSettled calls exhausted() %d time(s), want exactly 1: the guard no longer finds the helper", inHelper)
	}
	if len(bare) > 0 {
		t.Fatalf("exhausted() is called outside exhaustedSettled at %v: a placement that ends there skips the call deadline's answer (ADR 0065 decision 2)", bare)
	}
}
