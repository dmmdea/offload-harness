package delegate

// 0.184.0 integration review, silent failure hunt: a queue row's dial base is the holder's, which is no node that wrote an
// answer, so no reader of PlacedResult.ranBase may attribute a node's behaviour to it. The rescue origin was one reader and
// takeQueueAnswer's viaQueue mark keeps it away (queue_rescue_origin_test.go). The quarantine strike at the end of the rescue
// is the other: it names a node whose rescued object fails the document fingerprint and reads ranBase. The queue lane passes no
// strike today, so nothing fires, but a strike wired for it later would quarantine the queue holder for a claimant's misses.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func rescuedSchemaMissRow(viaQueue bool) PlacedResult {
	return PlacedResult{
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: "sim-node", Seat: "sim-seat",
			Output:   "The refrigerated shipment is RF-9082 with 7 pallets.",
			Deferred: true, DeferClass: core.DeferClassBudget, SchemaMiss: true, StopReason: "done",
			Reason: "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064",
		},
		ranBase: "http://holder.example:18793", viaQueue: viaQueue,
	}
}

func rescueThatSucceeds(context.Context, core.AgentContract, string, time.Duration) (Rescued, error) {
	return Rescued{Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`), Seat: "local-seat", How: "one re-pack completion", TokensOut: 30}, nil
}

// TestAQueueRowIsNeverStruckForTheHolder: a rescued object whose row came through the queue strikes nobody, even when a strike
// is wired, because the base it would strike is the holder's.
func TestAQueueRowIsNeverStruckForTheHolder(t *testing.T) {
	var struck []string
	got := rescueSchemaMiss(context.Background(), rescueThatSucceeds, queueRescueContract(), rescuedSchemaMissRow(true), time.Now(),
		func(base string, _ core.AgentWireResult, _ []string) { struck = append(struck, base) })
	if got.Result.Deferred {
		t.Fatalf("setup: the rescue did not deliver: %+v", got.Result)
	}
	if len(struck) != 0 {
		t.Fatalf("a queue row's rescue struck %v: that is the holder, not the node that wrote the answer", struck)
	}
}

// TestAPushRowIsStillStruckForItsNode: the other half, so the gate cannot silence the strike the push path relies on.
func TestAPushRowIsStillStruckForItsNode(t *testing.T) {
	var struck []string
	rescueSchemaMiss(context.Background(), rescueThatSucceeds, queueRescueContract(), rescuedSchemaMissRow(false), time.Now(),
		func(base string, _ core.AgentWireResult, _ []string) { struck = append(struck, base) })
	if len(struck) != 1 || struck[0] != "http://holder.example:18793" {
		t.Fatalf("a push row's rescue struck %v, want its node's base once", struck)
	}
}
