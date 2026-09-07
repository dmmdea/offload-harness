package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// The wire result carries the step trace (ADR 0036): one entry per tool call
// with the tool, its fate, and how much the model read — the per-step facts
// the delegation-log corpus lacked.
func TestRunAgentTaskWireCarriesStepTrace(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n == 1 {
				return toolChat(1) // list_dir on the context dir
			}
			return doneChat("The answer is 42.")
		},
		repack: func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if len(wire.Trace) != 1 {
		t.Fatalf("trace = %+v, want one list_dir step", wire.Trace)
	}
	st := wire.Trace[0]
	if st.Step != 1 || st.Tool != "list_dir" || st.Status != string(agent.EffectCommitted) || st.ObsChars == 0 || st.Rule != "" {
		t.Fatalf("trace[0] = %+v", st)
	}
	if wire.RulesFired != 0 {
		t.Fatalf("rules_fired = %d with no table", wire.RulesFired)
	}
}

// A box whose agent_env_rules table is invalid defers by CONFIG class, naming
// the rule — the contract and the seat are fine; the box's config is not.
func TestRunAgentTaskInvalidEnvRulesDefersByConfigClass(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("never reached") },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		AgentEnvRules: &core.AgentEnvRules{ObservationStrip: []string{"(unclosed"}},
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig || !strings.Contains(wire.Reason, "observation_strip[0]") {
		t.Fatalf("want a config-class defer naming the rule, got %+v", wire)
	}
}

// A valid table applies on the fleet path: the cap blocks the second list_dir
// and the trace + rules_fired say so.
func TestRunAgentTaskEnvRulesApplyOnTheFleetPath(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n <= 2 {
				return toolChat(n)
			}
			return doneChat("The answer is 42.")
		},
		repack: func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		AgentEnvRules: &core.AgentEnvRules{MaxCallsPerTool: map[string]int{"list_dir": 1}},
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	// toolChat(1) and toolChat(2) call list_dir with identical args: the second
	// would be the loop's own exact-repeat refusal — but the env cap (1) runs
	// FIRST and blocks it by rule, which is what the trace must attribute.
	if len(wire.Trace) != 2 || wire.Trace[1].Rule != "max_calls_per_tool" || wire.Trace[1].Status != string(agent.EffectNone) || wire.RulesFired != 1 {
		t.Fatalf("trace = %+v rules_fired = %d", wire.Trace, wire.RulesFired)
	}
}

func TestTraceFromEffectsNilInNilOut(t *testing.T) {
	if TraceFromEffects(nil) != nil {
		t.Fatal("no effects must project to no trace")
	}
	got := TraceFromEffects([]agent.EffectRecord{{Step: 2, Tool: "x", Status: agent.EffectFailed, ObsChars: 7, Rule: "rewrite_error"}})
	if len(got) != 1 || got[0] != (core.AgentTraceStep{Step: 2, Tool: "x", Status: "failed", ObsChars: 7, Rule: "rewrite_error"}) {
		t.Fatalf("got %+v", got)
	}
}
