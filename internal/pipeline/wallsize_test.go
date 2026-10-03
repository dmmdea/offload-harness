package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// TestSizeRunReadsTheStoreWithoutTouchingPipelineState (register D-102): the
// agent_run door sizes a run from the seat's remembered rate (the store over the
// configured agent_seat_tok_s, exactly as the delegation door's node does), and a
// READ must leave the pipeline's own seatRatesPath alone — that field is what a
// run's record step writes, and the door reads concurrently with those runs.
func TestSizeRunReadsTheStoreWithoutTouchingPipelineState(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), AgentModel: "agent-pool", AgentMaxTokens: 1024, AgentThinking: "off", AgentSeatTokS: 100}
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := seatrate.Path(root)
	if err := seatrate.Update(path, func(s *seatrate.Store) { s.Observe("agent-pool", 5, 200, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	p := New(cfg, nil, nil, nil)
	got := p.SizeRun(core.AgentContract{MaxSteps: 12}, "agent-pool", 20)
	// The remembered 5 tok/s, not the configured 100: one tool step (128 tok + 6 s) and
	// a 64-token final = 45 s, so a 20 s wall is refused and a 45 s wall is not.
	if got.MinViableSec != 45 || !got.Refuses(20) || !got.Refuses(44) || got.Refuses(45) || got.Refuses(0) {
		t.Fatalf("MinViableSec = %d, Refuses(20) = %v, Refuses(44) = %v, Refuses(45) = %v, Refuses(0) = %v; want 45 / true / true / false / false", got.MinViableSec, got.Refuses(20), got.Refuses(44), got.Refuses(45), got.Refuses(0))
	}
	if !strings.Contains(got.Estimate.Note, "5.0 tok/s (store") || !strings.Contains(got.Estimate.Note, "wall 20 s is BELOW the estimate") {
		t.Fatalf("the estimate must be the store's, against the wall given: %q", got.Estimate.Note)
	}
	if p.seatRatesPath != "" {
		t.Fatalf("SizeRun must not set pipeline state, seatRatesPath = %q", p.seatRatesPath)
	}
	// The configured agent seat the store never saw falls back to the configured rate
	// (100 tok/s: floor 8 s)...
	fresh := cfg
	fresh.StateDir = t.TempDir()
	if cfgOnly := New(fresh, nil, nil, nil).SizeRun(core.AgentContract{MaxSteps: 12}, "agent-pool", 20); cfgOnly.MinViableSec != 8 || cfgOnly.Refuses(20) {
		t.Fatalf("configured-rate fallback on the agent seat: MinViableSec = %d refuses(20) = %v, want 8 / false", cfgOnly.MinViableSec, cfgOnly.Refuses(20))
	}
	// ...and with neither there is no floor and nothing is refused.
	none := New(config.Config{StateDir: t.TempDir()}, nil, nil, nil).SizeRun(core.AgentContract{MaxSteps: 12}, "agent-pool", 1)
	if none.MinViableSec != 0 || none.Refuses(1) || !strings.Contains(none.Estimate.Note, "no decode-rate sample") {
		t.Fatalf("no rate must be no opinion: %+v", none)
	}
}

// TestSizeRunNeverAttributesTheAgentSeatsConfiguredRateToAnotherSeat: agent_seat_tok_s
// is the planner seat's rate (config.go), so a run on any OTHER seat — a model the
// caller named, or the seat composite placement picked — has no configured rate and
// may be sized only from its own entry in the seat-rates store. Without one it gets
// no opinion: no floor, no refusal, no published estimate. Sizing it at the agent
// seat's rate refused walls the named model could hold, and named that model in the
// refusal (the review of D-102).
func TestSizeRunNeverAttributesTheAgentSeatsConfiguredRateToAnotherSeat(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), AgentModel: "agent-pool", AgentMaxTokens: 1024, AgentThinking: "off", AgentSeatTokS: 5}
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seatrate.Update(seatrate.Path(root), func(s *seatrate.Store) { s.Observe("measured-seat", 10, 200, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	p := New(cfg, nil, nil, nil)
	contract := core.AgentContract{MaxSteps: 12}

	// The agent seat itself keeps the configured rate: 5 tok/s, floor 45 s.
	if own := p.SizeRun(contract, "agent-pool", 20); own.MinViableSec != 45 || !own.Refuses(20) {
		t.Fatalf("the configured agent seat must be sized at its own rate: MinViableSec = %d refuses(20) = %v, want 45 / true", own.MinViableSec, own.Refuses(20))
	}
	// Another seat with no store entry: no opinion, and the note says why.
	other := p.SizeRun(contract, "some-other-fast-model", 20)
	if other.MinViableSec != 0 || other.Refuses(20) {
		t.Fatalf("a seat that is not the agent seat must not inherit its configured rate: MinViableSec = %d refuses(20) = %v, want 0 / false", other.MinViableSec, other.Refuses(20))
	}
	if other.Estimate.TotalSec != 0 || other.Estimate.MinTurnSec != 0 || !strings.Contains(other.Estimate.Note, "no decode-rate sample") {
		t.Fatalf("no rate of its own must publish no estimate: %+v", other.Estimate)
	}
	// Another seat WITH a store entry is sized from that entry alone: 10 tok/s,
	// one tool step (128 tok + 6 s) and a 64-token final = 6 + 192/10 = 26 s.
	measured := p.SizeRun(contract, "measured-seat", 20)
	if measured.MinViableSec != 26 || !measured.Refuses(20) || measured.Refuses(26) || !strings.Contains(measured.Estimate.Note, "10.0 tok/s (store") {
		t.Fatalf("a seat with its own store entry is sized from it: MinViableSec = %d refuses(20) = %v refuses(26) = %v note %q, want 26 / true / false / the store's 10.0 tok/s", measured.MinViableSec, measured.Refuses(20), measured.Refuses(26), measured.Estimate.Note)
	}
	// The planner chain resolves at call time: with no agent_model the workhorse
	// is the agent seat and carries the configured rate.
	wh := cfg
	wh.AgentModel, wh.Model = "", "workhorse"
	pw := New(wh, nil, nil, nil)
	if got := pw.SizeRun(contract, "workhorse", 20); got.MinViableSec != 45 {
		t.Fatalf("the workhorse is the agent seat when agent_model is unset: MinViableSec = %d, want 45", got.MinViableSec)
	}
	if got := pw.SizeRun(contract, "agent-pool", 20); got.MinViableSec != 0 {
		t.Fatalf("a named seat other than the resolved agent seat gets no configured rate: MinViableSec = %d, want 0", got.MinViableSec)
	}
}
