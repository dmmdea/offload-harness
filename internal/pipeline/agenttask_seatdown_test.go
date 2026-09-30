package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

// End-to-end pins for the seat-down outcome (ADR 0066) through runAgentTask,
// against a fake llama-swap whose seat the test walks through a death: a wedge
// (the engine's counters freeze while llama-swap still lists the seat ready), a
// restart (`starting`, then a fresh engine whose counters begin again) or a
// start that keeps failing (HTTP 500 src=llama-swap). The liveness knobs are
// compressed the way the busy-hold tests compress them.

// seatLife is one seat's timeline. Before `restartAt` the seat reads ready with
// FROZEN counters (a wedged engine); from restartAt for `loadFor` it reads
// starting; then ready again with counters that begin from zero (a new
// incarnation). restartAt 0 = the seat never restarts.
type seatLife struct {
	base      atomic.Value // the fake's URL
	restartAt atomic.Int64
	loadFor   time.Duration
}

func (s *seatLife) state() string {
	r := s.restartAt.Load()
	if r == 0 {
		return "frozen"
	}
	now := time.Now().UnixNano()
	switch {
	case now < r:
		return "frozen"
	case now < r+int64(s.loadFor):
		return "starting"
	}
	return "new"
}

func (s *seatLife) running(int64) string {
	if s.state() == "starting" {
		return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x","proxy":"` + s.base.Load().(string) + `/seat"}]}`
	}
	return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + s.base.Load().(string) + `/seat"}]}`
}

func (s *seatLife) metrics(int64) string {
	if s.state() == "new" {
		return "vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count 3\nvllm:generation_tokens_total 10\nvllm:prompt_tokens_total 100\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.01\n"
	}
	// FROZEN: five requests running, nothing moves, whatever is asked.
	return "vllm:num_requests_running 5\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count 4000\nvllm:generation_tokens_total 90000\nvllm:prompt_tokens_total 1370000\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.4\n"
}

// answerOnce answers a loop call with a finished answer.
func answerOnce(w http.ResponseWriter) {
	sseStep(w, 0,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"The answer is 42."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":60,"completion_tokens":6}}`, `[DONE]`)
}

// seatDownPipeline is coldLoadTestPipeline with the admission gate off (a seat
// that reads `starting` must not be waited out in the pre-flight) and a short
// contention budget.
func seatDownPipeline(t *testing.T, base string, contentionSec int) *Pipeline {
	t.Helper()
	dir := t.TempDir()
	rates := `{"seats":{"` + agentTestSeat + `":{"tok_s":100,"prefill_tok_s":1000000,"samples":5}}}`
	if err := os.WriteFile(filepath.Join(dir, seatrate.FileName), []byte(rates), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Endpoint:              base,
		Model:                 "workhorse",
		AgentModel:            agentTestSeat,
		FleetNodeID:           "node-t",
		Temperature:           0.1,
		StateDir:              dir,
		AgentAdmissionWaitSec: -1,
		SeatContentionWaitSec: contentionSec,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// A seat that wedges and never comes back ends every run on it the same way:
// typed `seat down:`, in the bound the cold-load ceiling sets, with the engine's
// silence in the reason — not four independent per-run stalls. Four runs share
// the seat, as the four decode stalls of 2026-09-29 23:00:12 did.
func TestRunAgentTaskWedgedSeatFilesSeatDownPrefix(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 700*time.Millisecond)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	life := &seatLife{}
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		running:     life.running,
		seatMetrics: life.metrics,
		loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // a request that never answers
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	life.base.Store(srv.URL)

	start := time.Now()
	const runs = 4
	wires := make([]core.AgentWireResult, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wires[i] = decodeWire(t, seatDownPipeline(t, srv.URL, 0).Run(context.Background(), agentTestRequest(t, testContract())))
		}(i)
	}
	wg.Wait()
	for i, w := range wires {
		if !w.Deferred || w.DeferClass != core.DeferClassInfrastructure {
			t.Fatalf("run %d: want an infrastructure defer, got deferred=%v class=%q reason=%q", i, w.Deferred, w.DeferClass, w.Reason)
		}
		if !strings.HasPrefix(w.Reason, core.SeatDownReason) {
			t.Fatalf("run %d: reason = %q, want the %q prefix the delegator re-places on", i, w.Reason, core.SeatDownReason)
		}
		if !strings.Contains(w.Reason, "the seat's engine did no work") || !strings.Contains(w.Reason, "5 running") || !strings.Contains(w.Reason, "did not come back") {
			t.Fatalf("run %d: reason = %q, want the engine's silence, its 5 running requests and the wait that ran out", i, w.Reason)
		}
		if strings.Contains(w.Reason, "stalled: no progress") {
			t.Fatalf("run %d: filed as a per-run stall: %q", i, w.Reason)
		}
		if w.SeatRecoveries != 0 || w.SeatDownWaitSec < 0.5 {
			t.Fatalf("run %d: seat_recoveries=%d seat_down_wait_sec=%.2f, want no recovery and the ~0.7 s waited", i, w.SeatRecoveries, w.SeatDownWaitSec)
		}
	}
	if el := time.Since(start); el > 8*time.Second {
		t.Fatalf("four wedged runs took %s: the cold-load bound (700 ms) did not bound the wait", el)
	}
}

// A seat that wedges and then RESTARTS (the flagship's death: the engine's RPC
// timeout kills the API server, llama-swap starts it again) costs the run a
// wait, not its work: the run holds under the cold-load hold, re-issues the
// failed call on the new engine and finishes.
func TestRunAgentTaskSurvivesASeatRestart(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 8*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	life := &seatLife{loadFor: 400 * time.Millisecond}
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		running:     life.running,
		seatMetrics: life.metrics,
		loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				// The engine hangs under the first request, and restarts a second later.
				life.restartAt.Store(time.Now().Add(time.Second).UnixNano())
				<-r.Context().Done()
				return
			}
			answerOnce(w)
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	life.base.Store(srv.URL)
	contract := testContract()
	contract.OutputSchema = nil // the loop is under test

	wire := decodeWire(t, seatDownPipeline(t, srv.URL, 0).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("a seat restart ended the run: %s / %q", wire.DeferClass, wire.Reason)
	}
	if wire.Output != "The answer is 42." || wire.Steps != 1 {
		t.Fatalf("output=%q steps=%d, want the re-issued call's answer and no step spent", wire.Output, wire.Steps)
	}
	if wire.SeatRecoveries != 1 {
		t.Fatalf("seat_recoveries = %d, want 1", wire.SeatRecoveries)
	}
	if wire.SeatDownWaitSec < 0.8 {
		t.Fatalf("seat_down_wait_sec = %.2f, want the ~1 s waited for the restart", wire.SeatDownWaitSec)
	}
	if got := fake.loopCalls.Load(); got != 2 {
		t.Fatalf("loop calls = %d, want the hung call and its re-issue", got)
	}
}

