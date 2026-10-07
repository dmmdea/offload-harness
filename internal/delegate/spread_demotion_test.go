package delegate

// The spread deal reads what betterRemote reads first: lease demotion and saturation (the diagnosis'
// F09, ADR 0050's rule that auto and spread answer "which seat is best" alike).
//
// route=spread picked among the remotes by free-card tier and fit score only. A node reporting
// saturation.high (draining, or its admission queue at the ceiling) or sitting under a long or overdue
// text lease kept its slot of the cycle while a healthy node idled: the dispatch was refused, the node
// cooled, one of the subtask's re-placements was spent, and the cycle's order was gone. Both are
// demotions, never exclusions: a demoted node still takes its slot when nothing better is left in the
// cycle (the delegator's copy of those numbers is stale by construction).

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// spreadSeats deals n copies of the mechanical goal across the run's remotes with the local seat out of
// the rotation (a text lease on it), and returns the node each subtask landed on.
func spreadSeats(r *runner, n int) []string {
	r.spreadLease.Held = true
	r.spreadLease.Class = "text"
	contracts := make([]core.AgentContract, n)
	for i := range contracts {
		contracts[i] = fitSubtask(fitMechGoal, 100).Contract
	}
	where := make([]string, n)
	for i, sl := range r.dealSpread(contracts, fitLocal()) {
		where[i] = sl.view.NodeID
	}
	return where
}

func demotionFixture(id string) NodeView {
	v := fitMidRemote
	v.NodeID = id
	v.MaxConcurrentJobs, v.MaxQueueDepth = 4, 8
	return v
}

// TestSpreadDealDemotesASaturatedNodeBehindOneWithRoom is the S11 proof of the diagnosis: with a
// saturated node listed FIRST in the roster, a rotation tie would hand it the first slot.
func TestSpreadDealDemotesASaturatedNodeBehindOneWithRoom(t *testing.T) {
	sat := demotionFixture("saturated")
	sat.SaturationKnown, sat.SaturationHigh = true, true
	sat.JobsRunning, sat.QueueDepth = 1, 1
	ok := demotionFixture("healthy")

	if got := spreadSeats(fitRunner(sat, ok), 1); got[0] != "healthy" {
		t.Fatalf("the first slot went to %q, want the node with room: the saturated node's dispatch would be refused", got[0])
	}
	// By its own admission ceiling too: queue_depth at max_queue_depth is a certain refusal.
	full := demotionFixture("queue-full")
	full.QueueDepth = full.MaxQueueDepth
	if got := spreadSeats(fitRunner(full, ok), 1); got[0] != "healthy" {
		t.Fatalf("the first slot went to %q, want the node with room over one at its admission ceiling", got[0])
	}
	// Demoted, never excluded: the cycle gives every remote a slot, the saturated one last.
	if got := spreadSeats(fitRunner(sat, ok), 2); got[0] != "healthy" || got[1] != "saturated" {
		t.Fatalf("a two-slot cycle dealt %v, want [healthy saturated]: a demoted node still takes its slot when nothing better is left", got)
	}
}

// TestSpreadDealDemotesALongLeaseAndAnOverdueOneFurther: a node under a long text lease ranks behind a
// free one, and a node whose lease is overdue (its declared end has passed, so nothing it says about
// the card freeing is evidence) behind both.
func TestSpreadDealDemotesALongLeaseAndAnOverdueOneFurther(t *testing.T) {
	overdue := demotionFixture("overdue")
	overdue.LeaseOverdue = true
	longLease := demotionFixture("long-lease")
	longLease.LeaseBusy, longLease.LeasedText = true, true
	free := demotionFixture("free")

	for _, roster := range [][]NodeView{{overdue, longLease, free}, {free, longLease, overdue}, {longLease, overdue, free}} {
		got := spreadSeats(fitRunner(roster...), 3)
		if got[0] != "free" || got[1] != "long-lease" || got[2] != "overdue" {
			t.Fatalf("roster %v dealt %v, want [free long-lease overdue]", []string{roster[0].NodeID, roster[1].NodeID, roster[2].NodeID}, got)
		}
	}
}

// TestSpreadDealIsUnchangedWhenNoNodeIsDemoted: with nothing saturated or leased the rotation and the
// fit order are exactly what they were - the keys are demotions, not a new ranking.
func TestSpreadDealIsUnchangedWhenNoNodeIsDemoted(t *testing.T) {
	a, b, c := demotionFixture("a"), demotionFixture("b"), demotionFixture("c")
	if got := spreadSeats(fitRunner(a, b, c), 3); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("an all-equal roster dealt %v, want the rotation order [a b c]", got)
	}
}
