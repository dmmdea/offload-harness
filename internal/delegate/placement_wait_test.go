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