// A 500 src=llama-swap while llama-swap reads the seat `starting` is the seat's
// failure to start, not peers holding its slots: the run must not spend the
// contention budget on it, and must not tell the operator to raise
// concurrencyLimit (the 2026-09-29 23:03-23:16 rows).
func TestRunAgentTaskHTTP500WhileSeatStartingIsNotContended(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 700*time.Millisecond)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
		},
		loopStatus: func(int64) int { return http.StatusInternalServerError },
		loop: func(int64) string {
			return `{"error":{"message":"upstream command exited prematurely but successfully","src":"llama-swap"}}`
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	wire := decodeWire(t, seatDownPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("want an infrastructure defer, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if strings.Contains(wire.Reason, "peers hold its slots") || strings.Contains(wire.Reason, "seat contended") || strings.Contains(wire.Reason, "concurrencyLimit") {
		t.Fatalf("a seat that cannot start was filed as contention: %q", wire.Reason)
	}
	if !strings.HasPrefix(wire.Reason, core.SeatDownReason) || !strings.Contains(wire.Reason, "starting") {
		t.Fatalf("reason = %q, want a seat-down naming the state llama-swap reports", wire.Reason)
	}
	if wire.ContentionWaitSec != 0 {
		t.Fatalf("contention_wait_sec = %.1f: the 30 s contention budget was spent on a seat that is not serving", wire.ContentionWaitSec)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("took %s: the contention budget (30 s), not the recovery bound (700 ms), governed the wait", el)
	}
}

// The same 500, but the seat comes back after 400 ms: the run waits for it under
// the recovery path and re-issues — the failure that revealed the outage costs
// the run a wait, not its work.
func TestRunAgentTaskSurvivesA500FromASeatThatRestarts(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 8*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	var restartAt atomic.Int64
	var base atomic.Value
	starting := func() bool {
		r := restartAt.Load()
		return r == 0 || time.Now().UnixNano() < r
	}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			if starting() {
				return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
		},
		seatMetrics: func(int64) string { // a fresh engine
			return "vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count 3\nvllm:generation_tokens_total 10\nvllm:prompt_tokens_total 100\n"
		},
		loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				restartAt.CompareAndSwap(0, time.Now().Add(400*time.Millisecond).UnixNano())
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"error":{"message":"upstream command exited prematurely","src":"llama-swap"}}`)
				return
			}
			answerOnce(w)
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	contract := testContract()
	contract.OutputSchema = nil

	wire := decodeWire(t, seatDownPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred || wire.Output != "The answer is 42." {
		t.Fatalf("deferred=%v output=%q reason=%q, want the run to finish after the seat came back", wire.Deferred, wire.Output, wire.Reason)
	}
	if wire.SeatRecoveries != 1 || wire.ContentionWaitSec != 0 {
		t.Fatalf("seat_recoveries=%d contention_wait_sec=%.1f, want 1 recovery and no contention budget spent", wire.SeatRecoveries, wire.ContentionWaitSec)
	}
}

// contendedReason follows the status (ADR 0066): a 429 is contention, a 5xx is
// llama-swap failing to serve the seat.
func TestContendedReasonIsStatusAware(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
		not    string
	}{
		{http.StatusTooManyRequests, "seat contended:", "seat not serving"},
		{http.StatusInternalServerError, "seat not serving:", "peers hold its slots"},
		{http.StatusServiceUnavailable, "seat not serving:", "seat contended"},
		{http.StatusBadGateway, "seat not serving:", "concurrencyLimit or retry"},
	} {
		b := contentionBudgetWith(t, tc.status)
		r, ok := contendedReason(agentTestSeat, b)
		if !ok || !strings.HasPrefix(r, tc.want) || strings.Contains(r, tc.not) {
			t.Fatalf("status %d: reason = %q (ok=%v), want prefix %q and never %q", tc.status, r, ok, tc.want, tc.not)
		}
		if !strings.Contains(r, fmt.Sprintf("HTTP %d", tc.status)) {
			t.Fatalf("status %d: reason %q must name the status", tc.status, r)
		}
	}
	if _, ok := contendedReason(agentTestSeat, nil); ok {
		t.Fatal("no budget, no contention")
	}
}

// contentionBudgetWith is a budget that has waited out one busy answer of the
// given status.
func contentionBudgetWith(t *testing.T, status int) *seatwait.Budget {
	t.Helper()
	b := seatwait.NewBudget(60)
	if _, ok := b.NextFor(status, "1"); !ok {
		t.Fatal("the budget refused its first wait")
	}
	return b
}
