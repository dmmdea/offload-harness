package delegate

// 0.184.0 integration review, finding 1: the queue lane's rescue is handed no origin.
//
// takeQueueAnswer sets ranBase to the HOLDER, so that ran_on reads "node" for a row a claiming node answered (D-32).
// The rescue (rescue.go) hands the rescue origin over whenever ranBase is set, and the queue lane calls it for every
// schema miss, so the rescue dialled the holder's address with the CLAIMANT's seat name and node id. The holder is a
// queue, not the node that wrote the answer: it has no chat door for that seat, the call fails (or answers for another
// seat), and the rescue's note names a node that did not write the answer. The queue lane has no dial base of a node
// at all, so it hands the rescue no origin and the rescue goes to the delegator's own seat, as before the origin
// existed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
)

// queueSchemaMissWire is the wire a claiming node acks for a finished answer whose structured re-pack failed, with the
// defer class the node filed it under. The classes that hand the origin on a direct route are every one but infrastructure
// (a seat that just wedged is not the first place to send the rescue, originSeatFailed), so those are the ones that expose
// the queue lane: an infrastructure class never handed an origin on any route.
func queueSchemaMissWire(t *testing.T, class string) json.RawMessage {
	t.Helper()
	wire, err := json.Marshal(core.AgentWireResult{
		SchemaVersion: core.AgentWireSchemaVersion,
		NodeID:        "sim-node", Seat: "sim-seat",
		Output:   "The refrigerated shipment is RF-9082 with 7 pallets.",
		Deferred: true, DeferClass: class, SchemaMiss: true,
		StopReason: "done", Reason: "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064",
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// queueHolderWithClaimant mounts a real holder and starts a simulated claiming node that acks wire for the first job.
func queueHolderWithClaimant(t *testing.T, wire json.RawMessage) string {
	t.Helper()
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	go func() {
		for i := 0; i < 100; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				_ = q.Ack(job.ID, "sim-node", wire, "")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return srv.URL
}

func queueRescueContract() core.AgentContract {
	return core.AgentContract{
		Goal:         "which shipment is refrigerated?",
		OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`),
		Acceptance:   []string{"contains:RF-9082"},
		TimeoutSec:   30,
	}
}

// TestQueueLaneRescueIsHandedNoOrigin: a schema miss a claimant filed through the holder is rescued with no origin in
// the context (the holder is not the node that wrote the answer), and the row still says ran_on "node" (a claiming
// node ran it), so the fix does not undo D-32.
func TestQueueLaneRescueIsHandedNoOrigin(t *testing.T) {
	for _, class := range []string{core.DeferClassBudget, core.DeferClassAbstention, ""} {
		t.Run("class="+class, func(t *testing.T) { queueRescueIsHandedNoOrigin(t, class) })
	}
}

func queueRescueIsHandedNoOrigin(t *testing.T, class string) {
	t.Helper()
	holder := queueHolderWithClaimant(t, queueSchemaMissWire(t, class))
	var seen atomic.Value // RescueOrigin
	var handed atomic.Bool
	var calls atomic.Int64
	rescue := func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		calls.Add(1)
		o, ok := RescueOriginFrom(ctx)
		handed.Store(ok)
		seen.Store(o)
		return Rescued{Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`), Seat: "local-seat", How: "one re-pack completion", TokensOut: 30}, nil
	}
	cfg := config.Config{FleetQueueHolder: holder, StateDir: t.TempDir()}
	results, sum, rerr := RunWith(context.Background(), cfg, nil, []core.AgentContract{queueRescueContract()}, "queue", nil, &RunOptions{Rescue: rescue})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if calls.Load() != 1 || sum.Succeeded != 1 || results[0].Result.Deferred {
		t.Fatalf("setup: rescue calls = %d, summary %+v, deferred %v", calls.Load(), sum, results[0].Result.Deferred)
	}
	if handed.Load() {
		o, _ := seen.Load().(RescueOrigin)
		t.Fatalf("the queue lane's rescue was handed the origin %+v: the holder %s is a queue, not the node that wrote the answer, "+
			"and the seat %q is the claimant's", o, holder, o.Seat)
	}
	if v, present := ranOnOf(t, results[0]); !present || v != RanOnNode {
		t.Fatalf("ran_on = %q (present %v), want %q: a claiming node answered", v, present, RanOnNode)
	}
	if note := results[0].Result.RepackNote; !strings.Contains(note, "rescued on local-seat") {
		t.Fatalf("repack_note = %q, want the rescue on the delegator's seat", note)
	}
}

// TestQueueLaneRescueThatFailsAlsoGetsNoOrigin: the defer stands either way, and the failed rescue was not handed the
// holder.
func TestQueueLaneRescueThatFailsAlsoGetsNoOrigin(t *testing.T) {
	holder := queueHolderWithClaimant(t, queueSchemaMissWire(t, core.DeferClassBudget))
	var handed atomic.Bool
	rescue := func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		_, ok := RescueOriginFrom(ctx)
		handed.Store(ok)
		return Rescued{}, context.DeadlineExceeded
	}
	cfg := config.Config{FleetQueueHolder: holder, StateDir: t.TempDir()}
	results, sum, rerr := RunWith(context.Background(), cfg, nil, []core.AgentContract{queueRescueContract()}, "queue", nil, &RunOptions{Rescue: rescue})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if sum.Deferred != 1 || !results[0].Result.Deferred {
		t.Fatalf("setup: a failed rescue leaves the defer: summary %+v", sum)
	}
	if handed.Load() {
		t.Fatal("a queue-lane rescue that went on to fail was still handed an origin")
	}
}
