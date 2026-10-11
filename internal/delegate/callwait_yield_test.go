package delegate

// A capacity wait that holds a run slot while subtasks have not started gives the slot up at its
// TTL and keeps waiting (ADR 0073, amendment 2026-10-10, register C16).
//
// Before the amendment such a wait ENDED at the TTL as a capacity defer, so a saturated fleet turned
// a fan-out into a row of "no node had room within 2m0s" for subtasks that a node freeing a minute
// later would have taken, inside a call with 25 minutes. Only the last subtask to start waited to the
// call's horizon. The operator's rule is to queue and never refuse: a subtask in line for a node is
// making progress. The slot a wait holds is what kept the others out of the call, so the wait gives
// that up at its TTL, and takes a place again only to dispatch, so the number of attempts a call has
// open at once is still its width.

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestEverySubtaskOfASaturatedCallLandsWhenTheNodeFreesAfterTheTTL: five subtasks, four run slots, one
// node that is full for 1.4 s and then takes work, a TTL of 0.6 s and a call of 3.5 s. The four that
// hold slots used to end at 0.6 s as capacity defers; they now give their slots up, the fifth starts,
// and all five land on the node once it frees, none of them deferred.
func TestEverySubtaskOfASaturatedCallLandsWhenTheNodeFreesAfterTheTTL(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, heldPoll, 0)
	withCallReserve(t, heldReserve)
	withBuiltInWait(t, heldTTL)
	_, url := acceptingNode(t, "node-late", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = fullUntil(1400 * time.Millisecond)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0
	contracts := make([]core.AgentContract, runConcurrency+1)
	for i := range contracts {
		contracts[i] = remoteContract()
	}

	// The call's own events say when each subtask began, on the production code's clock.
	var mu sync.Mutex
	began := map[int]time.Time{}
	opts := deadlineIn(heldWindow)
	opened := opts.Deadline.Add(-heldWindow)
	opts.OnProgress = func(ev ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Kind == "started" {
			began[ev.Index] = time.Now()
		}
	}

	results, sum, elapsed := runWithin(t, 30*time.Second, cfg, neverLocal(t), contracts, "remote", []string{url}, opts, nil)

	if sum.Succeeded != len(contracts) || sum.Deferred != 0 || sum.Failed != 0 {
		for i, pr := range results {
			t.Logf("result %d: err=%q deferred=%v reason=%q", i, pr.Err, pr.Result.Deferred, pr.Result.Reason)
		}
		t.Fatalf("summary %+v after %s, want all %d subtasks landed: the node freed at 1.4 s, inside a %s call, and a subtask in line for it must not end at its %s TTL",
			sum, elapsed.Round(time.Millisecond), len(contracts), heldWindow, heldTTL)
	}
	// The fifth subtask began when the first four gave their slots up at the TTL, while the node was
	// still full (it frees at 1.4 s). A wait that kept its slot until it landed would start the fifth
	// only after the node freed, and the slot would again be what held a sibling out of the call.
	mu.Lock()
	b5, ok := began[runConcurrency]
	mu.Unlock()
	if !ok || b5.Sub(opened) > 1100*time.Millisecond {
		t.Errorf("the fifth subtask began %v into the call (began=%v), want it started by the slots the first four gave up at their %s TTL, well before the node freed at 1.4 s", b5.Sub(opened).Round(time.Millisecond), ok, heldTTL)
	}
	for i, pr := range results {
		if pr.Result.Deferred || pr.Err != "" {
			t.Errorf("result %d did not land: err=%q reason=%q", i, pr.Err, pr.Result.Reason)
		}
		if pr.CapacityWaitSec < (heldTTL).Seconds() && i < runConcurrency {
			t.Errorf("result %d waited %.2f s for capacity, want at least its %s TTL (the node freed at 1.4 s)", i, pr.CapacityWaitSec, heldTTL)
		}
	}
}

