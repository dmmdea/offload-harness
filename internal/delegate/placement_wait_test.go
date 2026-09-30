// placement_wait_test.go: INV-4 at the re-placement step - a node or a seat that is
// merely busy is a place in line, never a reason to refuse (ADR 0063, decision 2).
//
// The re-placement filters (room, backlog, cooldown, process gate, the local run-cap
// line) all run BEFORE a dispatch, so a node they hold out is never asked and never
// answers 503. When the refusal that started the chain was not a capacity refusal
// (a 500, a 404, a dropped connection) nothing else marked the subtask wait-worthy,
// and every candidate left was "busy" only in the delegator's own reading: the
// subtask failed as `placement refused` while a node that frees in a moment sat
// unasked.

package delegate

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// TestNonCapacityRefusalThenABusyNodeWaitsInsteadOfFailing: the first refusal is a
// 500, which excludes that node for good and is not a capacity signal; the only
// other place the subtask can go is busy for 300 ms. It must wait for it.
func TestNonCapacityRefusalThenABusyNodeWaitsInsteadOfFailing(t *testing.T) {
	t.Run("route=remote, the other node is full for 300 ms", func(t *testing.T) {
		compressPolls(t, 5*time.Millisecond, time.Second)
		compressWait(t, 20*time.Millisecond, 0)
		var bFree atomic.Bool
		_, aURL := refusingNode(t, "node-a", http.StatusInternalServerError, nil)
		b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
			f.maxQueueDepth = 1
			f.queueDepthFn = func() int {
				if bFree.Load() {
					return 0
				}
				return 1
			}
		})
		go func() {
			time.Sleep(300 * time.Millisecond)
			bFree.Store(true)
		}()
		cfg := testCfg(t)
		cfg.AgentPlacementWaitSec = 10
		results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Succeeded != 1 || results[0].Node != "node-b" || b.dispatches.Load() != 1 {
			t.Fatalf("summary = %+v node = %q err = %q b.dispatches = %d, want the subtask to wait for node-b and land there (INV-4: a full node is a place in line)", sum, results[0].Node, results[0].Err, b.dispatches.Load())
		}
	})

	t.Run("route=auto, the local run-cap line is full for 300 ms", func(t *testing.T) {
		compressPolls(t, 5*time.Millisecond, time.Second)
		compressWait(t, 20*time.Millisecond, 0)
		_, url := refusingNode(t, "node-broken", http.StatusInternalServerError, nil)
		cfg := testCfg(t)
		cfg.GPULockPath = busyLocal(t)
		cfg.FleetMaxConcurrentJobs = 1
		cfg.AgentPlacementWaitSec = 10
		seatRun := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: cfg.AgentPlannerModel(""), Kind: "contract", Goal: "occupies the seat", Phase: gpuactivity.PhaseRunning})
		if seatRun == nil {
			t.Fatal("fixture: could not register a run on the local seat")
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			seatRun.End()
		}()
		var localCalls atomic.Int64
		results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Succeeded != 1 || localCalls.Load() != 1 {
			t.Fatalf("summary = %+v err = %q localCalls = %d, want the subtask to wait for the local seat's slot and run there", sum, results[0].Err, localCalls.Load())
		}
	})

	// The other side of the rule: a node the CONTRACT cannot run on never frees up for
	// it, so holding a subtask for that node would only turn a failure the caller could
	// read at once into a wait for nothing.
	t.Run("an ineligible node is not worth waiting for", func(t *testing.T) {
		compressPolls(t, 5*time.Millisecond, time.Second)
		compressWait(t, 20*time.Millisecond, 0)
		_, aURL := refusingNode(t, "node-a", http.StatusInternalServerError, nil)
		tooSmall, bURL := acceptingNode(t, "node-tiny", "answer", func(f *fakeNode) { f.ctxTokens = 64 })
		cfg := testCfg(t)
		cfg.AgentPlacementWaitSec = 10
		began := time.Now()
		results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(began); elapsed > 2*time.Second {
			t.Fatalf("took %s: the subtask waited for a node whose context window can never hold the contract", elapsed)
		}
		if tooSmall.dispatches.Load() != 0 || sum.Failed != 1 || !strings.HasPrefix(results[0].Err, replacementExhaustedPrefix) {
			t.Fatalf("summary = %+v err = %q b.dispatches = %d, want an immediate placement refused", sum, results[0].Err, tooSmall.dispatches.Load())
		}
	})

	// With the wait switched off the outcome is the pre-wait one, but it must say what
	// was true: nothing was refused BY THE NODE THAT WAS PASSED OVER, it was full.
	t.Run("wait off names the node that was passed over", func(t *testing.T) {
		compressPolls(t, 5*time.Millisecond, time.Second)
		_, aURL := refusingNode(t, "node-a", http.StatusInternalServerError, nil)
		b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
			f.maxQueueDepth, f.queueDepth = 1, 1
		})
		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL})
		if err != nil {
			t.Fatal(err)
		}
		if b.dispatches.Load() != 0 || sum.Failed != 1 {
			t.Fatalf("summary = %+v b.dispatches = %d, want the subtask refused with node-b never asked (the wait is off)", sum, b.dispatches.Load())
		}
		if got := results[0].Err; !strings.Contains(got, "node-b") || !strings.Contains(got, "no room") {
			t.Fatalf("err = %q, want it to name node-b and say it had no room - the chain must not claim no remote was eligible", got)
		}
	})
}

