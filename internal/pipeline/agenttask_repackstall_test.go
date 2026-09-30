package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// compressRepackBound sets the flat re-pack bound (production: 120 s) for one
// test and returns the restore.
func compressRepackBound(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := livenessRepack
	livenessRepack = d
	return func() { livenessRepack = old }
}

// A re-pack the monitor has cancelled sends no further request (register C-66,
// RC-6). Attempts 2 and 3 used to start on the already-dead context, fail at
// once, and be counted: 119 of 119 stalled re-packs read repack_attempts 3 with
// repack_ms 120,001, which is ONE real request.
func TestRepackStopsRetryingOnceTheMonitorCancels(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackDelay:  2 * time.Second, // silent far past the compressed allowance
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)

	pol := agent.StallPolicy{Floor: 200 * time.Millisecond, Repack: 200 * time.Millisecond, Slack: 10 * time.Millisecond}
	cctx, live := agent.NewMonitor(context.Background(), pol, 30*time.Second)
	defer live.Stop()
	live.Phase(agent.PhaseRepack, 0)
	ctx := agent.ContextWithProgress(cctx, live.Progress)

	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	start := time.Now()
	_, _, _, attempts, err := p.repackStructured(ctx, agentTestSeat, schema, "The answer is 42.", 0)

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a cancelled re-pack sent one request, not three", attempts)
	}
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar requests = %d, want 1", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("chat fallback requests = %d, want 0", got)
	}
	var se *agent.StallError
	if !errors.As(err, &se) || se.Phase != agent.PhaseRepack {
		t.Fatalf("err = %v, want the monitor's re-pack stall (a *agent.StallError) in the chain", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the re-pack took %s after a 200 ms stall: it kept going", el)
	}
}

// The same through the whole run: the wire says one attempt and one note, not
// three identical "attempt n/3 (bound ...)" notes.
func TestRunAgentTaskRepackStallReportsOneAttempt(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackDelay:  2 * time.Second,
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure || !strings.Contains(wire.Reason, "stalled: no progress for") || !strings.Contains(wire.Reason, "in repack") {
		t.Fatalf("want an infrastructure stall in repack, got deferred=%v class=%s reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1", wire.RepackAttempts)
	}
	if strings.Contains(wire.RepackNote, "attempt 2/3") || strings.Contains(wire.RepackNote, "attempt 3/3") {
		t.Fatalf("repack_note = %q: phantom attempts are still on the wire", wire.RepackNote)
	}
	if fake.grammarCNT.Load() != 1 || fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("seat saw %d grammar + %d chat re-pack requests, want 1 + 0", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
}

// busyEngineRepackFake: a seat whose engine works for others while the re-pack
// request is silent for `hold` (a request waiting its turn in the engine's queue).
func busyEngineRepackFake(hold time.Duration, busy *atomic.Bool) (*agentFake, *atomic.Value) {
	fake, base := busySeatFake(0, busy)
	fake.repackDelay = hold
	return fake, base
}

// A re-pack request held by the busy hold is not cut by a transport bound of its
// own (ADR 0061, PR-5). That bound was min(seat allowance, wall left / attempts
// owed): about a third of what was left of the run, ~50 s into a hold in
// production. It cut the request the monitor was holding on purpose and
// re-sent it from the back of the engine's queue, up to three times.
func TestRepackHeldOnABusyEngineIsNotCutByItsTransportBound(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, 6)() // ceiling 6 s: the old bound was ~1.9 s
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busyEngineRepackFake(2500*time.Millisecond, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("a re-pack waiting its turn on a busy engine was cut: %s / %q (attempts %d)", wire.DeferClass, wire.Reason, wire.RepackAttempts)
	}
	if wire.RepackAttempts != 1 || fake.grammarCNT.Load() != 1 {
		t.Fatalf("attempts = %d, grammar requests = %d, want the one request left to finish", wire.RepackAttempts, fake.grammarCNT.Load())
	}
	if !strings.Contains(string(wire.Structured), `"answer":"42"`) {
		t.Fatalf("structured = %s", wire.Structured)
	}
}

