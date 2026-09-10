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
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
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
