package pipeline

// C-95: a roster miss on the seat a PLACEMENT chose must say where the seat came
// from. The reported symptom was a config copy repointed at a scratch engine
// (endpoint + agent_model) that kept the node's `layers`: every subtask went to
// the layer's agent seat and deferred 'agent seat "<27B>" is not in the endpoint's
// served roster' - a seat the copy never named, with nothing saying the layers
// had chosen it over agent_model.

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestRosterMissOnAPlacedSeatNamesTheLayerThatChoseIt(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{"copy-seat"}, // the scratch engine serves ONLY the copy's agent_model
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p := compositeTestPipeline(t, srv.URL)
	p.cfg.AgentModel = "copy-seat" // the copy's own planner key
	placed := &core.Placed{Layer: "pair", Role: "agent", Seat: agentTestSeat, Reason: "test"}
	wire, err := p.RunAgentContract(context.Background(), testContract(), AgentContractOptions{Seat: placed.Seat, Placed: placed})
	if err != nil {
		t.Fatal(err)
	}
	if !wire.Deferred || !strings.Contains(wire.Reason, "not in the endpoint's served roster") {
		t.Fatalf("expected the roster-miss defer, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
	for _, need := range []string{agentTestSeat, `layer "pair"`, `agent_model "copy-seat"`} {
		if !strings.Contains(wire.Reason, need) {
			t.Fatalf("the defer must name %q so the operator sees which source supplied the seat, got %q", need, wire.Reason)
		}
	}
}

// A seat that is the box's own agent_model (no placement involved) keeps the
// short, unchanged reason: nothing shadowed it.
func TestRosterMissOnThePlannerSeatKeepsTheShortReason(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{"something-else"},
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, _ := agentContractPipeline(t, srv.URL)
	wire, err := p.RunAgentContract(context.Background(), testContract(), AgentContractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !wire.Deferred || wire.Reason != `agent seat "`+agentTestSeat+`" is not in the endpoint's served roster` {
		t.Fatalf("a planner-seat miss must stay the plain roster defer, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
}
