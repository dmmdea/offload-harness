// deal_capacity_test.go: ADR 0063 / PR-11 - the spread deal counts each node's
// capacity, the process gate is shared by concurrent Runs, and a page that keeps
// failing is backed off.
//
// Capacity decides FEASIBILITY only (INV-5): a node dealt to its headroom drops
// out of the rotation, and among the nodes that remain the fit order, the deal
// cycle and the one-subtask-per-seat-per-cycle invariant are exactly what they
// were - never a preference for whichever seat is fastest.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// headroomRemote is a fit-fixture remote publishing an execution ceiling: it
// runs `running` of `max` jobs, so its headroom is max - running.
func headroomRemote(base NodeView, id string, max, running int) NodeView {
	base.NodeID = id
	base.MaxConcurrentJobs, base.JobsRunning = max, running
	return base
}

// busyLocalRunner is fitRunner with the local seat reading BUSY at deal time, so
// route=spread deals every slot to the remotes (the 0.113.20 rule) - the case the
// capacity accounting exists for: with the local seat out of the rotation a node
// dealt past its headroom has nowhere to overflow to but a queue it cannot leave.
func busyLocalRunner(views ...NodeView) *runner {
	r := fitRunner(views...)
	r.spreadLocalBusy = busyReading{busy: true, inflight: 3, note: "test"}
	return r
}

func dealCounts(slots []spreadSlot) (dealt map[string]int, waiting int) {
	dealt = map[string]int{}
	for _, sl := range slots {
		if sl.capacityWait {
			waiting++
			continue
		}
		dealt[sl.view.NodeID]++
	}
	return dealt, waiting
}

// TestDealSpreadNeverDealsMoreThanHeadroom: 6 subtasks into two remotes that
// can start 2 and 1 of them. The rotation used to deal 3 and 3 (slot := i %
// len(nodes)) whatever the nodes advertised - the very stacking that put 44 jobs
// into 13 slots. Now no node is dealt more than max_concurrent_jobs -
// jobs_running.
func TestDealSpreadNeverDealsMoreThanHeadroom(t *testing.T) {
	r := busyLocalRunner(headroomRemote(fitBigRemote, "big-remote", 2, 0), headroomRemote(fitMidRemote, "mid-remote", 1, 0))
	_, slots := deal(r, repeatGoal(fitMechGoal, 6)...)
	dealt, waiting := dealCounts(slots)
	if dealt["big-remote"] > 2 || dealt["mid-remote"] > 1 {
		t.Fatalf("dealt %v, want at most 2 to big-remote and 1 to mid-remote (their headroom)", dealt)
	}
	if got := dealt["big-remote"] + dealt["mid-remote"]; got != 3 {
		t.Fatalf("dealt %d subtasks to the remotes (%v), want all 3 free slots used", got, dealt)
	}
	if waiting != 3 {
		t.Fatalf("%d subtasks were left for the capacity wait, want the other 3", waiting)
	}
}

// TestDealSpreadOverflowGoesToTheCapacityWait: the subtasks beyond the fleet's
// free slots are not stacked on a full node or on the busy local seat: each is
// handed to the capacity wait (INV-4: a busy node is a place in line), and says
// which nodes it found at their headroom.
func TestDealSpreadOverflowGoesToTheCapacityWait(t *testing.T) {
	r := busyLocalRunner(headroomRemote(fitBigRemote, "big-remote", 2, 0), headroomRemote(fitMidRemote, "mid-remote", 1, 0))
	_, slots := deal(r, repeatGoal(fitMechGoal, 5)...)
	for i, sl := range slots {
		switch {
		case i < 3 && sl.capacityWait:
			t.Fatalf("subtask %d was sent to the capacity wait although the fleet still had a free slot: %+v", i, sl.placement)
		case i >= 3 && !sl.capacityWait:
			t.Fatalf("subtask %d (past the fleet's 3 free slots) was dealt to %q instead of the capacity wait", i, sl.view.NodeID)
		case i >= 3:
			for _, want := range []string{"already dealt to its headroom", "big-remote: cap (0/2 running, headroom 2, dealt 2)", "mid-remote: cap (0/1 running, headroom 1, dealt 1)"} {
				if !strings.Contains(sl.reason, want) {
					t.Errorf("subtask %d reason = %q, want it to contain %q", i, sl.reason, want)
				}
			}
		}
	}
}

