package delegate

// A job queued on a node whose workers are all busy stays in line at its queue deadline when nothing
// better is on offer (ADR 0063, amendment 2026-10-10, register C16).
//
// The queue budget is derived from the node's ETA (1.5 x the wait a new job faces + 30 s, floored at 60 s),
// and the ETA of a node is the median of its recent walls: a one-worker node mid-way through a job that runs
// three times its median publishes a wait that is a fraction of the one the job faces. At the budget the
// delegator withdrew the job (ADR 0064), the withdrawal was filed as a capacity refusal, and the subtask
// went back to the capacity wait, which asked the same node again and put the job at the BACK of the same
// line: the wait already spent was thrown away to land where it started. The budget is where the delegator
// asks whether there is a better place; if the node is plainly busy (its workers all running) and no other
// node, nor the local seat, has room, the job stays where it is and is asked about again a floor later, up
// to the patience the contract was given.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// busyQueueNode is a one-worker node that is idle when the job is placed and busy with somebody else's job
// from then on, and that starts the queued job `startsAfter` after the test began (never when 0).
func busyQueueNode(t *testing.T, startsAfter time.Duration, tune func(*fakeNode)) *fakeNode {
	t.Helper()
	begin := time.Now()
	zero := 0.0
	var f *fakeNode
	busy := func() bool { return f.dispatches.Load() > 0 }
	f = &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		maxConcurrentJobs: 1,
		queueWaitEstimate: &zero, // the node's own ETA says a new job starts at once
		jobsRunningFn: func() int {
			if busy() {
				return 1
			}
			return 0
		},
		queueDepthFn: func() int {
			if busy() {
				return 2 // the other tenant's running job and ours
			}
			return 0
		},
		pollState: func(int64) (map[string]any, int) {
			if startsAfter > 0 && time.Since(begin) >= startsAfter {
				return doneWire(t, remoteWire("the answer", `{"answer":"the answer"}`)), http.StatusOK
			}
			return map[string]any{"state": "accepted"}, http.StatusOK
		},
	}
	if tune != nil {
		tune(f)
	}
	return f
}

func runQueuedOn(t *testing.T, f *fakeNode) ([]PlacedResult, Summary, time.Duration) {
	t.Helper()
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // the 60 "s" floor is 300 ms; the 600 s contract's patience is 3 s
	srv := f.server()
	contract := plainContract()
	contract.TimeoutSec = 600
	began := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return results, sum, time.Since(began)
}

// TestAQueuedJobStaysInLineWhileTheNodeIsBusyAndNothingElseHasRoom: the node says a new job starts at
// once, is in fact running another job, and starts ours after 1 s - three times the 300 ms the budget
// gave it. Before the amendment the job was withdrawn at 300 ms and the subtask failed (or went back to the
// same node's line); now it is still in line when its turn comes.
func TestAQueuedJobStaysInLineWhileTheNodeIsBusyAndNothingElseHasRoom(t *testing.T) {
	results, sum, elapsed := runQueuedOn(t, busyQueueNode(t, time.Second, nil))
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q after %s, want the job waited for: the node's worker was busy and it started the job at 1 s", sum, results[0].Err, elapsed.Round(time.Millisecond))
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("finished after %s, before the node started the job", elapsed)
	}
}

// TestAQueuedJobIsGivenUpOnAtTheLimitOfTheContractsPatience: a node that stays busy and never starts the job
// is waited on to the patience the contract was given (its 600 s poll budget, 3 s here) and no further, and
// the deferral says it kept waiting and why.
func TestAQueuedJobIsGivenUpOnAtTheLimitOfTheContractsPatience(t *testing.T) {
	results, sum, elapsed := runQueuedOn(t, busyQueueNode(t, 0, nil))
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue-deadline failure at the end of the patience", sum, results[0].Err)
	}
	if elapsed < 2500*time.Millisecond {
		t.Fatalf("gave up after %s, want the whole 3 s patience: the node was busy and nothing else had room", elapsed.Round(time.Millisecond))
	}
	if elapsed > 6*time.Second {
		t.Fatalf("waited %s, past the contract's patience", elapsed.Round(time.Millisecond))
	}
	// The step does not overshoot the patience: the deadline it names is exactly the contract's poll budget
	// (3 s of wall plus the grace here), which is not a multiple of the 300 ms step.
	patience := patienceFor(func() core.AgentContract { c := plainContract(); c.TimeoutSec = 600; return c }(), NodeView{})
	for _, want := range []string{"kept in line", "workers were all busy", "queue deadline after " + patience.String() + ":"} {
		if !strings.Contains(results[0].Err, want) {
			t.Errorf("err = %q, want it to say %q", results[0].Err, want)
		}
	}
}

