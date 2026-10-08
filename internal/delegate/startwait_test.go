package delegate

// One number prices a NEW job's wait for a worker (ADR 0073, the diagnosis' F10).
//
// A node published queue_wait_estimate_sec, the wait of the deepest job ALREADY queued (excess x wall /
// workers), and the delegator preferred it outright although its own formula prices a new job one slot
// deeper (running + queued - workers + 1). With 4 running and 3 queued behind a 300 s wall on 4 workers
// the node said 225 s and a new job waited 300 s, so the ranking, the backlog gate and the queue budget
// under-priced exactly the slow nodes with the longest queues. At depth == max_concurrent_jobs the
// estimate was 0 and omitted while a new job still waited for a worker to retire. A node now also
// publishes new_job_wait_sec, the wait of the arrival, which a delegator believes as it stands; one that
// reads an older node's estimate adds the slot itself. Every combination of old and new on either side
// prices the new job at the same number.

import (
	"context"
	"strings"
	"testing"
)

// fullAndQueuedNode is a node with the counters of the example above: 4 running, 3 queued, 4 workers, a 300 s wall.
func fullAndQueuedNode() NodeView {
	return NodeView{JobsRunning: 4, JobsQueued: 3, MaxConcurrentJobs: 4, RecentAgentWallSec: 300}
}

// TestAPublishedNewJobWaitIsBelievedAsItStands: the node already counted the slot.
func TestAPublishedNewJobWaitIsBelievedAsItStands(t *testing.T) {
	v := fullAndQueuedNode()
	// A figure unlike the node's own counters, so reading anything else cannot look right: the node knows its
	// live backlog better than arithmetic over a health snapshot, and it is believed outright.
	forNew := 111.0
	v.NewJobWaitSec = &forNew
	old := 225.0
	v.QueueWaitEstimateSec = &old // a node that publishes both: the new field wins, the old is not added to it
	if got := queueWaitFor(v); got != 111 {
		t.Fatalf("queueWaitFor = %v, want new_job_wait_sec as it stands (111), not 111 + a slot, the older 225 + a slot, or the counters' 300", got)
	}
	if sec, known := etaStartFor(v); !known || sec != 111 {
		t.Fatalf("etaStartFor = %v/%v, want 111/known", sec, known)
	}
}

// TestAnOlderNodesEstimateIsPricedOneSlotDeeperForANewJob: the node says 225 s (excess 3 x 300 s / 4) and
// the delegator is about to send the fourth job past the workers.
func TestAnOlderNodesEstimateIsPricedOneSlotDeeperForANewJob(t *testing.T) {
	v := fullAndQueuedNode()
	old := 225.0
	v.QueueWaitEstimateSec = &old
	if got := queueWaitFor(v); got != 300 {
		t.Fatalf("queueWaitFor = %v, want 300: the node's 225 s plus the slot (300 s / 4 workers = 75 s) a new job queues behind", got)
	}
	// It is the number the delegator derives from the same counters when the node publishes nothing.
	derived := fullAndQueuedNode()
	if got := queueWaitFor(derived); got != 300 {
		t.Fatalf("derived queueWaitFor = %v, want 300", got)
	}
	// One slot is wall / workers, with the workers floored at 1 like the derived arithmetic.
	oneWorker := NodeView{JobsRunning: 1, JobsQueued: 2, MaxConcurrentJobs: 0, RecentAgentWallSec: 60}
	est := 120.0
	oneWorker.QueueWaitEstimateSec = &est
	if got := queueWaitFor(oneWorker); got != 180 {
		t.Fatalf("an estimate from a node that publishes no ceiling = %v, want 120 + 60 (one worker assumed)", got)
	}
	// No slot is added to something that is not a wait to add it to: zero is "a worker is free".
	zero := 0.0
	free := NodeView{JobsRunning: 1, MaxConcurrentJobs: 4, RecentAgentWallSec: 300, QueueWaitEstimateSec: &zero}
	if got := queueWaitFor(free); got != 0 {
		t.Fatalf("queueWaitFor(published 0) = %v, want 0: nothing waits for a worker", got)
	}
	// And none when the node publishes no wall to size a slot from.
	bare := NodeView{JobsRunning: 4, JobsQueued: 3, MaxConcurrentJobs: 4, QueueWaitEstimateSec: &old}
	if got := queueWaitFor(bare); got != 225 {
		t.Fatalf("queueWaitFor with no recent wall = %v, want the estimate as published (225)", got)
	}
}

