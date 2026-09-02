package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// contentionTestPipeline is agentTestPipeline with the seatwait budget set.
func contentionTestPipeline(t *testing.T, base string, waitSec int) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:              base,
		Model:                 "workhorse",
		AgentModel:            agentTestSeat,
		FleetNodeID:           "node-t",
		Temperature:           0.1,
		SeatContentionWaitSec: waitSec,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// A peer-held seat (429) with the wait DISABLED defers as before, but the
// reason now says "seat contended:" — the ledger grep key — and the wire
// carries a zero contention_wait_sec.
func TestRunAgentTaskContendedSeatWithoutWaitIsNamed(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repackStatus: http.StatusTooManyRequests,
	}
	srv := fake.server(t)
	defer srv.Close()
	res := contentionTestPipeline(t, srv.URL, -1).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("want an infrastructure defer, got %+v", wire)
	}
	if !strings.Contains(wire.Reason, "seat contended:") || !strings.Contains(wire.Reason, "429") {
		t.Fatalf("reason must name the contention and the status: %q", wire.Reason)
	}
	if wire.ContentionWaitSec != 0 {
		t.Fatalf("no wait was configured, contention_wait_sec = %v", wire.ContentionWaitSec)
	}
}

// With a budget, the re-pack waits on the SAME seat and succeeds once a slot
// frees; the wait is reported on the wire, and the result is not a defer.
func TestRunAgentTaskContendedSeatIsWaitedOutAndReported(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		repackStatusFor: func(n int64) int {
			if n <= 2 {
				return http.StatusTooManyRequests
			}
			return 0
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	res := contentionTestPipeline(t, srv.URL, 10).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("the seat freed after two busy answers; got a defer: %s", wire.Reason)
	}
	if wire.ContentionWaitSec < 2.9 { // ladder 1 s + 2 s
		t.Fatalf("contention_wait_sec must report the wait, got %v", wire.ContentionWaitSec)
	}
}
