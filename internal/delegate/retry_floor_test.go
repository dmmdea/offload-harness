package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The seat-aware retry floor (0.115.9, register D-46). Measured motivation
// (2026-09-10): a 603 s empty first attempt of a 900 s contract left the 27B
// a 296 s retry that generated 4,178 tokens of think and timed out; over 318
// retries the pass rate was 13 %, 0/9 that day; and the retry landed on a seat
// that was mid-generation for another job.

// emptyFinalLocal is a local seat whose run ended on an empty final: the
// 0.115.8 node shape — deferred, class abstention (retry-eligible by class),
// stop_reason "empty".
func emptyFinalLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Deferred: true, DeferClass: core.DeferClassAbstention, StopReason: "empty", StopNote: "finish stop, empty message",
			Reason: "empty final answer after 2 steps and 40 completion tokens: finish stop, empty message"}, nil
	}
}

// TestRunRetrySkippedWhenTheFirstAttemptEndedOnAnEmptyFinal: an empty final
// is not a wrong answer another seat corrects; no dispatch, a named note.
func TestRunRetrySkippedWhenTheFirstAttemptEndedOnAnEmptyFinal(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), emptyFinalLocal(&localCalls), contracts(1), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("an empty final must not be retried: dispatches=%d retried=%d retried_on=%q", node.dispatches.Load(), sum.Retried, pr.RetriedOn)
	}
	if !strings.Contains(pr.RetryNote, "empty final") || !strings.Contains(pr.RetryNote, "stop_reason empty") {
		t.Fatalf("retry_note = %q, want it to name the empty-final shape", pr.RetryNote)
	}
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassAbstention {
		t.Fatalf("the first attempt must be published as it was: %+v", pr.Result)
	}
}

// TestRunRetryFloorComesFromConfig: agent_retry_min_sec raises the 10 s floor;
// a contract whose whole budget is under it never retries, and the note says
// which knob set the floor.
func TestRunRetryFloorComesFromConfig(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	cfg := testCfg(t)
	cfg.AgentRetryMinSec = 300
	var localCalls atomic.Int64
	c := contracts(1)
	c[0].TimeoutSec = 120 // the whole budget is under the 300 s floor
	results, sum, err := Run(context.Background(), cfg, failingLocal(&localCalls), c, "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("retry must be skipped under the configured floor: dispatches=%d retried=%d", node.dispatches.Load(), sum.Retried)
	}
	if !strings.Contains(pr.RetryNote, "floor 300s, agent_retry_min_sec") {
		t.Fatalf("retry_note = %q, want the configured floor named", pr.RetryNote)
	}
	// Below the historical floor the knob is inert: 0 and small values keep 10 s.
	for _, v := range []int{0, 5, -1} {
		r := &runner{cfg: cfg}
		r.cfg.AgentRetryMinSec = v
		if got := r.retryFloorSec(); got != minRetrySec {
			t.Fatalf("agent_retry_min_sec %d: floor = %d, want %d", v, got, minRetrySec)
		}
	}
}

// TestRunRetryNeverLandsOnASeatAlreadyRunningAnotherJob: the retry node
// publishes jobs_running 1 — its seat is generating for someone else — so the
// retry is skipped with a note instead of halving both runs' tok/s.
func TestRunRetryNeverLandsOnASeatAlreadyRunningAnotherJob(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	node.jobsRunning = 1
	node.maxConcurrentJobs = 4 // room by the capacity rule — the SEAT is what is busy
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&localCalls), contracts(1), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("retry must not land on a running seat: dispatches=%d retried=%d retried_on=%q", node.dispatches.Load(), sum.Retried, pr.RetriedOn)
	}
	if !strings.Contains(pr.RetryNote, "already running another job") || !strings.Contains(pr.RetryNote, "jobs_running 1") {
		t.Fatalf("retry_note = %q, want the busy seat named", pr.RetryNote)
	}
	// LIVENESS (review finding, 0.115.9): route=spread probes the fleet ONCE
	// before the batch; the busy check must read the node NOW, not that
	// snapshot. The node is idle at the initial probe and turns busy while the
	// local first attempt runs — a cached read would let the retry land.
	live, liveURL := eligibleNode(t, "node-live", "qube from live")
	var busyNow atomic.Int64
	live.jobsRunningFn = func() int { return int(busyNow.Load()) }
	flipThenFail := func(ctx context.Context, c core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		busyNow.Store(1) // a sibling job landed on node-live after the fleet probe
		return failingLocal(&localCalls)(ctx, c, opts)
	}
	results, sum, err = Run(context.Background(), testCfg(t), flipThenFail, contracts(1), "spread", []string{liveURL})
	if err != nil {
		t.Fatal(err)
	}
	if live.dispatches.Load() != 0 || sum.Retried != 0 || !strings.Contains(results[0].RetryNote, "fresh health") {
		t.Fatalf("a node that turned busy AFTER the fleet probe must still block the retry: dispatches=%d retried=%d note=%q", live.dispatches.Load(), sum.Retried, results[0].RetryNote)
	}
	// And the local landing reads the seat's in-flight count: a busy reading
	// skips, an idle one lets the retry run.
	r := &runner{cfg: testCfg(t), localBusyProbe: func(context.Context) busyReading { return busyReading{busy: true, inflight: 2} }}
	if busy, why := r.retrySeatBusy(context.Background(), placement{}); !busy || !strings.Contains(why, "2 in flight") {
		t.Fatalf("local busy = %v (%q), want busy with the in-flight count", busy, why)
	}
	r.localBusyProbe = func(context.Context) busyReading { return busyReading{} }
	if busy, _ := r.retrySeatBusy(context.Background(), placement{}); busy {
		t.Fatal("an idle local seat must not block the retry")
	}
	_ = json.Marshal // keep the import honest if helpers change
}

