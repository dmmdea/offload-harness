package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// TestRunAgentTaskStampsTheAutoWallFromTheSeatRate (register D-03): the node
// that runs a timeout_auto contract sizes its wall from the seat-rates store on
// ITS state root and says so on the wire — wall_sec equals the clamped estimate
// and wall_note is marked. An explicit contract stamps nothing: the field is
// absent and the note is the plain estimate, byte-identical to before.
func TestRunAgentTaskStampsTheAutoWallFromTheSeatRate(t *testing.T) {
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

	auto := testContract()
	auto.MaxSteps, auto.TimeoutSec, auto.TimeoutAuto = 12, core.AgentTimeoutSecDefault, true
	store, _ := seatrate.Load(seatrate.Path(state))
	want, _ := AutoWallFor(cfg, auto, agentTestSeat, store.Get(agentTestSeat))
	if want <= core.AgentTimeoutSecDefault {
		t.Fatalf("fixture drifted: the auto wall %d must exceed the default to be observable", want)
	}
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, auto)))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.WallSec != want || !strings.HasPrefix(wire.WallNote, "auto wall (timeout_auto): ") {
		t.Fatalf("auto contract: wall_sec=%d wall_note=%q, want %d and the auto-wall prefix", wire.WallSec, wire.WallNote, want)
	}

	explicit := testContract()
	explicit.TimeoutSec = 120
	wire = decodeWire(t, p.Run(context.Background(), agentTestRequest(t, explicit)))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.WallSec != 0 || strings.Contains(wire.WallNote, "auto wall") {
		t.Fatalf("explicit contract: wall_sec=%d wall_note=%q, want no stamp and the plain estimate note", wire.WallSec, wire.WallNote)
	}
}
