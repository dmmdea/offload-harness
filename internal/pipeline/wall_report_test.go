package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// TestRunAgentTaskReportsTheWallItRunsUnder (register D-116): the wall a
// contract executes under is published to whoever wired a reporter onto the
// context — the fleet node, which writes it onto the job record so a poll of
// the RUNNING job carries it. Without this the number reached the delegator
// only on the final result, which is exactly the message a node that dies
// mid-run never sends, and the delegator had to hold its poll clock at the
// wire cap for every timeout_auto contract.
//
// The reported number must be the wall the run is ACTUALLY under: the node's
// own sized wall for an auto contract, the caller's timeout_sec for an
// explicit one.
func TestRunAgentTaskReportsTheWallItRunsUnder(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	state := t.TempDir()
	if err := seatrate.Update(seatrate.Path(state), func(s *seatrate.Store) { s.Observe(agentTestSeat, 10, 0, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: 30, StateDir: state, AgentMaxTokens: 1024, AgentThinking: "off",
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)

	var reported atomic.Int64
	ctx := core.WithWallReport(context.Background(), func(sec int) { reported.Store(int64(sec)) })

	auto := testContract()
	auto.MaxSteps, auto.TimeoutSec, auto.TimeoutAuto = 12, core.AgentTimeoutSecDefault, true
	store, _ := seatrate.Load(seatrate.Path(state))
	want, _ := AutoWallFor(cfg, auto, agentTestSeat, store.Get(agentTestSeat))
	if want <= core.AgentTimeoutSecDefault {
		t.Fatalf("fixture drifted: the auto wall %d must exceed the default to be observable", want)
	}
	wire := decodeWire(t, p.Run(ctx, agentTestRequest(t, auto)))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if got := int(reported.Load()); got != want {
		t.Fatalf("reported wall = %d, want the sized wall %d (wire.WallSec %d)", got, want, wire.WallSec)
	}

	reported.Store(0)
	explicit := testContract()
	explicit.TimeoutSec = 120
	if wire = decodeWire(t, p.Run(ctx, agentTestRequest(t, explicit))); wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if got := int(reported.Load()); got != 120 {
		t.Fatalf("reported wall = %d, want the caller's own timeout_sec 120", got)
	}

	// No reporter on the context: the previous behaviour, exactly — no panic,
	// nothing published.
	if wire = decodeWire(t, p.Run(context.Background(), agentTestRequest(t, explicit))); wire.Deferred {
		t.Fatalf("deferred without a reporter: %s", wire.Reason)
	}
}

// TestNoWallIsReportedUntilTheWallStarts (register D-116, review finding 1):
// `wall_sec` means "the wall has STARTED", never "a wall was sized". The node
// claims a job `running` before it runs it, and everything above the wall
// context — the cordon wait, the llama-swap pre-flight, the cold-load warm-up,
// the coherence probe, up to core.AgentAdmissionSecDefault — happens in that
// state. Published at the sizing, the number told a delegator that a 300 s
// wall was already burning while the seat still had 250 s of loading to do,
// and the delegator's poll clock is anchored on it.
//
// So a run that defers during ADMISSION must report nothing at all: no wall
// ever ran.
func TestNoWallIsReportedUntilTheWallStarts(t *testing.T) {
	// The seat cold-loads and the post-warm coherence probe catches it
	// answering the NaN shape: an infrastructure defer BEFORE the wall exists.
	fake := coldLoadFake(func(int64) string { return degenerateChat() })
	srv := fake.server(t)
	defer srv.Close()

	var reported atomic.Int64
	ctx := core.WithWallReport(context.Background(), func(sec int) { reported.Store(int64(sec)) })

	auto := testContract()
	auto.MaxSteps, auto.TimeoutSec, auto.TimeoutAuto = 12, core.AgentTimeoutSecDefault, true
	wire := decodeWire(t, coherenceTestPipeline(t, srv.URL, 30, "").Run(ctx, agentTestRequest(t, auto)))
	if !wire.Deferred {
		t.Fatalf("the fixture must defer during admission, got a result: %q", wire.Output)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer class = %q, want the admission-time infrastructure defer (%s)", wire.DeferClass, wire.Reason)
	}
	if got := reported.Load(); got != 0 {
		t.Fatalf("a run that never reached its wall published a wall of %d s: the delegator would anchor its poll clock on a wall that never started", got)
	}
}