// TestDealSpreadQualityOrderingIsUnchangedByCapacity: the guard for INV-5. With
// capacity to spare the deal is byte-for-byte the one it was; with a seat spent,
// the survivors keep their fit order (the next-smallest adequate seat takes a
// mechanical contract), and a node is never preferred because it has more room.
func TestDealSpreadQualityOrderingIsUnchangedByCapacity(t *testing.T) {
	goals := []string{fitMechGoal, fitReasonGoal, fitMechGoal, fitReasonGoal, fitMechGoal, fitMechGoal, fitReasonGoal}
	unpublished := fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
	ample := fitRunner(headroomRemote(fitBigRemote, "big-remote", 64, 0), headroomRemote(fitMidRemote, "mid-remote", 64, 0), headroomRemote(fitSmallRemote, "small-remote", 64, 0))
	want, _ := deal(unpublished, goals...)
	got, _ := deal(ample, goals...)
	if !equalStrings(got, want) {
		t.Fatalf("with capacity to spare the deal changed:\n got %v\nwant %v", got, want)
	}

	// The smallest seat can start ONE job: the first mechanical contract takes it
	// (best fit), the second and third take the next-best fits in the same cycle,
	// and the fourth - where the old deal would stack a second job on the full
	// small seat - goes to the next-smallest ADEQUATE seat, the mid one, never to
	// the big one just because it has the most room.
	spent := busyLocalRunner(headroomRemote(fitBigRemote, "big-remote", 8, 0), headroomRemote(fitMidRemote, "mid-remote", 8, 0), headroomRemote(fitSmallRemote, "small-remote", 1, 0))
	order, _ := deal(spent, repeatGoal(fitMechGoal, 4)...)
	if wantOrder := []string{"small-remote", "mid-remote", "big-remote", "mid-remote"}; !equalStrings(order, wantOrder) {
		t.Fatalf("mechanical deal with the small seat spent = %v, want %v (fit order kept among the survivors)", order, wantOrder)
	}
}

// TestRunSpreadNeverHoldsMoreOpenThanTheNodeAdmits: the whole path. Three
// subtasks, two nodes that can hold ONE job each, the local seat leased: the
// third waits in line until a node finishes, and no node ever holds two open.
func TestRunSpreadNeverHoldsMoreOpenThanTheNodeAdmits(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var openA, openB, peakA, peakB atomic.Int64
	node := func(id string, open, peak *atomic.Int64) (*fakeNode, string) {
		var polled sync.Map
		return acceptingNode(t, id, "answer from "+id, func(f *fakeNode) {
			f.maxConcurrentJobs, f.maxQueueDepth = 1, 1
			f.jobsRunningFn = func() int { return int(open.Load()) }
			f.queueDepthFn = func() int { return int(open.Load()) }
			f.onDispatch = func(string, core.AgentContract) {
				n := open.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
			}
			inner := f.pollByJob
			f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				// Running for a few polls, then done; the slot frees on the first `done`.
				cnt, _ := polled.LoadOrStore(jobID, new(atomic.Int64))
				if cnt.(*atomic.Int64).Add(1) < 4 {
					return map[string]any{"state": "running"}, http.StatusOK
				}
				if _, seen := polled.LoadOrStore("done-"+jobID, true); !seen {
					open.Add(-1)
				}
				return inner(jobID, n)
			}
		})
	}
	a, aURL := node("node-a", &openA, &peakA)
	b, bURL := node("node-b", &openB, &peakB)
	dir, _ := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 20

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	contracts := make([]core.AgentContract, 3)
	for i := range contracts {
		contracts[i] = plainContract()
	}
	_, sum, err := RunWith(ctx, cfg, neverLocal(t), contracts, "spread", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 3 || sum.Waited < 1 {
		t.Fatalf("summary = %+v, want 3 successes with at least one subtask having waited in line", sum)
	}
	if peakA.Load() > 1 || peakB.Load() > 1 {
		t.Fatalf("peak open jobs a=%d b=%d, want at most 1 each (their admission ceiling)", peakA.Load(), peakB.Load())
	}
	if a.dispatches.Load()+b.dispatches.Load() != 3 {
		t.Fatalf("dispatches a=%d b=%d, want exactly 3 in all (no refusal, no retry)", a.dispatches.Load(), b.dispatches.Load())
	}
}