// TestASubtaskThatGaveItsSlotUpDoesNotDispatchWithoutOne: eight subtasks, four run slots, a node that
// is full for 1.4 s and then takes work whose jobs run 300 ms. Every subtask lands, and the node never
// holds more than four of the call's jobs at once: a wait that gave its slot up takes a place again
// before it dispatches, so the width of the call still bounds its open attempts (the reason ADR 0073
// rejected releasing the slot outright, and the one pin this design stands on).
func TestASubtaskThatGaveItsSlotUpDoesNotDispatchWithoutOne(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 100*time.Millisecond, 0)
	withCallReserve(t, heldReserve)
	withBuiltInWait(t, heldTTL)

	var (
		mu       sync.Mutex
		started  = map[string]time.Time{}
		inFlight = map[string]bool{}
		peak     int
	)
	const jobRuns = 300 * time.Millisecond
	node, url := acceptingNode(t, "node-late", "zorblax after the wait", func(f *fakeNode) {
		// The node publishes its workers and no queue ceiling, so the process gate (which holds a node
		// to its queue ceiling) bounds nothing here and the call's width is the only bound on the
		// attempts it has open: four, from the deal.
		f.maxConcurrentJobs = runConcurrency
		f.dispatchHook = fullUntil(1400 * time.Millisecond)
		f.onDispatch = func(jobID string, _ core.AgentContract) {
			mu.Lock()
			defer mu.Unlock()
			if _, seen := started[jobID]; !seen {
				started[jobID] = time.Now()
				inFlight[jobID] = true
				if len(inFlight) > peak {
					peak = len(inFlight)
				}
			}
		}
		done := f.pollByJob
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			mu.Lock()
			at, ok := started[jobID]
			mu.Unlock()
			if ok && time.Since(at) < jobRuns {
				return map[string]any{"state": "running"}, http.StatusOK
			}
			body, status := done(jobID, n)
			mu.Lock()
			delete(inFlight, jobID)
			mu.Unlock()
			return body, status
		}
	})
	_ = node
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0
	contracts := make([]core.AgentContract, 8)
	for i := range contracts {
		contracts[i] = remoteContract()
	}

	_, sum, elapsed := runWithin(t, 30*time.Second, cfg, neverLocal(t), contracts, "remote", []string{url}, deadlineIn(8*time.Second), nil)

	if sum.Succeeded != len(contracts) || sum.Deferred != 0 {
		t.Fatalf("summary %+v after %s, want all %d subtasks landed", sum, elapsed.Round(time.Millisecond), len(contracts))
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > runConcurrency {
		t.Fatalf("the node held %d of the call's jobs at once, want at most the call's width of %d: a wait that gave its slot up dispatched without taking one", peak, runConcurrency)
	}
	if peak < runConcurrency {
		t.Logf("peak in flight %d (width %d): the jobs overlapped less than the width allowed, so this run proved the bound less tightly", peak, runConcurrency)
	}
}

