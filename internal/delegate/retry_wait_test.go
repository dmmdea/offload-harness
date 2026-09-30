// retry_wait_test.go: the second chance queues for a busy seat (ADR 0063, decision 10),
// and says truthfully what happened while it did.

package delegate

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRetryWaitIsCreditedToTheRetryBudget: the time a retry stands in line is queueing,
// not work, so the retry keeps its whole budget. remainingSec rounds elapsed time UP to
// whole seconds, so a 300 ms wait hides the credit; a 2.5 s one does not.
func TestRetryWaitIsCreditedToTheRetryBudget(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var busyNow atomic.Int64
	busyNow.Store(1)
	var retrySec atomic.Int64
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.maxConcurrentJobs = 1
	node.jobsRunningFn = func() int { return int(busyNow.Load()) }
	node.queueDepthFn = func() int { return int(busyNow.Load()) }
	node.onDispatch = func(_ string, c core.AgentContract) { retrySec.Store(int64(c.TimeoutSec)) }
	go func() {
		time.Sleep(2500 * time.Millisecond)
		busyNow.Store(0)
	}()
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	var localCalls atomic.Int64
	c := verifiedContract() // TimeoutSec 30
	_, sum, err := RunWith(t.Context(), cfg, failingLocal(&localCalls), []core.AgentContract{c}, "spread", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RetryRecovered != 1 {
		t.Fatalf("summary = %+v, want the retry to have waited for the seat and recovered (fixture)", sum)
	}
	if got := retrySec.Load(); got < int64(c.TimeoutSec-2) {
		t.Fatalf("the retry was dispatched with timeout_sec=%d after a 2.5 s wait in line, want >= %d - the wait was charged to the retry", got, c.TimeoutSec-2)
	}
}

// TestRetrySeatWaitCanceledByTheCallerIsNotReportedAsBusy: the caller gave up while the
// retry stood in line. The seat was not shown to stay busy, so the retry note must not say
// it did ("already running another job ... a shared seat would only slow both").
func TestRetrySeatWaitCanceledByTheCallerIsNotReportedAsBusy(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.jobsRunning, node.queueDepth, node.maxConcurrentJobs = 1, 1, 1 // busy for good
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	var localCalls atomic.Int64
	results, _, err := RunWith(ctx, cfg, failingLocal(&localCalls), []core.AgentContract{verifiedContract()}, "spread", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	note := results[0].RetryNote
	if !strings.Contains(note, "canceled") {
		t.Fatalf("retry_note = %q, want it to say the caller canceled", note)
	}
	if strings.Contains(note, "a shared seat would only slow both") {
		t.Fatalf("retry_note = %q claims the seat stayed busy for a wait the caller cut short", note)
	}
}

// TestRetrySeatWaitNamesAndLogsAHealthReadThatFailed: a wait of up to agent_placement_wait_sec
// against a node that stopped answering health judged it from the round's own cached
// snapshot every tick and logged nothing, so it read like a wait against a node that kept
// saying "busy". The note names the failed read; the log says so once per node.
func TestRetrySeatWaitNamesAndLogsAHealthReadThatFailed(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	logs := captureLog(t)
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.jobsRunning, node.queueDepth, node.maxConcurrentJobs = 1, 1, 1 // busy while it answers
	// The spread deal, the retry's alternative and the first busy check are reads 1-3; every
	// read the wait makes after that fails.
	node.healthFailFn = func(n int64) bool { return n >= 4 }
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	var localCalls atomic.Int64
	results, _, err := RunWith(t.Context(), cfg, failingLocal(&localCalls), []core.AgentContract{verifiedContract()}, "spread", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	note := results[0].RetryNote
	if !strings.Contains(note, "fresh health could not be read") || !strings.Contains(note, "cached view") {
		t.Fatalf("retry_note = %q, want it to say the fresh read failed and the cached view was used", note)
	}
	if n := strings.Count(logs.String(), "retry seat "+url); n != 1 {
		t.Fatalf("the failed read was logged %d times, want exactly once per node:\n%s", n, logs.String())
	}
}