// ---- the process gate -----------------------------------------------------

func TestInflightGateCountsPerBaseAndReleasesOnce(t *testing.T) {
	g := &inflightGate{open: map[string]int{}}
	r1, ok1 := g.tryAcquire("http://192.0.2.1:18811", 2)
	r2, ok2 := g.tryAcquire("http://192.0.2.1:18811", 2)
	if !ok1 || !ok2 {
		t.Fatal("the first two acquisitions under a ceiling of 2 must be admitted")
	}
	if _, ok := g.tryAcquire("http://192.0.2.1:18811", 2); ok {
		t.Fatal("a third acquisition past the ceiling must be turned away")
	}
	if g.available("http://192.0.2.1:18811", 2) {
		t.Fatal("available() must agree with tryAcquire when the node is full")
	}
	if _, ok := g.tryAcquire("http://192.0.2.2:18811", 2); !ok {
		t.Fatal("another node has its own count")
	}
	r1()
	r1() // idempotent: a second release must not free a slot it does not hold
	if got := g.load("http://192.0.2.1:18811"); got != 1 {
		t.Fatalf("load after releasing one slot twice = %d, want 1", got)
	}
	if _, ok := g.tryAcquire("http://192.0.2.1:18811", 2); !ok {
		t.Fatal("a released slot must be available again")
	}
	r2()
	if _, ok := g.tryAcquire("http://192.0.2.3:18811", 0); !ok {
		t.Fatal("a limit of 0 (unpublished ceiling) is unlimited, never a limit")
	}
}

// TestConcurrentRunsShareTheProcessGate: two RunWith calls in ONE process, each
// with two subtasks for a node that admits two jobs. Per Run each stays inside
// the node's headroom; together they put four open on it. The gate is
// process-wide, so the node never holds more than its ceiling - the other two
// subtasks wait in line for a slot and all four finish.
func TestConcurrentRunsShareTheProcessGate(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var open, peak atomic.Int64
	var polled sync.Map
	node, url := acceptingNode(t, "node-shared", "answer from the shared node", func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth = 2, 2
		f.onDispatch = func(string, core.AgentContract) {
			n := open.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
		}
		inner := f.pollByJob
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			cnt, _ := polled.LoadOrStore(jobID, new(atomic.Int64))
			if cnt.(*atomic.Int64).Add(1) < 8 { // running for ~40 ms
				return map[string]any{"state": "running"}, http.StatusOK
			}
			if _, seen := polled.LoadOrStore("done-"+jobID, true); !seen {
				open.Add(-1)
			}
			return inner(jobID, n)
		}
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 20

	start := make(chan struct{})
	var wg sync.WaitGroup
	sums := make([]Summary, 2)
	for k := range sums {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			_, sum, err := RunWith(ctx, cfg, neverLocal(t), []core.AgentContract{plainContract(), plainContract()}, "remote", []string{url}, nil)
			if err != nil {
				t.Errorf("Run %d: %v", k, err)
			}
			sums[k] = sum
		}()
	}
	close(start)
	wg.Wait()

	if got := sums[0].Succeeded + sums[1].Succeeded; got != 4 {
		t.Fatalf("summaries %+v %+v, want all 4 subtasks to succeed", sums[0], sums[1])
	}
	if peak.Load() > 2 {
		t.Fatalf("the node held %d jobs open at once, want at most its admission ceiling of 2 — the two Runs did not see each other's in-flight", peak.Load())
	}
	if node.dispatches.Load() != 4 {
		t.Fatalf("the node saw %d dispatches, want exactly 4 (waiting in line is not a refusal)", node.dispatches.Load())
	}
}

