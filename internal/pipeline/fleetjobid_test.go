// ADR 0064 (register C-63): a fleet node's ledger row names the fleet job id, so
// it joins to the delegator's row for the same run on one equality. Run carries
// Request.FleetJobID into the result meta and entryFrom maps it onto the row —
// the same two-step path the door takes (door_test.go) — and a call no node
// dispatched must publish a row byte-identical to the one it always did.
package pipeline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func TestRunCarriesTheFleetJobIDIntoMetaAndTheLedgerRow(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)

	res := p.Run(context.Background(), core.Request{
		Task:       core.TaskSummarize,
		Input:      summaryInput,
		Door:       "fleet",
		FleetJobID: "agd-0123456789abcdef01234567",
	})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.FleetJobID != "agd-0123456789abcdef01234567" {
		t.Fatalf("meta.FleetJobID = %q, want the dispatched job id", res.Meta.FleetJobID)
	}
	row := entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput))
	if row.FleetJobID != "agd-0123456789abcdef01234567" {
		t.Fatalf("ledger row fleet_job_id = %q, want the dispatched job id", row.FleetJobID)
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if !strings.Contains(string(b), `"fleet_job_id":"agd-0123456789abcdef01234567"`) {
		t.Fatalf("serialized row lacks the fleet_job_id column: %s", b)
	}
}

// A call no fleet node dispatched carries no fleet job id, and its row must not
// grow a key: a reader one release behind decodes it unchanged.
func TestRunWithoutAFleetJobIDOmitsTheColumn(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.FleetJobID != "" {
		t.Fatalf("meta.FleetJobID = %q, want empty for a call no node dispatched", res.Meta.FleetJobID)
	}
	b, err := json.Marshal(entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput)))
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if strings.Contains(string(b), "fleet_job_id") {
		t.Fatalf("a row with no fleet job id must omit the column: %s", b)
	}
}

// The request stays wire-compatible in both directions: an id-less request
// serializes without the key, and one carrying it decodes.
func TestRequestFleetJobIDIsAdditiveOnTheWire(t *testing.T) {
	b, err := json.Marshal(core.Request{Task: core.TaskSummarize, Input: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(b), "fleet_job_id") {
		t.Fatalf("id-less request must omit the key: %s", b)
	}
	var back core.Request
	if err := json.Unmarshal([]byte(`{"task":"summarize","input":"x","fleet_job_id":"agd-1"}`), &back); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if back.FleetJobID != "agd-1" {
		t.Fatalf("decoded fleet_job_id = %q, want agd-1", back.FleetJobID)
	}
}

// TestNodeAgentRowCarriesTheFleetJobID is the case the orphan join needs: the
// AGENT row a node writes for a dispatched contract names the id the delegator
// dispatched it under. Its own job_id stays the node-local one — the two ids are
// different facts and the row keeps both.
func TestNodeAgentRowCarriesTheFleetJobID(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, home := agentContractPipeline(t, srv.URL)
	ledgerPath := filepath.Join(home, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	contract := testContract()
	req := agentTestRequest(t, contract)
	req.Door = "fleet"                              // stamped by the node's dispatch closure
	req.FleetJobID = "agd-abcdef0123456789abcdef01" // and so is this
	res := p.Run(context.Background(), req)
	if wire := decodeWire(t, res); wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	_ = led.Close()

	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var agentRows []ledger.Entry
	for _, e := range rows {
		if e.Task == string(core.TaskAgentRun) {
			agentRows = append(agentRows, e)
		}
	}
	if len(agentRows) != 1 {
		t.Fatalf("agent rows = %d (%+v), want 1", len(agentRows), rows)
	}
	if e := agentRows[0]; e.FleetJobID != "agd-abcdef0123456789abcdef01" || e.JobID != "agent-test" || e.Door != "fleet" {
		t.Fatalf("agent row = fleet_job_id %q job_id %q door %q, want the dispatched id, the node-local id agent-test, and door fleet", e.FleetJobID, e.JobID, e.Door)
	}
}
