// auto_local_busy_test.go: route=auto's busy decision (W-01, PR-5 item 2,
// register S-01). Before this, route=auto's busy read was `leaseInfo.Held`
// ALONE (run.go, attempt()'s default case) — an idle local box with no lease
// held always won Place, so a local seat already carrying more in-flight
// requests than the fleet's own concurrency cap never looked at the fleet at
// all. The fix reuses the ONE-PROBE-PER-RUN pattern route=spread already
// established (spreadLocalBusy): probeLocalBusy is read once and cached on
// the runner, and busy is true when the lease is held, OR the cached
// in-flight count has reached FleetConcurrencyLimit(), OR a load is in
// progress.

package delegate

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRunAutoBusyReadsTheLocalSeatsInFlightCountNotJustTheLease(t *testing.T) {
	remote, url := eligibleNode(t, "node-a", "remote answered")
	cfg := testCfg(t)

	// {busy:true, inflight:4}: FleetConcurrencyLimit() defaults to 4 (config
	// 0 = built-in default), so 4 in flight has reached the cap — busy, and
	// the fleet must be consulted. One eligible remote → it is chosen.
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{url},
		intent:         openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{busy: true, inflight: 4} },
	}
	pr := r.runOne(context.Background(), 0, remoteContract())
	if localCalls.Load() != 0 {
		t.Fatalf("local runner called %d times, want 0 — inflight 4 reached the concurrency cap, so the fleet must be used", localCalls.Load())
	}
	if remote.dispatches.Load() != 1 {
		t.Fatalf("remote dispatches = %d, want 1", remote.dispatches.Load())
	}
	if pr.Node != "node-a" {
		t.Fatalf("Node = %q, want node-a", pr.Node)
	}

	// Control: {busy:true, inflight:1} — busy() as the OLD rule would have
	// read it (any positive count), but 1 is BELOW the concurrency cap, so
	// the new rule must still prefer the idle local seat.
	remote2, url2 := eligibleNode(t, "node-b", "must not be used")
	var localCalls2 atomic.Int64
	r2 := &runner{
		cfg: cfg, local: passingLocal(&localCalls2), route: "auto", remotes: []string{url2},
		intent:         openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{busy: true, inflight: 1} },
	}
	r2.runOne(context.Background(), 0, remoteContract())
	if localCalls2.Load() != 1 {
		t.Fatalf("local runner called %d times, want 1 — inflight 1 is below the concurrency cap", localCalls2.Load())
	}
	if remote2.dispatches.Load() != 0 {
		t.Fatalf("remote dispatches = %d, want 0 — the local seat had room", remote2.dispatches.Load())
	}
}

// TestRunAutoBusyProbedOnceProbedNotPerSubtask pins the "once per Run, cached
// on the runner" half of W-01: two subtasks placed through the SAME runner
// must not each pay their own local-busy probe.
func TestRunAutoBusyProbedOncePerRunNotPerSubtask(t *testing.T) {
	// The output must PASS acceptance ("contains:qube") so neither subtask
	// triggers the verification retry — retrySeatBusy makes its OWN fresh
	// probe by design (D-46: the retry seat's load must be read live, not
	// from a snapshot a sibling subtask may have since invalidated), and that
	// probe is deliberately uncached. Counting it here would test the wrong
	// thing.
	_, url := eligibleNode(t, "node-a", "qube answered")
	cfg := testCfg(t)
	var probes atomic.Int64
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{url},
		intent: openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading {
			probes.Add(1)
			return busyReading{busy: true, inflight: 4}
		},
	}
	r.runOne(context.Background(), 0, remoteContract())
	r.runOne(context.Background(), 1, remoteContract())
	if n := probes.Load(); n != 1 {
		t.Fatalf("local-busy probe fired %d times across two subtasks on one runner, want exactly 1 (cached)", n)
	}
}
