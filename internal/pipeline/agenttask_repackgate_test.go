package pipeline

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The structured re-pack sleeps the SAME contention budget the loop's chat calls do
// (llamaclient asks seatwait.Budget.NextFor before every wait), and an empty 502 —
// llama-swap's proxy failing to reach an upstream that is between states — is one of
// the answers the budget treats as "peers hold the seat". While the seat is NOT
// serving that is wrong: it sleeps up to the whole budget (90 s by default) before
// the typed `seat down:` defer, and the wire then reports a contention wait on a
// defer that is not contention. The seat gate on the budget stops it for every
// client at once (ADR 0066); the control shows a seat that IS serving keeps the
// ordinary wait.
func TestRunAgentTaskRepackDoesNotSleepTheContentionBudgetOnASeatThatIsNotServing(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	for _, tc := range []struct {
		name     string
		seatDown bool
	}{
		{"the seat reads starting once the re-pack's 502 arrives: no contention sleep", true},
		{"control: the seat reads ready and readable: the 502 is waited out as before", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var down atomic.Bool
			var base atomic.Value
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				running: func(int64) string {
					if down.Load() {
						return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
					}
					return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
				},
				seatMetrics: freshEngine,
				loop:        func(int64) string { return doneChat("The answer is 42.") },
				repackStatusFor: func(int64) int {
					if tc.seatDown {
						down.Store(true)
					}
					return http.StatusBadGateway // empty body: llama-swap's proxy between states
				},
				chatFallbackStatus: http.StatusBadGateway,
			}
			srv := fake.server(t)
			defer srv.Close()
			base.Store(srv.URL)

			budgetSec := 3
			if tc.seatDown {
				budgetSec = 30 // the budget a sleeping client would spend
			}
			start := time.Now()
			wire := decodeWire(t, seatDownPipeline(t, srv.URL, budgetSec).Run(context.Background(), agentTestRequest(t, testContract())))
			el := time.Since(start)
			if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
				t.Fatalf("want an infrastructure defer, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
			}
			if tc.seatDown {
				if !strings.HasPrefix(wire.Reason, core.SeatDownReason) || !strings.Contains(wire.Reason, "during the structured re-pack") {
					t.Fatalf("reason = %q, want %q prefixed and naming the re-pack", wire.Reason, core.SeatDownReason)
				}
				if wire.ContentionWaitSec != 0 || el > 8*time.Second {
					t.Fatalf("contention_wait_sec=%.1f after %s: the re-pack slept the contention budget on a seat that is not serving", wire.ContentionWaitSec, el)
				}
				return
			}
			if strings.HasPrefix(wire.Reason, core.SeatDownReason) || wire.ContentionWaitSec <= 0 {
				t.Fatalf("reason=%q contention_wait_sec=%.1f: a seat that serves keeps the ordinary contention wait", wire.Reason, wire.ContentionWaitSec)
			}
		})
	}
}
