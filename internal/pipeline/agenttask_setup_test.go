package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// A contract's setup_actions run on the fleet path before the first model
// turn: the model answers on its first Chat (one step), the wire result
// carries setup_ran and a step-0 trace entry with setup:true, and steps count
// the model's turns only.
func TestRunAgentTaskContractSetupActionsSeedTheRun(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	c := testContract()
	c.SetupActions = []core.AgentSetupAction{{Tool: "read_file", Args: json.RawMessage(`{"path":"notes.md"}`)}}
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, c))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.Steps != 1 || wire.SetupRan != 1 {
		t.Fatalf("steps=%d setup_ran=%d, want 1/1 (setup spends no step)", wire.Steps, wire.SetupRan)
	}
	if len(wire.Trace) != 1 || !wire.Trace[0].Setup || wire.Trace[0].Step != 0 || wire.Trace[0].Tool != "read_file" || wire.Trace[0].Status != string(agent.EffectCommitted) || wire.Trace[0].ObsChars == 0 {
		t.Fatalf("trace = %+v, want one step-0 setup read_file that committed", wire.Trace)
	}
	if fake.loopCalls.Load() != 1 {
		t.Fatalf("model turns = %d, want 1", fake.loopCalls.Load())
	}
}

// agent_seed_context_reads on THIS box prepends one read_file per context
// doc — the node's own names — ahead of the contract's actions; off, a
// contract without setup_actions has no replay and no setup_ran.
func TestRunAgentTaskSeedContextReadsIsANodeKey(t *testing.T) {
	run := func(seed bool, extra []core.AgentSetupAction) core.AgentWireResult {
		fake := &agentFake{
			rosterIDs: []string{agentTestSeat},
			loop:      func(int64) string { return doneChat("42") },
			repack:    func(int64) string { return `{"answer":"42"}` },
		}
		srv := fake.server(t)
		defer srv.Close()
		cfg := config.Config{
			Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
			AgentSeedContextReads: seed,
		}
		p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
		c := testContract()
		c.Context = []core.ContextDoc{{Name: "a.md", Text: "alpha"}, {Name: "b.md", Text: "beta"}}
		c.SetupActions = extra
		return decodeWire(t, p.Run(context.Background(), agentTestRequest(t, c)))
	}
	off := run(false, nil)
	if off.Deferred || off.SetupRan != 0 || len(off.Trace) != 0 {
		t.Fatalf("seed off + no contract actions must replay nothing: %+v", off)
	}
	on := run(true, []core.AgentSetupAction{{Tool: "list_dir"}})
	if on.Deferred {
		t.Fatalf("deferred: %s", on.Reason)
	}
	if on.SetupRan != 3 || len(on.Trace) != 3 {
		t.Fatalf("seed on: setup_ran=%d trace=%+v, want the two doc reads then the contract's list_dir", on.SetupRan, on.Trace)
	}
	if on.Trace[0].Tool != "read_file" || on.Trace[1].Tool != "read_file" || on.Trace[2].Tool != "list_dir" || !on.Trace[2].Setup {
		t.Fatalf("order = %+v", on.Trace)
	}
	// The seeded reads read the materialized docs — a non-trivial observation
	// size proves the file the node wrote is what came back.
	if on.Trace[0].Status != string(agent.EffectCommitted) || on.Trace[0].ObsChars < len("alpha") {
		t.Fatalf("seeded read = %+v", on.Trace[0])
	}
}

// The seeded reads and the contract's own actions are ONE list, clamped to
// the documented eight per run — seeded reads first, the contract's tail cut.
func TestSetupActionsForClampsTheCombinedList(t *testing.T) {
	c := testContract()
	c.Context = nil
	for i := 0; i < 6; i++ {
		c.Context = append(c.Context, core.ContextDoc{Name: "d" + string(rune('a'+i)) + ".md", Text: "t"})
	}
	for i := 0; i < 5; i++ {
		c.SetupActions = append(c.SetupActions, core.AgentSetupAction{Tool: "list_dir"})
	}
	got := setupActionsFor(config.Config{AgentSeedContextReads: true}, c)
	if len(got) != core.AgentSetupActionsMax {
		t.Fatalf("combined list = %d, want the cap %d", len(got), core.AgentSetupActionsMax)
	}
	for i := 0; i < 6; i++ {
		if got[i].Tool != "read_file" {
			t.Fatalf("seeded reads must come first: %+v", got)
		}
	}
	if got[6].Tool != "list_dir" || got[7].Tool != "list_dir" {
		t.Fatalf("the contract's actions fill the rest: %+v", got)
	}
	if off := setupActionsFor(config.Config{}, c); len(off) != 5 || off[0].Tool != "list_dir" {
		t.Fatalf("seed off = the contract's own list only: %+v", off)
	}
}

// A bad replay list on the wire defers by the contract class, by name —
// never a silent run without it.
func TestRunAgentTaskInvalidSetupActionsDeferByName(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("never") },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	c := testContract()
	c.SetupActions = []core.AgentSetupAction{{Tool: "read file"}}
	if err := c.Validate(); err == nil {
		t.Fatal("the fixture must be invalid")
	}
	// The fleet door validates at decode: an invalid contract never reaches
	// the pipeline. Prove the door's verdict names the field.
	b, _ := json.Marshal(c)
	if _, err := core.DecodeAgentContract(bytes.NewReader(b)); err == nil || !strings.Contains(err.Error(), "setup_actions") {
		t.Fatalf("decode must reject by name, got %v", err)
	}
}