// TestProcessGateReleasesOnTerminalAndGiveUp: the gate counts a dispatch until the
// delegator is done with it - an answer, a queue deadline, a cancel - and not a
// moment longer, or a node would fill up with slots nobody is waiting on.
func TestProcessGateReleasesOnTerminalAndGiveUp(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)

	t.Run("terminal answer", func(t *testing.T) {
		_, url := acceptingNode(t, "node-done", "answer", func(f *fakeNode) { f.maxQueueDepth = 4 })
		if _, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}); err != nil || sum.Succeeded != 1 {
			t.Fatalf("Run: summary %+v err %v", sum, err)
		}
		if got := processGate.load(url); got != 0 {
			t.Fatalf("gate still holds %d slot(s) on a node whose job finished", got)
		}
	})
	t.Run("queue deadline give-up", func(t *testing.T) {
		stuck := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "node-stuck", maxQueueDepth: 4,
			pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "accepted"}, http.StatusOK },
		}
		url := stuck.server().URL
		contract := plainContract()
		contract.TimeoutSec = 1
		_, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
		if err != nil || sum.Failed != 1 {
			t.Fatalf("Run: summary %+v err %v, want the queue-deadline failure", sum, err)
		}
		if got := processGate.load(url); got != 0 {
			t.Fatalf("gate still holds %d slot(s) after the delegator gave up on the job", got)
		}
	})
	t.Run("caller cancel", func(t *testing.T) {
		running := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "node-running", maxQueueDepth: 4,
			pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
		}
		url := running.server().URL
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			// Cancel once the node holds the job, not after a fixed sleep: a cancel that
			// lands before the dispatch makes the subtask defer instead of being canceled
			// mid-run, and the gate assertion below would then prove nothing.
			for running.dispatches.Load() < 1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		results, _, err := Run(ctx, testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.HasPrefix(results[0].Err, "canceled") {
			t.Fatalf("err = %q, want the cancel", results[0].Err)
		}
		if got := processGate.load(url); got != 0 {
			t.Fatalf("gate still holds %d slot(s) after the caller canceled", got)
		}
	})
}

// TestProcessGateIsReleasedBeforeTheDelegatorRescuesTheAnswer: the gate counts what a
// NODE holds for this process, and a node's part of a job ends with its terminal answer.
// What the delegator does after that - here the re-pack the node could not finish, run
// on this box by the delegator's own rescue (rescue.go) and allowed minutes - is this
// box's work, so it must not be counted as an open slot on the node (ADR 0063 decision
// 7: the count ends with a terminal answer or with the delegator giving up). The
// previous tests read the gate once Run had returned, which a slot held through that
// step passes; this one reads it from inside the rescue.
func TestProcessGateIsReleasedBeforeTheDelegatorRescuesTheAnswer(t *testing.T) {
	base := rescueNode(t, legacyRepackStall())
	rs := &rescuer{structured: `{"answer":"42"}`}
	rescue := rs.fn()
	var held atomic.Int64
	held.Store(-1) // -1: the rescue never ran
	results, _ := runRescued(t, base, rescueContract(), func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		held.Store(int64(processGate.load(base)))
		return rescue(ctx, c, output, budget)
	})
	if rs.calls.Load() != 1 || results[0].Result.Deferred {
		t.Fatalf("fixture: rescue calls = %d, result = %+v, want one rescue that saved the answer", rs.calls.Load(), results[0].Result)
	}
	if got := held.Load(); got != 0 {
		t.Fatalf("the gate held %d slot(s) on a node whose terminal answer was already in, while the delegator re-packed it", got)
	}
	if got := processGate.load(base); got != 0 {
		t.Fatalf("gate still holds %d slot(s) after the run", got)
	}
}

