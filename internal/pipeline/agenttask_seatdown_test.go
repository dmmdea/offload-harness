package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
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

func (s *seatLife) metrics(n int64) string {
	if s.state() == "new" {
		// A fresh engine that is WORKING: its counters advance on every read. A
		// constant reading would make any re-issued call slower than the compressed
		// allowance plus flat bound (200 + 300 ms) a wedge again — a fixture that
		// flakes under CPU contention, not a seat that is down.
		return freshEngine(n)
	}
	// FROZEN: five requests running, nothing moves, whatever is asked.
	return "vllm:num_requests_running 5\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count 4000\nvllm:generation_tokens_total 90000\nvllm:prompt_tokens_total 1370000\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.4\n"
}

// freshEngine is a new engine incarnation's /metrics on its n-th read: one running
// request, counters that start over and advance with every read.
func freshEngine(n int64) string {
	return "vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count " + strconv.FormatInt(3+n, 10) +
		"\nvllm:generation_tokens_total " + strconv.FormatInt(10+n, 10) + "\nvllm:prompt_tokens_total 100\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.01\n"
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
	// The restart completes 1.4 s after the hung call (1 s + the 400 ms load) and
	// the wedge is filed about 0.5 s after it: nominally ~0.9 s waited. The floor
	// leaves a verdict up to a second late — the lower bound of a quantity that
	// SHRINKS as the verdict is delayed must not sit near its nominal value.
	if wire.SeatDownWaitSec < 0.3 {
		t.Fatalf("seat_down_wait_sec = %.2f, want the wait for the restart (about 0.9 s on this schedule)", wire.SeatDownWaitSec)
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
		seatMetrics: freshEngine, // a fresh engine that is working
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

// A seat lost while the run is in its structured re-pack — after the loop
// finished its answer — ends the run typed too: the transport failure is
// confirmed against llama-swap (the seat reads starting) and filed `seat down:`,
// so the delegator re-places the contract; the finished answer stays in output
// for the caller. With the seat reading ready and readable the same failure is
// the ordinary `structured re-pack unreachable:` it always was.
func TestRunAgentTaskSeatDownDuringTheRepackKeepsThePrefix(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	for _, tc := range []struct {
		name     string
		seatDown bool
	}{
		{"the seat reads starting once the re-pack fails: seat down", true},
		{"control: the seat reads ready and readable: an ordinary re-pack failure", false},
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
				seatMetrics: func(int64) string {
					return "vllm:num_requests_running 1\nvllm:num_requests_waiting 0\nvllm:iteration_tokens_total_count 3\nvllm:generation_tokens_total 10\nvllm:prompt_tokens_total 100\n"
				},
				loop: func(int64) string { return doneChat("The answer is 42.") },
				repackStatusFor: func(int64) int { // the grammar lane dies under the re-pack
					if tc.seatDown {
						down.Store(true)
					}
					return http.StatusInternalServerError
				},
				chatFallbackStatus: http.StatusInternalServerError,
			}
			srv := fake.server(t)
			defer srv.Close()
			base.Store(srv.URL)

			wire := decodeWire(t, seatDownPipeline(t, srv.URL, 0).Run(context.Background(), agentTestRequest(t, testContract())))
			if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
				t.Fatalf("want an infrastructure defer, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
			}
			if wire.Output != "The answer is 42." {
				t.Fatalf("output = %q: the finished answer must stay for the caller", wire.Output)
			}
			if tc.seatDown {
				if !strings.HasPrefix(wire.Reason, core.SeatDownReason) || !strings.Contains(wire.Reason, "during the structured re-pack") {
					t.Fatalf("reason = %q, want %q prefixed and naming the re-pack", wire.Reason, core.SeatDownReason)
				}
				return
			}
			if !strings.HasPrefix(wire.Reason, "structured re-pack unreachable: ") || strings.Contains(wire.Reason, core.SeatDownReason) {
				t.Fatalf("reason = %q, want the ordinary re-pack transport failure", wire.Reason)
			}
		})
	}
}