// TestWaitNoteDoesNotLeakIntoALaterAttemptsDefer: why a subtask waited when nothing
// refused it (a gate turn-away, a deal's overflow) belongs to the attempt that was held.
// The verification retry is another placeAndRun over the same ledger; its defer must not
// quote the first attempt's hold.
func TestWaitNoteDoesNotLeakIntoALaterAttemptsDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	pl := newPlacements()
	pl.waitNote = "process gate: an earlier attempt of this subtask was held in line"
	pr := r.placeAndRun(t.Context(), 0, plainContract(), nil, time.Now(), 30, pl)
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v err = %q, want the capacity defer of a node that never had room (fixture)", pr.Result, pr.Err)
	}
	if strings.Contains(pr.Result.Reason, "earlier attempt") {
		t.Fatalf("reason = %q quotes the hold of an earlier attempt", pr.Result.Reason)
	}
}

// TestWaitNeverPlacesTheRetryOnTheSeatThatRanTheFirstAttempt: the first attempt ran on
// node A and failed verification. The local seat is fenced by a media render, so the retry
// goes to another remote, node B - and B refuses for capacity. The retry's premise is a
// DIFFERENT seat, but the capacity wait it is then held in took the best node with room:
// A, the seat whose answer it exists to correct. A node that REFUSED for capacity stays
// re-askable (that is what the wait is for); one that TOOK the first attempt does not.
func TestWaitNeverPlacesTheRetryOnTheSeatThatRanTheFirstAttempt(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var bReads atomic.Int64
	a, aURL := acceptingNode(t, "node-a", "an unrelated answer", func(f *fakeNode) { f.maxQueueDepth = 8 })
	b, bURL := refusingNode(t, "node-b", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "300" // out for the whole test once it has refused
		f.maxQueueDepth = 1
		// Full for the first read only: the run's first placement prefers node A.
		f.queueDepthFn = func() int {
			if bReads.Add(1) == 1 {
				return 1
			}
			return 0
		}
	})
	cfg := testCfg(t)
	cfg.GPULockPath = busyLocal(t) // a media render fences the local seat: a retry cannot go there
	cfg.AgentPlacementWaitSec = 1
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{verifiedContract()}, "remote", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.dispatches.Load() != 1 || b.dispatches.Load() != 1 {
		t.Fatalf("dispatches a=%d b=%d summary = %+v note = %q, want the retry asked of node B once and never placed on node A, the seat whose answer it corrects", a.dispatches.Load(), b.dispatches.Load(), sum, results[0].RetryNote)
	}
	if sum.FailedVerification != 1 || sum.Retried != 1 || sum.RetryRecovered != 0 {
		t.Fatalf("summary = %+v, want the first attempt's failed verification to stand (the retry found no other seat)", sum)
	}
}

// TestReplacementGateTurnAwayDoesNotSpendTheReplacementBound: the re-placement chose node B
// and another Run of this process took B's last gate slot before the dispatch. Nothing was
// sent, so the turn-away is no placement: the bound (maxRemoteReplacements) is not spent and
// the subtask lands on B when the slot frees. The branch was untestable until the seam
// existed (beforeForced is the one window in which another Run can take the slot).
func TestReplacementGateTurnAwayDoesNotSpendTheReplacementBound(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var bReads atomic.Int64
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = "300" })
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
		f.maxQueueDepth = 1
		f.queueDepthFn = func() int { // full for the first read only: the first placement prefers node A
			if bReads.Add(1) == 1 {
				return 1
			}
			return 0
		}
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	r := &runner{cfg: cfg, route: "remote", remotes: []string{aURL, bURL}, local: neverLocal(t)}
	var once sync.Once
	r.beforeForced = func(base string) {
		if base != bURL {
			return
		}
		once.Do(func() { holdGate(t, bURL, 1, 1, 150*time.Millisecond) }) // a sibling Run takes B's last slot
	}
	pl := newPlacements()
	pr := r.placeAndRun(t.Context(), 0, plainContract(), nil, time.Now(), 30, pl)
	if pr.Err != "" || pr.Result.Deferred || pr.Node != "node-b" {
		t.Fatalf("result = err %q deferred %v node %q, want the subtask on node-b once its slot freed", pr.Err, pr.Result.Deferred, pr.Node)
	}
	if a.dispatches.Load() != 1 || b.dispatches.Load() != 1 {
		t.Fatalf("dispatches a=%d b=%d, want 1 each (the turned-away dispatch never reached node B)", a.dispatches.Load(), b.dispatches.Load())
	}
	if pl.used != 0 {
		t.Fatalf("pl.used = %d, want 0: a dispatch the gate turned away spends no re-placement", pl.used)
	}
}

// TestCapacityDeferNamesANodeThatWasCoolingDown: a node that refused with a Retry-After
// longer than the whole wait is never asked again inside it, while its health advertises
// room on every tick. The defer said only "no node had room", which is not what happened.
func TestCapacityDeferNamesANodeThatWasCoolingDown(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "5"
		f.maxQueueDepth = 4 // its health advertises room the whole time
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity || node.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v class = %q dispatches = %d, want a capacity defer after ONE ask (the cooldown outlasts the wait)", sum, pr.Result.DeferClass, node.dispatches.Load())
	}
	for _, want := range []string{"node-a", "cooling down after its own refusal", "left"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to say %q: the node was not asked again although its health advertised room", pr.Result.Reason, want)
		}
	}
}