// TestARescuingRunDoesNotHoldASiblingOutOfAnIdleNode: the same defect seen from the next
// Run in the process. The node admits one job (max_queue_depth 1). Run 1's job is finished
// on it and Run 1 is re-packing the answer on this box, so the node is idle. A second Run
// must be dispatched to it at once. With the slot held through the rescue the gate turned
// the sibling away for its whole placement wait and published a "no node had room" defer
// for a node that had room all along - the gate that protects a node starving it.
func TestARescuingRunDoesNotHoldASiblingOutOfAnIdleNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	var firstJob atomic.Value // the job id Run 1 dispatched
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "node-rescuing", maxQueueDepth: 1,
		onDispatch: func(jobID string, _ core.AgentContract) { firstJob.CompareAndSwap(nil, jobID) },
		pollByJob: func(jobID string, _ int64) (map[string]any, int) {
			if jobID == firstJob.Load() { // Run 1's job: a finished answer whose re-pack failed on the node
				return doneWire(t, legacyRepackStall()), http.StatusOK
			}
			return doneWire(t, remoteWire("the sibling's answer", `{"answer":"the sibling's answer"}`)), http.StatusOK
		},
	}
	url := node.server().URL

	inRescue, unpark := make(chan struct{}), make(chan struct{})
	var entered, unparked sync.Once
	rescue := func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		entered.Do(func() { close(inRescue) })
		select { // parked until the sibling has been served, never past the test
		case <-unpark:
		case <-ctx.Done():
		}
		return Rescued{Structured: []byte(`{"answer":"42"}`), Seat: "local-seat", How: "one re-pack completion"}, nil
	}
	cfg1, cfg2, local := testCfg(t), testCfg(t), neverLocal(t)
	cfg2.AgentPlacementWaitSec = 1 // a turned-away sibling gives up after one second, not the default wait
	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _, _ = RunWith(t.Context(), cfg1, local, []core.AgentContract{rescueContract()}, "remote", []string{url}, &RunOptions{Rescue: rescue})
	}()
	// Whatever ends the test, Run 1 is let go and waited for. t.Context() is already
	// canceled when a cleanup runs, so even a Run 1 stuck elsewhere unwinds: a fixture
	// that fails must fail the test, never hang the suite.
	t.Cleanup(func() {
		unparked.Do(func() { close(unpark) })
		<-first
	})
	select {
	case <-inRescue:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: Run 1 never reached its rescue")
	}

	dispatchedBefore := node.dispatches.Load()
	results, sum, err := RunWith(t.Context(), cfg2, local, []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	r := results[0]
	if got := node.dispatches.Load() - dispatchedBefore; got != 1 || sum.Succeeded != 1 {
		t.Fatalf("the sibling Run made %d dispatch(es) to the idle node while Run 1 re-packed its answer; summary %+v, err %q, reason %.200q; want 1 dispatch and a success",
			got, sum, r.Err, r.Result.Reason)
	}
}

// ---- the per-page retry cap -----------------------------------------------

// pageContract is a research-door digest contract for one page: the door and the
// page text are what identify it; acceptance can never pass (the node answers
// something else), so every issue of it fails verification.
func pageContract(page string) core.AgentContract {
	c := verifiedContract()
	c.Door = "offload_research"
	c.Context = []core.ContextDoc{{Name: "01-example.txt", Text: page}}
	return c
}

var pageSeq atomic.Int64

// uniquePage returns page text no other test - and no earlier iteration of the
// same test under -count=N - has used. pageRetries is process-wide and has no
// reset switch, so a page keyed on t.Name() was backed off from the second pass on
// and the test failed for a reason that had nothing to do with the code under test.
func uniquePage(label string) string {
	return label + " (" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(pageSeq.Add(1), 10) + ")"
}

// failingPageNode answers every job with an answer that fails acceptance.
func failingPageNode(t *testing.T) (*fakeNode, string) {
	return acceptingNode(t, "node-page", "an unrelated answer", nil)
}

// TestPerPageRetryCapBacksOff: the same page issued 24 times (the worst page on
// 09-29 took 24 attempts) runs three times - the original and two re-issues -
// and is then backed off with a defer that says why, so a page no seat can digest
// stops consuming the fleet.
func TestPerPageRetryCapBacksOff(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := failingPageNode(t)
	page := uniquePage("the page nobody can digest")
	var backedOff int
	var localCalls atomic.Int64 // the verification retry runs on the local seat and fails there too
	for issue := 1; issue <= 24; issue++ {
		results, _, err := Run(t.Context(), testCfg(t), failingLocal(&localCalls), []core.AgentContract{pageContract(page)}, "remote", []string{url})
		if err != nil {
			t.Fatalf("issue %d: %v", issue, err)
		}
		pr := results[0]
		if strings.Contains(pr.Result.Reason, "page retry cap") {
			backedOff++
			if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassContract || !pr.Unplaced {
				t.Fatalf("issue %d: backed-off result = %+v, want a contract-class defer nobody ran", issue, pr.Result)
			}
		}
	}
	if got := node.dispatches.Load(); got != int64(pageMaxIssues) {
		t.Fatalf("the node saw %d dispatches for 24 issues of one failing page, want %d (the original and two re-issues)", got, pageMaxIssues)
	}
	if backedOff != 24-pageMaxIssues {
		t.Fatalf("%d issues were backed off, want %d", backedOff, 24-pageMaxIssues)
	}
}