// TestAQueuedJobIsGivenUpOnWhenTheNodesHealthCannotBeRead: at the budget the delegator reads the node again to
// see whether its workers are busy. A node that no longer answers health gives it no reason to wait ("unknown is
// never a yes"), so the job is withdrawn at the budget as it always was.
func TestAQueuedJobIsGivenUpOnWhenTheNodesHealthCannotBeRead(t *testing.T) {
	f := busyQueueNode(t, 0, func(f *fakeNode) {
		// The first read is the placement's; every later one fails, so the refresh and the question at the budget
		// both find the node mute while its job answers `accepted`.
		f.healthFailFn = func(n int64) bool { return n > 1 }
	})
	results, sum, elapsed := runQueuedOn(t, f)
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue-deadline failure", sum, results[0].Err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("gave up after %s, want the 300 ms budget: the node's health could not be read", elapsed.Round(time.Millisecond))
	}
	if strings.Contains(results[0].Err, "kept in line") {
		t.Errorf("err = %q says the job was kept in line on a node nobody could read", results[0].Err)
	}
}

// TestAQueuedJobIsStillGivenUpOnWhenTheNodeHasAFreeWorker: the node reports a free worker and still has not
// started the job. That is not a queue, it is a node that is not working, and the job is withdrawn at the
// budget as it always was.
func TestAQueuedJobIsStillGivenUpOnWhenTheNodeHasAFreeWorker(t *testing.T) {
	f := busyQueueNode(t, 0, func(f *fakeNode) {
		f.jobsRunningFn = func() int { return 0 }
		f.queueDepthFn = func() int { return 1 }
	})
	results, sum, elapsed := runQueuedOn(t, f)
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue-deadline failure", sum, results[0].Err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("gave up after %s, want the 300 ms budget: the node had a free worker and still did not start the job", elapsed.Round(time.Millisecond))
	}
	if strings.Contains(results[0].Err, "kept in line") {
		t.Errorf("err = %q says the job was kept in line", results[0].Err)
	}
}

// TestAQueuedJobLeavesForAPlaceThatHasRoom: the busy node, and a second node that was busy when the job was
// placed and has room now. The job is withdrawn at its budget exactly as before, so the re-placement takes it
// there: a node being busy is not a reason to stay when a better place is on offer.
func TestAQueuedJobLeavesForAPlaceThatHasRoom(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond)
	compressWait(t, 20*time.Millisecond, 0)
	stuck := busyQueueNode(t, 0, nil) // idle until the job is dispatched, busy from then on, never starts it
	probe := &withdrawProbe{answer: confirmsWithdrawal}
	stuckSrv := probe.front(t, stuck.server())
	// The other node is busy until the first node has been dispatched to (so the deal places the job on the first),
	// and has room after.
	idle, idleURL := acceptingNode(t, "node-idle", "zorblax from the other node", func(f *fakeNode) {
		f.maxConcurrentJobs = 1
		f.jobsRunningFn = func() int {
			if stuck.dispatches.Load() == 0 {
				return 1
			}
			return 0
		}
		f.queueDepthFn = func() int {
			if stuck.dispatches.Load() == 0 {
				return 1
			}
			return 0
		}
	})
	contract := plainContract()
	contract.TimeoutSec = 600
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	began := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{stuckSrv.URL, idleURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(began)
	if sum.Succeeded != 1 || results[0].Err != "" {
		t.Fatalf("summary = %+v err = %q, want the job to have moved to the node with room", sum, results[0].Err)
	}
	if got := probe.deletes.Load(); got != 1 {
		t.Fatalf("the busy node was asked to take the job back %d time(s), want 1: a better place was on offer", got)
	}
	if idle.dispatches.Load() != 1 {
		t.Fatalf("the other node saw %d dispatch(es), want the re-placed job", idle.dispatches.Load())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s: the job should have left at its 300 ms budget, not stayed in line to the patience", elapsed.Round(time.Millisecond))
	}
}

// TestBetterPlaceThanNamesOnlyAPlaceWithRoom: the question the queue deadline asks, on its own. Another node with
// room is a better place; no other node is none; a node that is itself busy is none either.
func TestBetterPlaceThanNamesOnlyAPlaceWithRoom(t *testing.T) {
	stuck := busyQueueNode(t, 0, nil)
	stuck.dispatches.Store(1) // busy from the start
	stuckSrv := stuck.server()
	_, roomyURL := acceptingNode(t, "node-roomy", "zorblax", func(f *fakeNode) { f.maxConcurrentJobs = 1 })
	_, fullURL := acceptingNode(t, "node-full", "zorblax", func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning, f.queueDepth = 1, 1, 1
		f.saturation = &struct {
			Score    float64 `json:"score"`
			High     bool    `json:"high"`
			IdleSlot bool    `json:"idle_slot"`
		}{Score: 1, High: true}
	})
	contract := plainContract()
	cfg := testCfg(t)
	cases := []struct {
		name    string
		remotes []string
		want    string
	}{
		{"another node has room", []string{stuckSrv.URL, roomyURL}, "node-roomy"},
		{"no other node", []string{stuckSrv.URL}, ""},
		{"the only other node is saturated", []string{stuckSrv.URL, fullURL}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{cfg: cfg, route: "remote", remotes: tc.remotes, local: neverLocal(t)}
			why, better := r.betterPlaceThan(t.Context(), stuckSrv.URL, contract)
			if better != (tc.want != "") || !strings.Contains(why, tc.want) {
				t.Fatalf("betterPlaceThan = %q, %v, want a place naming %q", why, better, tc.want)
			}
		})
	}
}
