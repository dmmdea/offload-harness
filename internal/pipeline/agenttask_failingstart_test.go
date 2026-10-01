package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The 2026-09-29 23:01-23:20 shape: the seat's engine died, llama-swap does not list
// it, and every request that tries to start it gets an instant HTTP 500 (the
// launcher refused for 18 minutes). A run whose seat cannot start must not turn each
// request into two quick 500s and a defer (INV-4): it waits at the poll and triggers
// the start again, inside the cold-load bound counted from the first failure.

// failingStartSeat is a fake llama-swap whose seat is unlisted until `up` is set,
// and whose first `failures` loop requests each answer a failed start.
func failingStartSeat(t *testing.T, failures int64) (*agentFake, *httptest.Server) {
	t.Helper()
	var up atomic.Bool
	var base atomic.Value
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			if !up.Load() {
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
		},
		seatMetrics: freshEngine,
		loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if failures < 0 || n <= failures {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"error":{"message":"upstream command exited prematurely","src":"llama-swap"}}`)
				return
			}
			up.Store(true) // the start finally succeeded, and this request is served by it
			answerOnce(w)
		},
	}
	srv := fake.server(t)
	t.Cleanup(srv.Close)
	base.Store(srv.URL)
	return fake, srv
}

func TestRunAgentTaskSurvivesAStartThatFailsThreeTimes(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 8*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	fake, srv := failingStartSeat(t, 3)
	contract := testContract()
	contract.OutputSchema = nil

	start := time.Now()
	wire := decodeWire(t, seatDownPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred || wire.Output != "The answer is 42." {
		t.Fatalf("deferred=%v output=%q reason=%q, want the run to finish once the start succeeded", wire.Deferred, wire.Output, wire.Reason)
	}
	if got := fake.loopCalls.Load(); got != 4 {
		t.Fatalf("loop calls = %d, want three failed starts and the request the fourth start served", got)
	}
	if wire.SeatRecoveries != 1 {
		t.Fatalf("seat_recoveries = %d, want ONE recovery for the outage (three failed starts recovered nothing)", wire.SeatRecoveries)
	}
	if wire.ContentionWaitSec != 0 || strings.Contains(wire.Reason, "seat contended") {
		t.Fatalf("contention_wait_sec = %.1f: a seat that cannot start is not contention", wire.ContentionWaitSec)
	}
	if wire.SeatDownWaitSec <= 0 || wire.SeatDownWaitSec > time.Since(start).Seconds() {
		t.Fatalf("seat_down_wait_sec = %.2f of a %.2f s run, want the whole episode booked", wire.SeatDownWaitSec, time.Since(start).Seconds())
	}
}

func TestRunAgentTaskWaitsOutAStartThatNeverSucceedsThenFilesSeatDown(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 700*time.Millisecond)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	fake, srv := failingStartSeat(t, -1)
	contract := testContract()
	contract.OutputSchema = nil

	start := time.Now()
	wire := decodeWire(t, seatDownPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract)))
	el := time.Since(start)
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure || !strings.HasPrefix(wire.Reason, core.SeatDownReason) {
		t.Fatalf("deferred=%v class=%q reason=%q, want a typed seat-down defer", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.Reason, "start attempt") || !strings.Contains(wire.Reason, "did not come back") || strings.Contains(wire.Reason, "came back and it went down again") {
		t.Fatalf("reason = %q, want the start attempts named and no claim that the seat came back", wire.Reason)
	}
	if el < 600*time.Millisecond || el > 8*time.Second {
		t.Fatalf("the run took %s: it must wait to the 700 ms cold-load bound, not end after two quick attempts", el)
	}
	if got := fake.loopCalls.Load(); got < 3 || got > 30 {
		t.Fatalf("loop calls = %d, want several paced start attempts and no tight loop", got)
	}
	if wire.SeatRecoveries != 0 || wire.SeatDownWaitSec < 0.5 || wire.ContentionWaitSec != 0 {
		t.Fatalf("seat_recoveries=%d seat_down_wait_sec=%.2f contention_wait_sec=%.1f, want no recovery, the ~0.7 s waited and no contention budget spent",
			wire.SeatRecoveries, wire.SeatDownWaitSec, wire.ContentionWaitSec)
	}
}
