package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// End-to-end pins for the busy hold (ADR 0061) through runAgentTask: a fake
// llama-swap whose /running row carries the seat's own address (proxy) and a
// vLLM-shaped /metrics at that address. The liveness knobs are compressed the
// way agenttask_coldload_test.go compresses the cold-load ones (200 ms floor,
// 50 ms poll). Every stall test in agenttask_coldload_test.go and the
// held-tool-call test run against a fake with NO proxy, so their engine reads
// as unreadable and the ADR 0055 rule decides; these are the tests where the
// engine reads.

// compressBusyHold sets the busy-hold knobs for one test (production: flat 120
// s, poll 10 s) and returns the restore.
func compressBusyHold(t *testing.T, flat, poll time.Duration) func() {
	t.Helper()
	f, p := engineFlatBound, enginePoll
	engineFlatBound, enginePoll = flat, poll
	return func() { engineFlatBound, enginePoll = f, p }
}

// busySeatFake: while `busy` is set every /metrics read shows one more engine
// step AND one more generated token (the engine works for someone else); the
// loop's first request is silent for loopHold.
func busySeatFake(loopHold time.Duration, busy *atomic.Bool) (*agentFake, *atomic.Value) {
	var base atomic.Value
	var steps atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
		},
		seatMetrics: func(int64) string {
			if busy.Load() {
				steps.Add(1)
			}
			return fmt.Sprintf("vllm:num_requests_running 4\nvllm:num_requests_waiting 1\nvllm:iteration_tokens_total_count %d\nvllm:generation_tokens_total %d\nvllm:prompt_tokens_total 5000\n", steps.Load(), 100+steps.Load())
		},
		loop: func(n int64) string {
			if n == 1 {
				time.Sleep(loopHold)
			}
			return doneChat("answered after waiting its turn")
		},
		repack: func(int64) string { return `{"answer":"42"}` },
	}
	return fake, &base
}

// sseStep answers one chat request as a stream whose first byte arrives after
// `hold` (a request queued in the engine) and then sends frames.
func sseStep(w http.ResponseWriter, hold time.Duration, frames ...string) {
	time.Sleep(hold)
	fl := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		_, _ = w.Write([]byte("data: " + f + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		fl.Flush()
	}
}

// A request silent for 7x its prefill allowance while its seat's engine works
// for others survives, and the contention reaches the wire, the call meta and
// the ledger row: queued_ms is measured at all four hops. Mutants that leave
// the whole suite green: dropping `meta.QueuedMs = w.QueuedMs`, dropping
// `QueuedMs:` from entryFrom, renaming the wire key.
func TestRunAgentTaskHoldsARequestWhoseEngineWorksForOthers(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 400*time.Millisecond, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(1500*time.Millisecond, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)

	p := coldLoadTestPipeline(t, srv.URL)
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	res := p.Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a request waiting its turn on a busy engine was filed as a stall: %s / %q", wire.DeferClass, wire.Reason)
	}
	if wire.QueuedMs < 800 {
		t.Fatalf("wire queued_ms = %d, want ~1.2 s of a 1.5 s wait", wire.QueuedMs)
	}
	if res.Meta.QueuedMs != wire.QueuedMs {
		t.Fatalf("meta queued_ms = %d, wire = %d", res.Meta.QueuedMs, wire.QueuedMs)
	}
	if !strings.Contains(string(res.Data), `"queued_ms":`) { // the literal key is the contract between node and delegator versions
		t.Fatalf("the wire result carries no queued_ms key: %s", res.Data)
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil || len(rows) != 1 || rows[0].QueuedMs != wire.QueuedMs {
		t.Fatalf("ledger rows = %+v (%v), want one row carrying queued_ms %d", rows, err, wire.QueuedMs)
	}
}

// The same silence with an engine that does no work for anyone is a wedged
// seat: a stall filed as infrastructure, in ~ the allowance plus the flat
// bound, with the engine's silence in the reason.
func TestRunAgentTaskStallsAWedgedEngine(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	var busy atomic.Bool // stays false: a flat engine
	fake, base := busySeatFake(6*time.Second, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)

	start := time.Now()
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || !strings.Contains(wire.Reason, "the seat's engine did no work") || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("want an engine-flat infrastructure stall, got deferred=%v class=%s reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("a wedged engine was held for %s", el)
	}
}

// A seat /running lists with no proxy address cannot be read: the ADR 0055 rule
// decides and the reason says so. This is what every older stall test in this
// package actually exercises.
func TestRunAgentTaskWithoutASeatAddressKeepsTheOldRule(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}` },
		loop:      func(int64) string { time.Sleep(6 * time.Second); return doneChat("never read") },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || !strings.Contains(wire.Reason, "the engine could not be read") || !strings.Contains(wire.Reason, "in prefill") {
		t.Fatalf("reason = %q", wire.Reason)
	}
}

// Two silent steps in one run through the real streaming client and loop: the
// hold is entered, left on the first delta, the tool runs, and it is entered
// again. queued_ms is the sum of both.
func TestRunAgentTaskTwoHoldsInOneRun(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(0, &busy)
	fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			sseStep(w, 800*time.Millisecond,
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","type":"function","index":0,"function":{"name":"read_file","arguments":"{\"path\": \"notes.md\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":12}}`, `[DONE]`)
			return
		}
		sseStep(w, 800*time.Millisecond,
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"The answer is 42."},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":60,"completion_tokens":6}}`, `[DONE]`)
	}
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred || wire.Steps != 2 {
		t.Fatalf("deferred=%v steps=%d reason=%q", wire.Deferred, wire.Steps, wire.Reason)
	}
	if wire.QueuedMs < 900 {
		t.Fatalf("queued_ms = %d, want the two ~600 ms holds summed", wire.QueuedMs)
	}
}

