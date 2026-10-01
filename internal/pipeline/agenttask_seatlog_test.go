package pipeline

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A recovery leaves no other trace than two wire numbers, and a seat that cannot be
// read from here silently gets no seat-down handling: the run says both once.
func TestRunAgentTaskLogsWhatItsSeatDidToIt(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 8*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()

	capture := func(t *testing.T) *bytes.Buffer {
		var buf bytes.Buffer
		oldOut, oldFlags := log.Writer(), log.Flags()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
		return &buf
	}

	t.Run("a recovery is logged", func(t *testing.T) {
		buf := capture(t)
		fake, srv := failingStartSeat(t, 1)
		contract := testContract()
		contract.OutputSchema = nil
		wire := decodeWire(t, seatDownPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract)))
		if wire.Deferred || wire.SeatRecoveries != 1 || fake.loopCalls.Load() != 2 {
			t.Fatalf("deferred=%v recoveries=%d calls=%d (premise: one failed start, then a recovery)", wire.Deferred, wire.SeatRecoveries, fake.loopCalls.Load())
		}
		if !strings.Contains(buf.String(), "went down under the run: 1 recovery(ies)") {
			t.Fatalf("no recovery line in the log:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "could not be read") {
			t.Fatalf("a readable seat was logged as unreadable:\n%s", buf.String())
		}
	})

	t.Run("a seat that cannot be read is logged", func(t *testing.T) {
		buf := capture(t)
		var hits atomic.Int64
		fake := &agentFake{
			rosterIDs:     []string{agentTestSeat},
			runningStatus: http.StatusInternalServerError, // llama-swap is up but answers nothing useful
			loopStatus:    func(int64) int { hits.Add(1); return http.StatusInternalServerError },
			loop: func(int64) string {
				return `{"error":{"message":"upstream command exited prematurely","src":"llama-swap"}}`
			},
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, seatDownPipeline(t, srv.URL, 1).Run(context.Background(), agentTestRequest(t, testContract())))
		if !wire.Deferred {
			t.Fatalf("the run finished against a seat that answers only 500: %+v", wire)
		}
		if !strings.Contains(buf.String(), "could not be read") || !strings.Contains(buf.String(), "read as an ordinary error") {
			t.Fatalf("no unreadable-seat line in the log:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "went down under the run") {
			t.Fatalf("a run whose seat never went down logged a recovery:\n%s", buf.String())
		}
	})
}
