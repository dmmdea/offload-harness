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

// TestRunAgentTaskPublishesTheRunStartFinalFit pins what the delegation door's node
// publishes as the run-start final-budget fit (register D-95), on the case the agent_run
// door's D-134 fix is measured against: ten steps under a 420 s wall on a seat measured at
// 17.2 tok/s with a 330 s cold load and an 8,192-token final (a 2,048-token step budget,
// thinking off), which the seat's own numbers size at 928 s with a min_turn_sec of 807 s.
// The wall cannot hold the run's other terms (the cold load, the tool steps) and a final,
// so the fit is the 1,024-token floor, and the note says the wall will be the stop. A
// contract with an output_schema splits the same wall with the structured re-pack.
//
// Until the agent_run door shared this arithmetic nothing tested the node's publication at
// all (the loop-level fit is covered in internal/agent, the arithmetic in internal/seatrate),
// so the door and the node could drift unseen; internal/mcpserver's
// TestAgentRunSizesTheRunStartFinalLikeTheNodeDoes compares the two.
func TestRunAgentTaskPublishesTheRunStartFinalFit(t *testing.T) {
	const (
		tokS  = 17.2
		cold  = 330
		steps = 10
		wall  = 420
	)
	cases := []struct {
		name   string
		schema bool
		want   string
	}{
		{"no schema", false, "final 8192 → floor 1024: 420 s of wall at 17.2 tok/s buys only 0 tok — the wall will be the stop"},
		{"with an output_schema", true, "final 8192 → floor 1024: 420 s of wall at 17.2 tok/s buys only 0 tok (split with the output_schema re-pack) — the wall will be the stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				loop:      func(int64) string { return doneChat("The answer is 42.") },
				repack:    func(int64) string { return `{"answer":"42"}` },
				running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
			}
			srv := fake.server(t)
			defer srv.Close()
			state := t.TempDir()
			if err := seatrate.Update(seatrate.Path(state), func(s *seatrate.Store) { s.Observe(agentTestSeat, tokS, cold, time.Now()) }); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{
				Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
				AgentAdmissionWaitSec: 30, StateDir: state, AgentMaxTokens: 2048, AgentThinking: "off",
			}
			p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
			contract := core.AgentContract{
				SchemaVersion: core.AgentWireSchemaVersion,
				Goal:          "answer the question",
				Context:       []core.ContextDoc{{Name: "notes.md", Text: "the answer is 42"}},
				MaxSteps:      steps,
				TimeoutSec:    wall,
				Depth:         1,
			}
			if tc.schema {
				contract.OutputSchema = testContract().OutputSchema
			}

			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
			if wire.Deferred {
				t.Fatalf("the run must answer, got a deferral: %s", wire.Reason)
			}
			if wire.WallEstimateSec <= wall || wire.MinTurnSec <= wall {
				t.Fatalf("test premise: the wall %d s must be under the estimate (%d s) and min_turn_sec (%d s)", wall, wire.WallEstimateSec, wire.MinTurnSec)
			}
			if wire.FinalBudgetFit != seatrate.FinalBudgetFloor || wire.BudgetNote != tc.want {
				t.Fatalf("run-start fit = %d %q, want the floor %d and\n  %q", wire.FinalBudgetFit, wire.BudgetNote, seatrate.FinalBudgetFloor, tc.want)
			}
			if !strings.Contains(wire.WallNote, "wall 420 s is BELOW the estimate") {
				t.Errorf("wall_note = %q, want the below-estimate arithmetic the fit is sized from", wire.WallNote)
			}
		})
	}
}