// TestPerPageRetryCapCountsASeatDownIssueWhoseRetrySeatRanItAndFailed: an issue counts against its
// page when a seat RAN it and produced no verified digest (ADR 0063 decision 8), and a seat-down
// issue has two attempts. The first is the dead seat's: it says nothing about the page. The retry
// is another node's, it ran the page and failed verification, and because the published result is
// the FIRST attempt (mergeAttempts) the cap read only a seat-down defer and counted nothing: a page
// that no seat could digest, on a fleet that kept losing a seat, was re-issued without end, at two
// seat runs an issue.
func TestPerPageRetryCapCountsASeatDownIssueWhoseRetrySeatRanItAndFailed(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		return doneWire(t, seatDownWire("node-a", 1)), http.StatusOK
	}
	page := uniquePage("the page the retry seat cannot digest either")
	var localCalls atomic.Int64 // the retry runs on the local seat and fails verification there
	const issues = 6
	var backedOff int
	for issue := 1; issue <= issues; issue++ {
		results, _, err := Run(t.Context(), testCfg(t), failingLocal(&localCalls), []core.AgentContract{pageContract(page)}, "remote", []string{urlA})
		if err != nil {
			t.Fatalf("issue %d: %v", issue, err)
		}
		if strings.Contains(results[0].Result.Reason, "page retry cap") {
			backedOff++
		}
	}
	if got := nodeA.dispatches.Load(); got != int64(pageMaxIssues) {
		t.Fatalf("the node whose seat went down saw %d dispatches for %d issues of one page that fails on the retry seat too, want %d (the original and two re-issues)", got, issues, pageMaxIssues)
	}
	if got := localCalls.Load(); got != int64(pageMaxIssues) {
		t.Fatalf("the retry seat ran the page %d times, want %d", got, pageMaxIssues)
	}
	if backedOff != issues-pageMaxIssues {
		t.Fatalf("%d issues were backed off, want %d", backedOff, issues-pageMaxIssues)
	}
}

// The other half: a seat-down issue whose retry did NOT fail the page is not counted, so the flag
// that carries the retry's verdict must be set from the retry's own result, never unconditionally. A
// retry that recovers forgets the page; a retry the local seat declined for capacity never ran it.
func TestPerPageRetryCapDoesNotCountASeatDownIssueWhoseRetryDidNotFailThePage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry func(calls *atomic.Int64) LocalRunner
	}{
		{"the retry recovers it", func(calls *atomic.Int64) LocalRunner {
			return func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
				calls.Add(1)
				return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
					Output: "verified digest", Structured: json.RawMessage(`{"answer":"verified digest"}`), StopReason: "done"}, nil
			}
		}},
		{"the retry seat never ran it", capacityDeferLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "unused")
			nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				return doneWire(t, seatDownWire("node-a", 1)), http.StatusOK
			}
			page := uniquePage("the page whose retry did not fail it")
			var localCalls atomic.Int64
			const issues = 6
			for issue := 1; issue <= issues; issue++ {
				results, _, err := Run(t.Context(), testCfg(t), tc.retry(&localCalls), []core.AgentContract{pageContract(page)}, "remote", []string{urlA})
				if err != nil {
					t.Fatalf("issue %d: %v", issue, err)
				}
				if strings.Contains(results[0].Result.Reason, "page retry cap") {
					t.Fatalf("issue %d was backed off although no seat ran the page and failed it", issue)
				}
			}
			if got := nodeA.dispatches.Load(); got != issues {
				t.Fatalf("the node saw %d dispatches for %d issues, want every one", got, issues)
			}
		})
	}
}

