package delegate

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A subtask the deal sent to a remote because the local seat read busy, and that
// the PROCESS GATE then turned away (this process already held the node's one
// admission slot), entered the capacity wait without the deal's reading: only an
// overflow subtask kept it, so the gated one took the busy local seat at the first
// tick. That was the CI-only flake in TestOverflowStaysOffABusySeatWhoseLoadBecomesUnreadable
// (also red on main, 2026-10-01). Made deterministic here: run A holds the node's
// only slot, the node's health lags (it reports no job running), so run B's deal
// sends B there too and the gate turns B away. B must wait for A, never take the
// busy local seat.
func TestAGateTurnAwayKeepsTheDealsBusyReading(t *testing.T) {
	for _, tc := range []struct {
		route    string
		inflight int
	}{{"spread", 2}, {"auto", 4}} {
		t.Run(tc.route, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			compressWait(t, 10*time.Millisecond, 0)
			var dispatches atomic.Int64
			var polled sync.Map
			_, url := acceptingNode(t, "node-a", "answer from node-a", func(f *fakeNode) {
				f.maxConcurrentJobs, f.maxQueueDepth = 1, 1
				// Lagging health: the node never reports the job it holds, so every deal
				// sees room and only this process's own gate knows the slot is taken.
				f.jobsRunningFn = func() int { return 0 }
				f.queueDepthFn = func() int { return 0 }
				f.onDispatch = func(string, core.AgentContract) { dispatches.Add(1) }
				inner := f.pollByJob
				f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
					cnt, _ := polled.LoadOrStore(jobID, new(atomic.Int64))
					if cnt.(*atomic.Int64).Add(1) < 40 {
						return map[string]any{"state": "running"}, http.StatusOK
					}
					return inner(jobID, n)
				}
			})
			cfg := testCfg(t)
			cfg.Endpoint = busySwap(t, "local-seat", tc.inflight)
			cfg.AgentPlacementWaitSec = 10
			var localCalls atomic.Int64
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			doneA := make(chan Summary, 1)
			go func() {
				_, sum, err := RunWith(ctx, cfg, passingLocal(&localCalls), []core.AgentContract{plainContract()}, tc.route, []string{url}, nil)
				if err != nil {
					t.Error(err)
				}
				doneA <- sum
			}()
			for dispatches.Load() == 0 {
				if ctx.Err() != nil {
					t.Fatal("run A never dispatched")
				}
				time.Sleep(2 * time.Millisecond)
			}
			results, sumB, err := RunWith(ctx, cfg, passingLocal(&localCalls), []core.AgentContract{plainContract()}, tc.route, []string{url}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sumA := <-doneA
			if localCalls.Load() != 0 {
				t.Fatalf("the local seat ran %d subtask(s) while it read busy (B's placement: %q)", localCalls.Load(), results[0].PlacementReason)
			}
			if sumA.Succeeded != 1 || sumB.Succeeded != 1 || dispatches.Load() != 2 {
				t.Fatalf("A=%+v B=%+v dispatches=%d: B must wait for A's slot and run on the node", sumA, sumB, dispatches.Load())
			}
		})
	}
}
