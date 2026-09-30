package pipeline

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// slowFirstDeltaLoop streams one final answer whose first delta arrives only
// after `wait` — a request queued behind siblings on a busy engine — and
// reports 5,000 uncached prompt tokens, so the run carries a prefill sample.
func slowFirstDeltaLoop(wait time.Duration) func(int64, map[string]any, http.ResponseWriter, *http.Request) {
	return func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte("data: " + s + "\n\n")); fl.Flush() } // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"The answer is 42."},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		write(`{"choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":6}}`)
		write(`[DONE]`)
	}
}

// A run held in the busy hold (ADR 0061) waited behind other requests, so its
// first-delta wall is not the seat's prefill speed: folding it into
// seat-rates.json would lower the measured rate and inflate every later
// allowance, ceiling and placement ETA on that seat. The same run with an
// allowance long enough that no hold happens (the control) records its sample.
// Both arms wait the same 600 ms for the first delta; only the hold differs.
func TestRunAgentTaskHeldRunDoesNotFeedTheSeatRates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		floor    time.Duration
		wantHeld bool
	}{
		{"held run: the sample is not the seat's rate", 200 * time.Millisecond, true},
		{"control: an unheld run records its sample", 3 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer compressLiveness(t, tc.floor, 100*time.Millisecond, core.AgentCeilingSecCap)()
			var srvURL string
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				running: func(int64) string {
					return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + srvURL + `/seat"}]}`
				},
				// An engine working for other requests: every read sees new steps.
				seatMetrics: func(n int64) string {
					return "vllm:num_requests_running 4\nvllm:num_requests_waiting 1\nvllm:iteration_tokens_total_count " +
						strconv.FormatInt(100*n, 10) + "\nvllm:generation_tokens_total " + strconv.FormatInt(40*n, 10) +
						"\nvllm:prompt_tokens_total 9000\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.5\n"
				},
				loopStream: slowFirstDeltaLoop(600 * time.Millisecond),
			}
			srv := fake.server(t)
			defer srv.Close()
			srvURL = srv.URL
			p := coldLoadTestPipeline(t, srv.URL)
			contract := testContract()
			contract.OutputSchema = nil // the loop is under test, not the re-pack
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
			if wire.Deferred {
				t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
			}
			if held := wire.QueuedMs > 0; held != tc.wantHeld {
				t.Fatalf("queued_ms = %d (engine reads %d), want held=%v", wire.QueuedMs, fake.seatMetricsCNT.Load(), tc.wantHeld)
			}
			store, err := seatrate.Load(filepath.Join(p.Cfg().StateDir, seatrate.FileName))
			if err != nil {
				raw, _ := os.ReadFile(filepath.Join(p.Cfg().StateDir, seatrate.FileName))
				t.Fatalf("seat-rates store: %v (%s)", err, raw)
			}
			got := store.Seats[agentTestSeat].PrefillTokS
			if tc.wantHeld && got != 1000000 {
				t.Fatalf("a held run changed the seat's prefill rate: %v, want the stored 1000000", got)
			}
			if !tc.wantHeld && got == 1000000 {
				t.Fatalf("the unheld control did not record its prefill sample (rate still %v): this test cannot tell the gate from a dead observation path", got)
			}
		})
	}
}