// TestPerPageRetryCapForgetsAfterASuccessAndAfterTheBackoff, and never caps a
// contract that is not a research digest.
func TestPerPageRetryCapForgetsAfterASuccessAndAfterTheBackoff(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var okNow atomic.Bool
	node, url := acceptingNode(t, "node-page", "verified digest", func(f *fakeNode) {
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			out := "an unrelated answer"
			if okNow.Load() {
				out = "verified digest"
			}
			w := remoteWire(out, `{"answer":"`+out+`"}`)
			w.NodeID = "node-page"
			return doneWire(t, w), http.StatusOK
		}
	})
	var localCalls atomic.Int64 // the verification retry runs on the local seat and fails there too
	run := func(c core.AgentContract) PlacedResult {
		results, _, err := Run(t.Context(), testCfg(t), failingLocal(&localCalls), []core.AgentContract{c}, "remote", []string{url})
		if err != nil {
			t.Fatal(err)
		}
		return results[0]
	}

	// A success forgets the page: fail twice, succeed, and two more failures do not trip the cap.
	page := uniquePage("the page that sometimes works")
	run(pageContract(page))
	run(pageContract(page))
	okNow.Store(true)
	if pr := run(pageContract(page)); len(pr.AcceptanceFailures) != 0 {
		t.Fatalf("fixture: the third issue should have passed, got %v", pr.AcceptanceFailures)
	}
	okNow.Store(false)
	before := node.dispatches.Load()
	run(pageContract(page))
	run(pageContract(page))
	if got := node.dispatches.Load() - before; got != 2 {
		t.Fatalf("%d of the next 2 issues reached the node after a success reset the count, want both", got)
	}

	// Time forgets it too: three failures back the page off, and once the backoff
	// has passed it is issued again.
	stale := uniquePage("the page that cooled off")
	for i := 0; i < pageMaxIssues; i++ {
		run(pageContract(stale))
	}
	if pr := run(pageContract(stale)); !strings.Contains(pr.Result.Reason, "page retry cap") {
		t.Fatalf("the page was not backed off after %d failures: %+v", pageMaxIssues, pr.Result)
	}
	realNow := pageRetries.now
	pageRetries.now = func() time.Time { return time.Now().Add(pageBackoff + time.Minute) }
	t.Cleanup(func() { pageRetries.now = realNow })
	before = node.dispatches.Load()
	run(pageContract(stale))
	if node.dispatches.Load() == before {
		t.Fatal("a page whose backoff has passed must be issued again")
	}
	pageRetries.now = realNow

	// A contract that is not a research digest is never capped, however often it fails.
	before = node.dispatches.Load()
	other := verifiedContract()
	other.Context = []core.ContextDoc{{Name: "01-example.txt", Text: uniquePage("not a research page")}}
	for i := 0; i < 2*pageMaxIssues; i++ {
		run(other)
	}
	if got := node.dispatches.Load() - before; got != int64(2*pageMaxIssues) {
		t.Fatalf("a non-research contract reached the node %d times of %d — only research digests are capped", got, 2*pageMaxIssues)
	}
}

func TestPageKeyIsTheContentNotTheName(t *testing.T) {
	a, b := pageContract("same page"), pageContract("same page")
	b.Context[0].Name = "07-other-host.txt" // source 7 of another call: same page, another name
	ka, oka := pageKeyFor(a)
	kb, okb := pageKeyFor(b)
	if !oka || !okb || ka != kb {
		t.Fatalf("keys %q/%v %q/%v, want the same page to have one key whatever its file name", ka, oka, kb, okb)
	}
	if kc, _ := pageKeyFor(pageContract("another page")); kc == ka {
		t.Fatal("different pages must not share a key")
	}
	plain := plainContract()
	plain.Context = a.Context
	if _, ok := pageKeyFor(plain); ok {
		t.Fatal("a contract from another door must not be keyed")
	}
	if _, ok := pageKeyFor(pageContract("")); !ok {
		// an empty page is still a page
		t.Fatal("a research contract with an empty page is still keyed")
	}
}
