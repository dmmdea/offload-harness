package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
)

// TestRunQueueRouteEndToEnd drives route:"queue" against a REAL mounted holder
// with a simulated claiming node: submit lands on the holder, the "node"
// claims and acks a wire result, and the delegator's poll returns it with
// acceptance evaluated — the full Option B path minus only the fleet server.
func TestRunQueueRouteEndToEnd(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The simulated node: claim until a job appears, then ack a finished wire.
	done := make(chan string, 1)
	go func() {
		for i := 0; i < 100; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				wire, _ := json.Marshal(core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion,
					NodeID:        "sim-node", Seat: "sim-seat",
					Output:     "The refrigerated shipment is RF-9082 with 7 pallets.",
					Structured: json.RawMessage(`{"shipment_id":"RF-9082"}`),
				})
				_ = q.Ack(job.ID, "sim-node", wire, "")
				done <- job.ID
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	cfg := config.Config{FleetQueueHolder: srv.URL, StateDir: t.TempDir()}
	contract := core.AgentContract{
		Goal:         "which shipment is refrigerated?",
		OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`),
		Acceptance:   []string{"contains:RF-9082"},
		TimeoutSec:   30,
	}
	results, sum, rerr := Run(context.Background(), cfg, nil, []core.AgentContract{contract}, "queue", nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the simulated node never claimed")
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v; results[0] = %+v", sum, results[0])
	}
	pr := results[0]
	if pr.Node != "sim-node" || pr.Seat != "sim-seat" {
		t.Fatalf("provenance = %s/%s", pr.Node, pr.Seat)
	}
	if len(pr.AcceptanceFailures) != 0 {
		t.Fatalf("acceptance must pass: %v", pr.AcceptanceFailures)
	}
	if !strings.Contains(pr.PlacementReason, "route=queue") {
		t.Fatalf("placement = %q", pr.PlacementReason)
	}
}

// TestRunQueueRouteGuards: the route refuses without a holder, and refuses
// schema-less contracts before anything is submitted.
func TestRunQueueRouteGuards(t *testing.T) {
	_, _, err := Run(context.Background(), config.Config{}, nil,
		[]core.AgentContract{{Goal: "g", OutputSchema: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`)}}, "queue", nil)
	if err == nil || !strings.Contains(err.Error(), "fleet_queue_holder") {
		t.Fatalf("holderless queue route must refuse, got %v", err)
	}
	_, _, err = Run(context.Background(), config.Config{FleetQueueHolder: "http://127.0.0.1:1"}, nil,
		[]core.AgentContract{{Goal: "g"}}, "queue", nil)
	if err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("schema-less contract must refuse, got %v", err)
	}
}
