package delegate

// D-32, fix round: ran_on of the results the call deadline and the queue lane produce.
//
// ran_on reads placedWhere, and placedWhere said "nowhere" for a node's refusal by reading the refused flag. The call
// deadline's cut clears that flag (the row is a defer now, not a refusal), so a job the node had confirmed it took back
// before it started read as placed on the node, and the wire said ran_on "node" beside a reason that said it would never
// run there. The queue lane's finished answers are the other rows that carried no ran_on at all.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRanOnOfADeadlineCutJobFollowsWhatTheNodeSaid: the cut of a job a node acked keeps the node's own word (withdrawn,
// nodeNeverRan), and ran_on follows it. A confirmed withdrawal and the node's record of a job it never ran say no seat
// ran it: the key is absent. A job the node may still start (a withdraw it did not confirm, a cancel nobody followed
// up) is on the node, placed and dispatched there: node.
func TestRanOnOfADeadlineCutJobFollowsWhatTheNodeSaid(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}
	for _, tc := range []struct {
		name string
		set  func(*PlacedResult)
		want string
	}{
		{"a confirmed withdrawal", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded"
			p.refuseAsWithdrawn(0)
		}, ""},
		{"the node's own record of a job it never ran", func(p *PlacedResult) {
			p.Err = "remote job error: reaped: nobody polled it within the lease"
			p.refuseAsNeverRan("reaped: nobody polled it within the lease", 0)
		}, ""},
		{"a withdraw the node did not confirm", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded; withdraw not confirmed: HTTP 405: the node has no withdraw route"
			p.orphanable = true
		}, "node"},
		{"a cancel no give-up has marked", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded"
		}, "node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := PlacedResult{intentRecorded: true, ranBase: "http://node-a:8080", JobID: "agd-x", Node: "node-a", Seat: "seat-b"}
			tc.set(&pr)
			got := r.cutByDeadline(pr)
			if !got.deadlineCut || !got.Result.Deferred {
				t.Fatalf("cut = %+v, want the call-deadline defer", got)
			}
			if v, present := ranOnOf(t, got); v != tc.want || present != (tc.want != "") {
				t.Fatalf("ran_on = %q (present %v), want %q; the reason says %q", v, present, tc.want, got.Result.Reason)
			}
			// The hint clause and the pin tallies read the same decision, so they cannot say otherwise.
			clause := (&runner{pin: pinCall{asked: "remote", hint: true}}).stampPin(got).PlacementReason
			if placed := strings.Contains(clause, "honoured: placed on node-a"); placed != (tc.want == "node") {
				t.Fatalf("ran_on is %q but the hint clause reads %q", tc.want, clause)
			}
		})
	}
}

// TestRanOnIsAbsentForAJobTheNodeTookBackAtTheCallDeadline is the same through a real call: a node that holds the job
// queued, a deadline that passes, and a node that confirms the withdraw.
func TestRanOnIsAbsentForAJobTheNodeTookBackAtTheCallDeadline(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	url, _ := withWithdrawAnswer(t, inner, http.StatusOK, withdrawnBody)

	results, _, _ := runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)

	r := results[0]
	if !r.deadlineCut || !r.withdrawn {
		t.Fatalf("result %+v, want a deadline cut of a job the node took back", r)
	}
	if v, present := ranOnOf(t, r); present {
		t.Fatalf("ran_on = %q on a job the node confirmed it took back before it started: the key must be absent (reason %q)", v, r.Result.Reason)
	}
}
