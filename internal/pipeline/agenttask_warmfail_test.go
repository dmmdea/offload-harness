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
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// A warm-up that is refused with a server error while the seat never reads ready
// is a seat whose process did not start (register C-76, R-05): llama-swap answers
// 500 "upstream command exited prematurely" for an engine that exits at start.
// Proceeding into the wall spent the whole wall on it (one run filed a 358 s
// EngineCore death after exactly this). With agent_warm_failure_defer it defers at
// once, as the seat's health, before any wall exists, and the loop is never
// started. (What must NOT defer, and the audit mode, are agenttask_warmbusy_test.go's.)
func TestRunAgentTaskWarmUp5xxDefersAtOnce(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the loop ran on a seat whose process did not start")
			return doneChat("x")
		},
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        func(int64) string { return `{"running":[]}` }, // the seat never reads ready
		upstreamStatus: func(int64) int { return http.StatusInternalServerError },
		upstreamBody:   func(int64) string { return "upstream command exited prematurely" },
	}
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("deferred=%v class=%q reason=%q, want an infrastructure defer", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.HasPrefix(wire.Reason, core.SeatWarmFailedReason) || !strings.Contains(wire.Reason, "HTTP 500") || !strings.Contains(wire.Reason, "did not start") {
		t.Fatalf("reason = %q, want the stable prefix, the status and what it means", wire.Reason)
	}
	if !strings.Contains(wire.AdmissionNote, "HTTP 500") {
		t.Fatalf("admission_note = %q, want the refusal reported on the wire", wire.AdmissionNote)
	}
	if fake.loopCalls.Load() != 0 {
		t.Fatalf("loop requests = %d, want 0", fake.loopCalls.Load())
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("the defer took %s: it must land within about one poll interval, not a wall", el)
	}
}

// The guard on the other side: a 5xx from the passthrough while /running reads
// the seat ready is a seat that is up (another request loaded it, or the proxy
// hiccuped). It proceeds, as before.
func TestRunAgentTaskWarmUp5xxWithTheSeatReadyProceeds(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running: func(n int64) string {
			if n <= 2 { // the admission poll and the warm-up's own check: absent
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
		upstreamStatus: func(int64) int { return http.StatusServiceUnavailable },
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || fake.loopCalls.Load() == 0 {
		t.Fatalf("deferred=%v loop requests=%d reason=%q, want a run on a seat that reads ready", wire.Deferred, fake.loopCalls.Load(), wire.Reason)
	}
}

// A warm-up answered with a non-200 that leaves the seat never listed is not a
// load, and must not be reported as one (register C-76). The reference to
// "attempted" is what the coherence probe and the cold-load bookkeeping key on:
// it told them a load had run for this contract.
func TestWarmSeatNon200IsNotAnAttemptedLoad(t *testing.T) {
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, running: func(int64) string { return `{"running":[]}` }} // the passthrough 404s
	srv := fake.server(t)
	defer srv.Close()

	spent, note, attempted := warmSeat(context.Background(), srv.URL, agentTestSeat, 30*time.Second)
	if attempted {
		t.Fatalf("a 404 that loaded nothing was reported as an attempted load (spent %v, note %q)", spent, note)
	}
	if !strings.Contains(note, "HTTP 404") || !strings.Contains(note, "never listed") || !strings.Contains(note, "(proceeding)") {
		t.Fatalf("note = %q, want the status, the missing listing and that the run proceeds", note)
	}
}

// Through the whole run: a 404 warm-up proceeds into the wall (as before) but
// records no cold load against the seat and does not fire the D-118 coherence
// probe, which is asked of a seat that was just loaded for this contract.
func TestRunAgentTaskWarmUp404ProceedsWithoutCountingALoad(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t",
		Temperature: 0.1, AgentAdmissionWaitSec: 30, StateDir: t.TempDir(),
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)

	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("a 404 warm-up must proceed into the wall: %s / %q", wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.AdmissionNote, "HTTP 404") {
		t.Fatalf("admission_note = %q, want the refusal reported", wire.AdmissionNote)
	}
	if got := fake.probeCNT.Load(); got != 0 {
		t.Fatalf("coherence probes = %d, want 0: nothing was loaded for this run", got)
	}
	store, _ := seatrate.Load(p.seatRatesPath)
	if got := store.Get(agentTestSeat).ColdLoadSec; got != 0 {
		t.Fatalf("seat-rates recorded a cold load of %.1fs for a warm-up that loaded nothing", got)
	}
}