// TestAHolderThatGaveItsSlotUpHoldsNoPlaceWhileItWaits drives one wait directly, so the place can be
// read while the wait runs. A subtask whose call has a sibling that has not started holds its place for
// its TTL (the rule that keeps a short wait cheap is unchanged), gives it up at the TTL, holds none
// while it sleeps between refused attempts (a refused attempt does not keep it: a sibling that could be
// placed elsewhere must be able to take it), and holds one again when it lands, because the attempt it
// dispatched is open and counts against the call's width.
func TestAHolderThatGaveItsSlotUpHoldsNoPlaceWhileItWaits(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, heldPoll, 0)
	withCallReserve(t, heldReserve)
	withBuiltInWait(t, heldTTL)
	node, url := acceptingNode(t, "node-late", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = fullUntil(1400 * time.Millisecond)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	// A call of two subtasks of which one has begun (this one): the other has not started, so this wait
	// holds a place somebody needs.
	r.call = newCallDeadline(t.Context(), &RunOptions{Deadline: time.Now().Add(heldWindow)}, 2)
	r.call.begin()
	sem := make(chan struct{}, runConcurrency)
	sem <- struct{}{} // the launcher's take
	slot := newRunSlot(sem)
	ctx := withRunSlot(t.Context(), slot)

	type outcome struct{ pr PlacedResult }
	done := make(chan outcome, 1)
	begin := time.Now()
	go func() {
		seed := PlacedResult{waitCapacity: true, pendingReason: "test: nothing had room"}
		done <- outcome{r.awaitCapacity(ctx, 0, plainContract(), time.Now(), 30, newPlacements(), seed, nil, "")}
	}()

	var early, waiting, heldWhileWaiting int
	for len(done) == 0 {
		el := time.Since(begin)
		switch {
		case el < heldTTL/2:
			early++
			if !slot.holds() {
				t.Errorf("at %s, inside the %s TTL, the wait had already given its place up", el.Round(time.Millisecond), heldTTL)
			}
		case el > heldTTL+150*time.Millisecond && el < 1300*time.Millisecond:
			waiting++
			if slot.holds() {
				heldWhileWaiting++
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := <-done
	pr := out.pr
	if pr.Err != "" || pr.Result.Deferred || pr.Result.Output == "" {
		t.Fatalf("result err=%q deferred=%v reason=%q, want it landed on the node once it freed at 1.4 s", pr.Err, pr.Result.Deferred, pr.Result.Reason)
	}
	if early == 0 || waiting == 0 {
		t.Fatalf("sampled %d times before the TTL and %d times while it waited: the probe did not run", early, waiting)
	}
	// An attempt (a refused dispatch) takes a place for the few milliseconds it lasts, once a tick.
	if heldWhileWaiting*3 > waiting {
		t.Errorf("the wait held its place in %d of %d samples between the TTL and the node freeing: it keeps a place it is not using", heldWhileWaiting, waiting)
	}
	if !slot.holds() {
		t.Error("the wait landed a job without holding a place: the open attempt would not count against the call's width")
	}
	if node.dispatches.Load() < 2 {
		t.Errorf("the node saw %d dispatches, want the refusals and the landing", node.dispatches.Load())
	}
}

// TestTheLastSubtaskToStartKeepsItsPlaceWhileItWaits: the wait began with a sibling that had not started,
// and the sibling started before the TTL. At the TTL nobody is behind it, so it has no one to give its
// place to and keeps it to the call's horizon as the last subtask always did. Giving it up would only add
// an acquire to every dispatch.
func TestTheLastSubtaskToStartKeepsItsPlaceWhileItWaits(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, heldPoll, 0)
	withCallReserve(t, heldReserve)
	withBuiltInWait(t, heldTTL)
	_, url := acceptingNode(t, "node-late", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = fullUntil(1400 * time.Millisecond)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	r.call = newCallDeadline(t.Context(), &RunOptions{Deadline: time.Now().Add(heldWindow)}, 2)
	r.call.begin()                          // this subtask; its sibling has not started, so the wait is capped at the TTL
	time.AfterFunc(heldTTL/3, r.call.begin) // the sibling starts well inside the TTL
	sem := make(chan struct{}, runConcurrency)
	sem <- struct{}{}
	slot := newRunSlot(sem)
	ctx := withRunSlot(t.Context(), slot)

	done := make(chan PlacedResult, 1)
	go func() {
		seed := PlacedResult{waitCapacity: true, pendingReason: "test: nothing had room"}
		done <- r.awaitCapacity(ctx, 0, plainContract(), time.Now(), 30, newPlacements(), seed, nil, "")
	}()
	samples, lost := 0, 0
	for len(done) == 0 {
		samples++
		if !slot.holds() {
			lost++
		}
		time.Sleep(5 * time.Millisecond)
	}
	pr := <-done
	if pr.Err != "" || pr.Result.Deferred {
		t.Fatalf("result err=%q reason=%q, want it landed once the node freed", pr.Err, pr.Result.Reason)
	}
	if lost > 0 {
		t.Errorf("the last subtask's wait gave its place up in %d of %d samples: nobody was behind it", lost, samples)
	}
}
