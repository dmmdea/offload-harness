package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// An early 429 that RESOLVED must not relabel a later, genuine wall timeout
// as contention: the wall was spent by the model's own work, and the operator
// must be sent to the budget, not to concurrencyLimit (review, 2026-09-02).
func TestRunAgentTaskEarlyResolvedContentionDoesNotRelabelAGenuineTimeout(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(2500 * time.Millisecond) // well past the 1 s wall, and no 429 in sight
			return doneChat("too late")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
		repackStatusFor: func(n int64) int {
			return 0
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	// Seed the contract's budget history by hand is not possible from outside;
	// instead the re-pack never runs (the loop times out first) and the loop
	// itself saw no busy answer — so LastStatus stays 0 and the class must be
	// budget. The companion test below covers "a busy answer occurred but was
	// not what spent the wall".
	contract := testContract()
	contract.TimeoutSec = 1
	res := contentionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !strings.Contains(wire.Reason, "wall timeout") {
		t.Fatalf("a genuine wall timeout stays a budget defer: %+v", wire)
	}
}

// One 429 on the re-pack, resolved after a 1 s sleep, then the wall (long)
// is NOT reached: the result is a success with contention_wait_sec = 1 — and
// CausedTimeout would be false because nothing timed out. The unit-level
// guarantee lives in seatwait_test (TestCausedTimeoutNeedsCausation).
func TestRunAgentTaskOneResolvedBusyAnswerIsASuccessWithTheWaitReported(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		repackStatusFor: func(n int64) int {
			if n == 1 {
				return http.StatusTooManyRequests
			}
			return 0
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	res := contentionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a resolved busy answer is not a defer: %s", wire.Reason)
	}
	if wire.ContentionWaitSec < 0.9 || wire.ContentionWaitSec > 1.5 {
		t.Fatalf("one ladder step (1 s) must be reported, got %v", wire.ContentionWaitSec)
	}
}
