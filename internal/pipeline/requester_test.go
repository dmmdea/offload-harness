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

// D11: a fleet node's ledger row says who the job was for. Run carries Request.Requester into the
// result meta and entryFrom maps it onto the row, exactly as the fleet job id travels (fleetjobid_test),
// and a call nobody asked for publishes a row with no such column.
func TestRunCarriesTheRequesterIntoMetaAndTheLedgerRow(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput, Door: "fleet", FleetJobID: "agd-1", Requester: "node-q"})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.Requester != "node-q" {
		t.Fatalf("meta.Requester = %q, want node-q", res.Meta.Requester)
	}
	row := entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput))
	if row.Requester != "node-q" {
		t.Fatalf("ledger row requester = %q, want node-q", row.Requester)
	}
	b, err := json.Marshal(row)
	if err != nil || !strings.Contains(string(b), `"requester":"node-q"`) {
		t.Fatalf("serialized row lacks the requester column: %s (%v)", b, err)
	}

	res = p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	b, _ = json.Marshal(entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput)))
	if strings.Contains(string(b), "requester") {
		t.Fatalf("a row nobody asked for must omit the column: %s", b)
	}
}

// The pipeline is the core.RemoteAttributor the remote lanes receive: its handle writes the asker
// row into ITS ledger, and a pipeline built without an emitter still writes the row (no card).
func TestPipelineBeginRemoteWritesTheAskerRowIntoItsLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	p := New(config.Default(), nil, nil, led)

	var a core.RemoteAttributor = p
	h := a.BeginRemote(core.Request{Task: core.TaskClassify, Door: "offload_classify", Input: "x"}, "remote")
	h.Dispatched("http://node-b:18811", "node-b-fleet16", "text-1")
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b-fleet16", Model: "m", Placement: "remote: forced"}})

	rows, err := ledger.ReadAll(path)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (%v), want one", rows, err)
	}
	r := rows[0]
	if r.Task != "classify" || r.Node != "node-b" || r.NodeID != "node-b-fleet16" || r.FleetJobID != "text-1" || !r.CardByCaller || r.Route != "remote" {
		t.Fatalf("row = %+v", r)
	}

	// A nil pipeline degrades to no attribution rather than a panic.
	var nilP *Pipeline
	if h := core.BeginRemote(nilP, core.Request{}, "remote"); h == nil {
		t.Fatal("BeginRemote must always return a handle")
	}
}
