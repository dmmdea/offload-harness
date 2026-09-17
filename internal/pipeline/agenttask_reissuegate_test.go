package pipeline

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// agentTestPipelineWithSeatRate is agentTestPipeline plus a configured
// agent_seat_tok_s — the knob a red test needs to put a REAL number through
// seatrate.MinTurnFor instead of the "no rate yet" 0 floor that fails every
// gate open.
func agentTestPipelineWithSeatRate(t *testing.T, base string, tokS float64) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:      base,
		Model:         "workhorse",
		AgentModel:    agentTestSeat,
		FleetNodeID:   "node-t",
		Temperature:   0.1,
		AgentSeatTokS: tokS,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30_000_000_000), nil, nil)
}

// TestRunAgentTaskListCapReissueSizedFromFittedFinal (S-06/W-07, register
// D-95): the list-cap re-issue used to be gated on
// seatrate.MinTurnFor(0, finalBudget, repackBudget, tokS) — the CONFIGURED
// final (up to 8,192) PLUS a full re-pack term — while the re-issue itself is
// only ONE turn at the FITTED final (startFit.Budget) with nothing to
// re-pack. Below ~9 tok/s the configured-budget gate exceeds the 900 s wall
// cap and can never fire, which was the #1 measured defer fleet-wide (48 rows
// "re-pack skipped: the final answer was cut at the completion budget").
//
// At 5 tok/s the OLD gate needs ceil((4096+4096)/5) = 1,639 s — more than
// double a 600 s wall, so it never fires no matter how much of the wall is
// left. The FIX gates on ceil(fittedFinal/5) instead, which by construction
// fits comfortably inside a 600 s wall that has barely been touched.
func TestRunAgentTaskListCapReissueSizedFromFittedFinal(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n == 1 {
				// The FIRST completion is cut at the budget: content present,
				// no tool call, finish_reason "length" — the shape the list-cap
				// re-issue exists to rescue.
				return lengthChat(`{"answer":"the ledger shows pass 4 measured and the tail was cut here`)
			}
			// The re-issue's own completion: a clean, schema-valid final.
			return doneChat(`{"answer":"42"}`)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 600 // >>> the fitted turn (~a few hundred s at 5 tok/s), <<< the OLD gate's 1,639 s
	p := agentTestPipelineWithSeatRate(t, srv.URL, 5)
	res := p.Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.FinalReissue != agent.FinalReissueListCap {
		t.Fatalf("final_reissue = %q, want %q — the fitted-budget gate must fire with >300s of a 600s wall left at 5 tok/s", wire.FinalReissue, agent.FinalReissueListCap)
	}
	if got := fake.loopCalls.Load(); got != 2 {
		t.Fatalf("loop completions = %d, want exactly 2 — the cut final plus one re-issued final", got)
	}
}

// TestRunAgentTaskListCapReissueRefusesWhenTheFittedTurnDoesNotFit is the
// control: the SAME 5 tok/s seat and schema contract, but a wall short enough
// that even the FITTED final's own turn cannot fit inside what is left. The
// fixed gate must still refuse here — it is a real ceiling, not "always true"
// — and the run falls back to the existing D-91 abstention (no reissue, the
// partial rides in output).
func TestRunAgentTaskListCapReissueRefusesWhenTheFittedTurnDoesNotFit(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			return lengthChat(`{"answer":"the ledger shows pass 4 measured and the tail was cut here`)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 5 // far under even the fitted turn's own cost at 5 tok/s
	p := agentTestPipelineWithSeatRate(t, srv.URL, 5)
	res := p.Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.FinalReissue == agent.FinalReissueListCap {
		t.Fatalf("final_reissue = %q, want no list-cap re-issue — the fitted turn cannot fit a 5s wall", wire.FinalReissue)
	}
	if !wire.Deferred || !wire.OutputTruncated {
		t.Fatalf("deferred=%v output_truncated=%v, want the existing D-91 abstention over a cut partial", wire.Deferred, wire.OutputTruncated)
	}
}