// repackSeatDown asks the monitor's own verdict FIRST: a wedge filed during the
// re-pack wraps the engine-flat stall it replaced, so stallOf reads it too — and
// the stall arm of runAgentTask would file it without the prefix.
func TestRepackSeatDownPrefersTheMonitorsWedgeVerdict(t *testing.T) {
	pol := agent.StallPolicy{Floor: 40 * time.Millisecond, PrefillTokS: 1000, TokS: 100, Slack: 10 * time.Millisecond,
		Repack: 120 * time.Millisecond, EngineFlat: 150 * time.Millisecond, EnginePoll: 20 * time.Millisecond, EngineProbeTimeout: time.Second}
	frozen := func(context.Context) (agent.EngineReading, error) {
		return agent.EngineReading{Fingerprint: "v|1", TokenFingerprint: "vt|1", Summary: "vllm-metrics: 4 running, 0 waiting", Running: 4}, nil
	}
	ctx, m := agent.NewMonitor(context.Background(), pol, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(frozen)
	m.Phase(agent.PhaseRepack, 0)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a frozen engine under the re-pack was never declared down")
	}
	if stallOf(m) == nil || seatDownOf(m) == nil {
		t.Fatalf("stallOf=%v seatDownOf=%v: a wedge must read as both (it wraps the stall it replaced) — the reason that leads with the prefix is the seat-down one", stallOf(m), seatDownOf(m))
	}
	got := repackSeatDown(context.Background(), m, errors.New("Post: context canceled"), false)
	if !strings.HasPrefix(got, core.SeatDownReason) || !strings.Contains(got, "during the structured re-pack") || !strings.Contains(got, "4 running") {
		t.Fatalf("repackSeatDown = %q, want the seat-down reason with the engine's gauges", got)
	}
	// Nothing filed and no transport failure: no verdict.
	_, quiet := agent.NewMonitor(context.Background(), pol, 10*time.Second)
	defer quiet.Stop()
	if got := repackSeatDown(context.Background(), quiet, errors.New("output failed schema"), false); got != "" {
		t.Fatalf("repackSeatDown = %q for a re-pack that failed on its schema", got)
	}
	if got := repackSeatDown(context.Background(), nil, errors.New("x"), true); got != "" {
		t.Fatalf("repackSeatDown = %q without a monitor", got)
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

// The re-issued call after a seat restart is the request that WAITS OUT llama-swap's
// load: its time-to-first-delta and its wall carry the seat's cold load, so what it
// teaches the store is not the seat's own rate (a 12k-token prompt behind a 200 s
// load reads as a ~49 tok/s prefill). A run that waited out a seat going down never
// moves the published prefill or decode rate — PR-13's fail condition — while a run
// that met no outage still does (the control), so the test can tell the gate from a
// dead observation path.
func TestRunAgentTaskARunThatRecoveredFromASeatDownDoesNotTeachTheSeatRates(t *testing.T) {
	defer compressLiveness(t, 2*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 8*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	for _, tc := range []struct {
		name        string
		outage      bool
		wantTeaches bool
	}{
		{"a restart: the re-issue waits out the load and teaches nothing", true, false},
		{"control: no outage, the same slow first delta teaches the rates", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var restartAt atomic.Int64
			var base atomic.Value
			starting := func() bool {
				r := restartAt.Load()
				return tc.outage && (r == 0 || time.Now().UnixNano() < r)
			}
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				running: func(int64) string {
					if starting() {
						return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
					}
					return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
				},
				seatMetrics: freshEngine,
				loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
					if tc.outage && n == 1 {
						restartAt.CompareAndSwap(0, time.Now().Add(400*time.Millisecond).UnixNano())
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, `{"error":{"message":"upstream command exited prematurely","src":"llama-swap"}}`)
						return
					}
					// the (re-)issued request is held while llama-swap loads the seat
					sseStep(w, 1500*time.Millisecond,
						`{"choices":[{"index":0,"delta":{"role":"assistant","content":"The answer is 42."},"finish_reason":null}]}`,
						`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
						`{"choices":[],"usage":{"prompt_tokens":50000,"completion_tokens":2000}}`, `[DONE]`)
				},
			}
			srv := fake.server(t)
			defer srv.Close()
			base.Store(srv.URL)
			contract := testContract()
			contract.OutputSchema = nil
			p := seatDownPipeline(t, srv.URL, 30)
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
			if wire.Deferred {
				t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
			}
			if tc.outage && wire.SeatRecoveries != 1 {
				t.Fatalf("seat_recoveries = %d: the run did not recover, so this test is not exercising the recovery (premise)", wire.SeatRecoveries)
			}
			got := storedSeat(t, p.Cfg().StateDir)
			moved := got.PrefillTokS != 1000000 || got.TokS != 100
			if moved != tc.wantTeaches {
				t.Fatalf("prefill_tok_s=%v tok_s=%v: moved=%v, want moved=%v (stored 1000000 / 100)", got.PrefillTokS, got.TokS, moved, tc.wantTeaches)
			}
		})
	}
}

// The decision itself: a run that queued in the busy hold, recovered from a seat
// going down, or merely waited on one that never came back timed something other
// than the seat's own rate.
func TestTimedTheSeatsOwnRates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		contended  bool
		recoveries int
		waited     time.Duration
		want       bool
	}{
		{"a run that met nothing", false, 0, 0, true},
		{"queued in the busy hold", true, 0, 0, false},
		{"recovered from a seat going down", false, 1, 0, false},
		{"waited on a seat that never came back", false, 0, 30 * time.Second, false},
		{"recovered, and waited", false, 2, 45 * time.Second, false},
		{"queued and recovered", true, 1, time.Second, false},
	} {
		if got := timedTheSeatsOwnRates(tc.contended, tc.recoveries, tc.waited); got != tc.want {
			t.Errorf("%s: timedTheSeatsOwnRates = %v, want %v", tc.name, got, tc.want)
		}
	}
}