// And it is the HOLD that governs the wait: with the flat re-pack bound
// compressed under the seat's silence, the monitor reads the engine, sees it
// working for others, and holds the request instead of filing a stall. The
// contention is booked as queued_ms.
func TestRepackSilentOnABusyEngineIsHeldNotStalled(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 2*time.Second, 50*time.Millisecond)()
	var busy atomic.Bool
	busy.Store(true)
	fake, base := busyEngineRepackFake(1500*time.Millisecond, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("a silent re-pack on a busy engine was filed as a stall: %s / %q", wire.DeferClass, wire.Reason)
	}
	if wire.QueuedMs < 500 {
		t.Fatalf("queued_ms = %d, want the ~1.2 s the re-pack spent held", wire.QueuedMs)
	}
}

// The guard: the same silence on an engine that does no work for anyone is a
// wedged seat, still a stall, filed at the flat bound — the fix lets the hold
// decide, it does not let a dead seat hold a re-pack for the ceiling.
func TestRepackSilentOnAWedgedEngineStillStalls(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	defer compressBusyHold(t, 400*time.Millisecond, 50*time.Millisecond)()
	var busy atomic.Bool // stays false: a flat engine
	fake, base := busyEngineRepackFake(2500*time.Millisecond, &busy)
	srv := fake.server(t)
	defer srv.Close()
	base.Store(srv.URL)

	start := time.Now()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure || !strings.Contains(wire.Reason, "the seat's engine did no work") {
		t.Fatalf("want an engine-flat infrastructure stall, got deferred=%v class=%s reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1", wire.RepackAttempts)
	}
	if el := time.Since(start); el > 2400*time.Millisecond {
		t.Fatalf("a wedged engine held the re-pack for %s", el)
	}
}

// The job record shows the re-pack phase with the allowance it runs under. A
// remote delegator keeps polling a job only while the node's last report is
// inside its published allowance plus grace: the re-pack told the monitor and
// nobody else, so the record kept the loop's last decode allowance and a re-pack
// sized to minutes was abandoned by the delegator while the node ran it on.
//
// The floor is five seconds, not a few hundred milliseconds: the LOOP's own first
// answer has to beat the compressed floor on a loaded machine and did not always
// (one run in fifteen stalled in the loop's prefill before any re-pack began, and
// a floor of one second still lost one in fifteen). The answer is long enough that
// the arithmetic allowance (~21 s) stands well clear of that floor, so a re-pack
// started without its size (or sized to the completion cap instead of the answer)
// reads an allowance of 5 s or 28 s and fails the window.
func TestRepackPublishesItsPhaseAndAllowanceToTheJobRecord(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 200*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		loop:        func(int64) string { return doneChat(strings.Repeat("The answer is 42. ", 220)) },
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 300 * time.Millisecond,
	}
	srv := fake.server(t)
	defer srv.Close()

	var mu sync.Mutex
	var reports []core.LiveProgress
	ctx := core.WithProgressReport(context.Background(), func(p core.LiveProgress) { mu.Lock(); reports = append(reports, p); mu.Unlock() })
	// A seat measured at 100 tok/s: 1,383 expected tokens x 1.5 / 100 + the 100 ms slack = ~20.9 s.
	wire := decodeWire(t, coldLoadTestPipeline(t, srv.URL).Run(ctx, agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s / %s", wire.DeferClass, wire.Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	var got *core.LiveProgress
	for i := range reports {
		if reports[i].Phase == "repack" {
			got = &reports[i]
		}
	}
	if got == nil {
		t.Fatalf("the job record never showed the re-pack phase: %+v", reports)
	}
	if got.AllowanceMs < 19000 || got.AllowanceMs > 23000 || got.LastProgressMs == 0 {
		t.Fatalf("re-pack report = %+v, want the arithmetic allowance (~20.9 s) and a fresh last-progress stamp", *got)
	}
}