// TestRunRetryFloorComesFromTheRetrySeat (0.117.2, register D-46 follow-up):
// the 2026-09-10 retry cleared a floor sized from the FIRST attempt's seat (the
// 4B's 201 s) and landed on the 27B, whose own floor was ≈ 484 s — with a
// schema contract's re-pack on top, ≈ 757 s. The floor is now the RETRY
// seat's: its published rate and cold load at its own final budget, plus the
// re-pack term for a schema contract. A slow retry seat skips the retry with a
// note naming the publisher; a fast one lets it run.
func TestRunRetryFloorComesFromTheRetrySeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	slow, slowURL := eligibleNode(t, "node-slow", "qube from slow")
	slow.seatRate = map[string]any{"tok_s": 30.0, "cold_load_sec": 210.0, "samples": 5, "min_turn_sec": 484}
	slow.seatBudget = map[string]any{"step_tokens": 4096, "final_tokens": 8192, "thinking": "off"}
	var localCalls atomic.Int64
	c := contracts(1)
	c[0].TimeoutSec = 600 // clears the configured 10 s floor and the first attempt's (none); not the 27B's 757 s
	results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&localCalls), c, "spread", []string{slowURL})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if slow.dispatches.Load() != 0 || sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("retry must be skipped under the RETRY seat's floor: dispatches=%d retried=%d retried_on=%q note=%q", slow.dispatches.Load(), sum.Retried, pr.RetriedOn, pr.RetryNote)
	}
	if !strings.Contains(pr.RetryNote, "floor 757s") || !strings.Contains(pr.RetryNote, "min_turn_sec published by node-slow") {
		t.Fatalf("retry_note = %q, want the retry seat's 757 s floor and its publisher named", pr.RetryNote)
	}
	// The same contract with a FAST retry seat: 5 + (4096+4096)/100 = 86.92 → 87 s < 600 → the retry runs.
	fast, fastURL := eligibleNode(t, "node-fast", "qube from fast")
	fast.seatRate = map[string]any{"tok_s": 100.0, "cold_load_sec": 5.0, "samples": 3, "min_turn_sec": 46}
	fast.seatBudget = map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "off"}
	c2 := contracts(1)
	c2[0].TimeoutSec = 600
	results, sum, err = Run(context.Background(), testCfg(t), failingLocal(&localCalls), c2, "spread", []string{fastURL})
	if err != nil {
		t.Fatal(err)
	}
	if fast.dispatches.Load() != 1 || sum.Retried != 1 || results[0].RetriedOn != "node-fast" {
		t.Fatalf("retry must run on a fast seat: dispatches=%d retried=%d retried_on=%q note=%q", fast.dispatches.Load(), sum.Retried, results[0].RetriedOn, results[0].RetryNote)
	}
	// A node that publishes no rate keeps the pre-0.117.2 behaviour: the first
	// attempt's numbers (here none → the configured floor), and the note says so.
	r := &runner{cfg: testCfg(t)}
	r.cfg.AgentRetryMinSec = 300
	first := PlacedResult{Node: "local", Result: core.AgentWireResult{MinTurnSec: 350}}
	floor, src := r.retryFloorOn(first, placement{base: "http://x", view: NodeView{NodeID: "node-old"}}, remoteContract())
	if floor != 350 || !strings.Contains(src, "min_turn_sec of the seat on local") || !strings.Contains(src, "retry seat published no rate") {
		t.Fatalf("no published rate: floor = %d %q, want the first attempt's 350 s with the fallback named", floor, src)
	}
}