// Silence in the middle of DECODING (the 2026-09-29 "four decode stalls in one
// second on one seat" shape): a preempted or time-shared request goes quiet
// for 4.5x its decoding allowance and resumes.
func TestRunAgentTaskDecodingSilenceOnABusyEngineIsHeld(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(0, &busy)
	fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte("data: " + s + "\n\n")); fl.Flush() } // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{"content":"The answer "},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{"content":"is "},"finish_reason":null}]}`)
		time.Sleep(900 * time.Millisecond)
		write(`{"choices":[{"index":0,"delta":{"content":"42."},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		write(`{"choices":[],"usage":{"prompt_tokens":60,"completion_tokens":6}}`)
		write(`[DONE]`)
	}
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred || wire.Output != "The answer is 42." || wire.QueuedMs < 400 {
		t.Fatalf("deferred=%v output=%q queued_ms=%d reason=%q", wire.Deferred, wire.Output, wire.QueuedMs, wire.Reason)
	}
}

// The engine keeps STEPPING but produces no token for anyone (a
// preempt-and-recompute thrash): stalled at the token bound (3 flat bounds by
// default), named as such. Mutant: dropping `TokenFingerprint:` in
// engineActivityProbe disables the token bound in production with every
// monitor and probe test green.
func TestRunAgentTaskStallsAThrashingEngine(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 400*time.Millisecond, 50*time.Millisecond)() // token bound = 3 x 400 ms
	var base atomic.Value
	var steps atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
		},
		seatMetrics: func(int64) string { // steps, preemptions and KV move; no token is ever credited
			n := steps.Add(1)
			return fmt.Sprintf("vllm:num_requests_running 4\nvllm:num_requests_waiting 3\nvllm:iteration_tokens_total_count %d\nvllm:generation_tokens_total 100\nvllm:prompt_tokens_total 5000\nvllm:num_preemptions_total %d\nvllm:kv_cache_usage_perc 0.98\n", n, n)
		},
		loop:   func(int64) string { time.Sleep(6 * time.Second); return doneChat("never read") },
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || !strings.Contains(wire.Reason, "produced no token") || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("a thrashing engine was held: deferred=%v class=%s reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
}

// A llama-server answers /metrics and /slots only between batches: one read
// may take far longer than a poll. Mutant: `http.Client{Timeout: enginePoll}`
// in engineActivityProbe (the pre-e894fcfd value) reads every busy seat as
// unreadable; TestLivenessPolicyArmsTheBusyHold checks the policy field, not
// this client.
func TestRunAgentTaskSlowEngineReadIsNotUnreadable(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 5*time.Second, 50*time.Millisecond)() // poll 50 ms; a read takes 300 ms
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(1500*time.Millisecond, &busy)
	inner := fake.seatMetrics
	fake.seatMetrics = func(n int64) string { time.Sleep(300 * time.Millisecond); return inner(n) }
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("a slow but answering engine read was treated as unreadable: %s", wire.Reason)
	}
}

// heldToolCallLoop / the undeclared arm of TestRunAgentTaskHeldToolCallOnVLLMSeatIsProgress
// asserts "without the engine's signal the hold is silence: stalled" — true
// only because its fake cannot be read. On a readable, working engine the same
// held call is a hold. (This is the twin that shows the old assertion now
// describes the fake, not the seat.)
func TestRunAgentTaskUndeclaredSeatHeldToolCallIsHeldWhenTheEngineReads(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var mu sync.Mutex
	var sawIDs []bool
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(0, &busy)
	fake.loopStream = heldToolLoop(1200*time.Millisecond, &mu, &sawIDs)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	p := agentTestPipeline(t, srv.URL)
	cfg := p.Cfg()
	cfg.AgentSeatTokS = 1000
	p.cfg = cfg // NOT declared vLLM: no return_token_ids
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("a held tool call on a readable, working engine was stalled: %s", wire.Reason)
	}
}

// The job record a delegator polls: prefill -> queued (the rolling allowance)
// -> decoding on the first byte. Also logs what the record shows so the
// allowance left on it after the hold is visible: at e894fcfd it still reads
// the hold's flat-bound-plus-poll allowance while decoding, until the next
// phase event.
func TestRunAgentTaskJobRecordLeavesQueuedOnTheFirstByte(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(0, &busy)
	fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		sseStep(w, 800*time.Millisecond,
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"The answer is 42."},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":" And more words."},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":60,"completion_tokens":6}}`, `[DONE]`)
	}
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	var mu sync.Mutex
	var reports []core.LiveProgress
	ctx := core.WithProgressReport(context.Background(), func(p core.LiveProgress) { mu.Lock(); reports = append(reports, p); mu.Unlock() })
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(ctx, agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	sawQueued := false
	for _, p := range reports {
		if p.Phase == "queued" {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatalf("the job record never showed the hold: %+v", reports)
	}
	if last := reports[len(reports)-1]; last.Phase == "queued" {
		t.Fatalf("the last report of a producing run still says queued (allowance %d ms)", last.AllowanceMs)
	}
}

// DECISION TEST — RED at 427ec6b1. A run that spends its ceiling in the hold
// (queued_ms ~ the ceiling) is filed as a BUDGET defer with the reason
// "ceiling 1s reached while producing (0 tok at 0.0 tok/s)". core/agentwire.go
// defines budget as "a larger budget helps"; the run was simply not served
// (capacity: "not this contract's turn"), and runAgentTask already refuses to
// file peer-wait timeouts as budget (contention.CausedTimeout) because that
// poisons the delegator's contract sizing. Before this release a busy seat
// killed such runs at the first stall (infrastructure); the hold moves them
// into the ceiling path. Pick the class, then assert it.
func TestRunAgentTaskCeilingWhileHeldIsNotABudgetDefer(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, 1)() // 1 s ceiling
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 30*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busySeatFake(6*time.Second, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.QueuedMs < 500 {
		t.Fatalf("precondition: deferred=%v queued_ms=%d reason=%q", wire.Deferred, wire.QueuedMs, wire.Reason)
	}
	if wire.DeferClass == core.DeferClassBudget || strings.Contains(wire.Reason, "while producing") {
		t.Fatalf("a run held %d ms of its 1 s ceiling was filed class=%s reason=%q", wire.QueuedMs, wire.DeferClass, wire.Reason)
	}
}
