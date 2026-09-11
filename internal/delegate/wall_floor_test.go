package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Wall sizing on the delegator (0.115.21, register D-03): the retry floor is
// the seat's own min_turn_sec when the first attempt published one, and the
// sizing fields ride the published result.

// sizedFailingLocal is failingLocal with the node's wall sizing on the result:
// a 27B-class seat that measured itself at a 500 s minimum turn.
func sizedFailingLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "agent-pool",
			Output: "no idea", Structured: json.RawMessage(`{"answer":"no idea"}`), StopReason: "done",
			SeatTokS: 30.1, WallEstimateSec: 596, MinTurnSec: 500,
			WallNote: "wall 900 s vs estimate 596 s for agent-pool: cold load 210 s + …; min_turn 500 s"}, nil
	}
}

// TestRunRetryFloorUsesTheFirstAttemptsMinTurn: the box floor is the 10 s
// default, the seat says 500 s, the contract has 400 s — the retry is skipped
// and the note names the seat's number as the floor.
func TestRunRetryFloorUsesTheFirstAttemptsMinTurn(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	cfg := testCfg(t)
	cfg.AgentRetryMinSec = 0
	var localCalls atomic.Int64
	c := contracts(1)
	c[0].TimeoutSec = 400
	results, sum, err := Run(context.Background(), cfg, sizedFailingLocal(&localCalls), c, "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("retry must be skipped under the seat's min_turn: dispatches=%d retried=%d", node.dispatches.Load(), sum.Retried)
	}
	if !strings.Contains(pr.RetryNote, "floor 500s, min_turn_sec") {
		t.Fatalf("retry_note = %q, want the seat's min_turn_sec named as the floor", pr.RetryNote)
	}
	// The configured floor still wins when it is the larger number.
	r := &runner{cfg: cfg}
	r.cfg.AgentRetryMinSec = 600
	if floor, src := r.retryFloorFor(pr); floor != 600 || !strings.Contains(src, "agent_retry_min_sec") {
		t.Fatalf("floor = %d (%s), want the larger configured 600", floor, src)
	}
	// And a first attempt without sizing keeps the configured floor.
	r.cfg.AgentRetryMinSec = 300
	if floor, _ := r.retryFloorFor(PlacedResult{}); floor != 300 {
		t.Fatalf("floor without sizing = %d, want 300", floor)
	}
	// The sizing rides the published result.
	rw := WireResponse(results, sum, nil).Results[0]
	if rw.SeatTokS != 30.1 || rw.WallEstimateSec != 596 || rw.MinTurnSec != 500 || !strings.Contains(rw.WallNote, "min_turn 500 s") {
		t.Fatalf("published sizing = tok_s %.1f estimate %d min_turn %d note %q", rw.SeatTokS, rw.WallEstimateSec, rw.MinTurnSec, rw.WallNote)
	}
}