// TestAPublishedAndADerivedWaitAreTheSameNumber is the property, over a grid of node states: what a node
// publishes for a new job (the arithmetic the node side uses: (depth - workers + 1) x wall / workers, 0
// when a worker is free), what the delegator derives from the counters, and an older node's estimate
// (excess x wall / workers, omitted when 0) plus the slot are one number. fleetnode pins the same
// arithmetic on the node's own function with the same figures (newjobwait_test.go).
func TestAPublishedAndADerivedWaitAreTheSameNumber(t *testing.T) {
	for _, tc := range []struct {
		running, queued, workers int
		wall                     float64
	}{
		{4, 3, 4, 300}, {4, 0, 4, 300}, {3, 0, 4, 300}, {4, 1, 4, 120}, {1, 2, 1, 60}, {1, 0, 1, 60}, {2, 9, 2, 45.5}, {8, 8, 8, 10}, {0, 0, 4, 100},
	} {
		depth := tc.running + tc.queued
		ahead := depth - tc.workers + 1
		want := 0.0
		if ahead > 0 {
			want = float64(ahead) * tc.wall / float64(tc.workers)
		}
		counters := NodeView{JobsRunning: tc.running, JobsQueued: tc.queued, MaxConcurrentJobs: tc.workers, RecentAgentWallSec: tc.wall}

		if got := queueWaitFor(counters); got != want {
			t.Errorf("%+v: derived wait = %v, want %v", tc, got, want)
		}
		forNew := counters
		publishedNew := want
		if want > 0 {
			forNew.NewJobWaitSec = &publishedNew // a new node omits 0
		}
		if got := queueWaitFor(forNew); got != want {
			t.Errorf("%+v: published new_job_wait_sec read as %v, want %v", tc, got, want)
		}
		older := counters
		if excess := depth - tc.workers; excess > 0 {
			est := float64(excess) * tc.wall / float64(tc.workers) // an older node omits 0
			older.QueueWaitEstimateSec = &est
		}
		if got := queueWaitFor(older); got != want {
			t.Errorf("%+v: an older node's estimate read as %v, want %v (its estimate plus one slot, or the derived number at depth == workers)", tc, got, want)
		}
	}
}

// TestTheStartArithmeticSaysWhichNumberItIs: the gate prints the node's numbers, not a figure with no source.
func TestTheStartArithmeticSaysWhichNumberItIs(t *testing.T) {
	forNew, old := 300.0, 225.0
	v := fullAndQueuedNode()
	v.NewJobWaitSec = &forNew
	if got := startArithmetic(v); !strings.Contains(got, "new_job_wait_sec 300 s") {
		t.Errorf("startArithmetic with new_job_wait_sec = %q", got)
	}
	v = fullAndQueuedNode()
	v.QueueWaitEstimateSec = &old
	got := startArithmetic(v)
	for _, want := range []string{"queue_wait_estimate_sec 225 s", "deepest queued job", "one slot of 75 s"} {
		if !strings.Contains(got, want) {
			t.Errorf("startArithmetic with an older node's estimate = %q, want it to contain %q", got, want)
		}
	}
	if got := startArithmetic(fullAndQueuedNode()); !strings.Contains(got, "4 running + 3 queued - 4 worker(s) + 1 = 4 ahead") {
		t.Errorf("startArithmetic from the counters = %q", got)
	}
	// And the gate quotes it: 300 s of patience cannot cover a 300 s start plus nothing, but 299 s cannot either.
	ok, why := startsWithinPatience(v, 280*pollSecond)
	if ok || !strings.Contains(why, "~300 s") || !strings.Contains(why, "one slot of 75 s") {
		t.Errorf("the gate = %v %q, want a refusal pricing the new job at ~300 s with the slot named", ok, why)
	}
}

// TestANegativeNewJobWaitIsNoOpinion: a negative figure is a node bug, ignored like a negative estimate;
// the older estimate or the counters decide.
func TestANegativeNewJobWaitIsNoOpinion(t *testing.T) {
	bad, old := -5.0, 225.0
	v := fullAndQueuedNode()
	v.NewJobWaitSec = &bad
	v.QueueWaitEstimateSec = &old
	if got := queueWaitFor(v); got != 300 {
		t.Fatalf("queueWaitFor = %v, want the older estimate plus the slot (300): a negative new_job_wait_sec is no opinion", got)
	}
	v.QueueWaitEstimateSec = nil
	if got := queueWaitFor(v); got != 300 {
		t.Fatalf("queueWaitFor = %v, want the derived 300", got)
	}
}

// TestFetchNodeViewDecodesTheNewJobWait: present -> a pointer to the figure; absent (an older node, or a
// node with a free worker, which omits it) -> nil, never a zero the gate would read as "starts now".
func TestFetchNodeViewDecodesTheNewJobWait(t *testing.T) {
	srv := healthServer(t, `{"node_id":"n","queue_wait_estimate_sec":225,"new_job_wait_sec":300,"recent_agent_wall_sec":300}`, nil)
	v, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.NewJobWaitSec == nil || *v.NewJobWaitSec != 300 || v.QueueWaitEstimateSec == nil || *v.QueueWaitEstimateSec != 225 {
		t.Fatalf("decoded new=%v est=%v, want 300 and 225", v.NewJobWaitSec, v.QueueWaitEstimateSec)
	}
	srv2 := healthServer(t, `{"node_id":"n","queue_wait_estimate_sec":225,"recent_agent_wall_sec":300}`, nil)
	v2, err := FetchNodeView(context.Background(), srv2.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.NewJobWaitSec != nil {
		t.Fatalf("an older node decoded new_job_wait_sec = %v, want nil (absent)", *v2.NewJobWaitSec)
	}
}
