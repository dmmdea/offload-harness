package delegate

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// ADR 0061 end to end, delegator side. A node that HOLDS a silent request
// because its seat's engine is working for others (phase "queued") reports no
// token progress for minutes. While the engine keeps working the node
// re-publishes its allowance on every moving read — last_progress_ms = the
// read, allowance_ms = the hold's flat bound plus one poll — so the
// delegator's extension rolls forward with the evidence and never runs to the
// ceiling on the node's word alone. Three arms:
//
//   - refresh: the node keeps re-publishing; the delegator keeps polling past
//     its own poll budget and collects the result that comes at 3x that budget;
//   - no refresh (control): the same short allowance published once, at the
//     hold's start — the delegator gives the job up, because an allowance that
//     does not move is not evidence the engine does;
//   - dead node: the node re-publishes for a while and then goes quiet (its
//     process hung, its link died) — the delegator gives up one allowance plus
//     grace after the last refresh, far inside the run's ceiling, where the
//     first draft (the time left to the ceiling, published once) polled a dead
//     node to the ceiling.
func TestDelegatorFollowsARollingBusyHold(t *testing.T) {
	const allowanceMs = 200
	for _, tc := range []struct {
		name       string
		refreshFor time.Duration // how long the node keeps re-publishing (0 = never, <0 = always)
		finishAt   time.Duration // when the node reports done (0 = never)
		wantDone   bool
		minWall    time.Duration // the delegator must still be polling this long (0 = no bound)
		maxWall    time.Duration // and must have given up by this long (0 = no bound)
	}{
		{name: "refreshed allowance is followed to the result", refreshFor: -1, finishAt: 1500 * time.Millisecond, wantDone: true},
		{name: "control: an allowance published once is not followed", refreshFor: 0, finishAt: 1500 * time.Millisecond, wantDone: false},
		// The last refresh lands at ~400 ms: the delegator must follow it to
		// ~400+200+100 ms and give up long before the ceiling (300 x 10 ms +
		// grace = 3.1 s after dispatch).
		{name: "dead node is given up well before the ceiling", refreshFor: 400 * time.Millisecond, wantDone: false,
			minWall: 600 * time.Millisecond, maxWall: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, 100*time.Millisecond)
			old := pollSecond
			pollSecond = 10 * time.Millisecond // a 30 s contract polls for 300 ms + the grace
			t.Cleanup(func() { pollSecond = old })
			start := time.Now()
			var lastRefresh atomic.Int64
			lastRefresh.Store(start.UnixMilli())
			node := &fakeNode{
				t: t, token: "sekrit", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
				pollState: func(n int64) (map[string]any, int) {
					now := time.Now()
					if tc.finishAt > 0 && now.After(start.Add(tc.finishAt)) {
						return doneWire(t, remoteWire("the held answer", `{"answer":"42"}`)), 200
					}
					if tc.refreshFor < 0 || now.Before(start.Add(tc.refreshFor)) {
						lastRefresh.Store(now.UnixMilli()) // a moving engine read re-publishes the allowance
					}
					return map[string]any{"state": "running", "progress": map[string]any{
						"phase": "queued", "last_progress_ms": lastRefresh.Load(), "allowance_ms": allowanceMs, "ceiling_sec": 300}}, 200
				},
			}
			srv := node.server()
			defer srv.Close()
			cfg := testCfg(t)
			cfg.FleetAuthToken = "sekrit"
			contract := remoteContract()
			contract.Acceptance = []string{"contains:held answer", "nonempty:answer"}
			res, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
			wall := time.Since(start)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := sum.Succeeded == 1 && len(res) == 1 && !res[0].Result.Deferred
			if got != tc.wantDone {
				reason := ""
				if len(res) == 1 {
					reason = res[0].Result.Reason
				}
				t.Fatalf("succeeded=%d deferred=%v reason=%q after %s, want done=%v", sum.Succeeded, len(res) == 1 && res[0].Result.Deferred, reason, wall, tc.wantDone)
			}
			if tc.minWall > 0 && wall < tc.minWall {
				t.Fatalf("gave the job up after %s: the last refresh still covered it to at least %s", wall, tc.minWall)
			}
			if tc.maxWall > 0 && wall > tc.maxWall {
				t.Fatalf("polled a node that stopped refreshing for %s (bound %s): the extension must end one allowance + grace after the last refresh, not at the ceiling", wall, tc.maxWall)
			}
		})
	}
}
