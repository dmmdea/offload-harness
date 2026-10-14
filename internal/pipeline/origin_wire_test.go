package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register D-101 (ADR 0085): a fleet node's ledger row names the session and run class of the box that asked,
// not the node process's own. Run carries Request.OriginSession and RunClass into the result meta as
// AskerSession and AskerClass and entryFrom maps them onto the row, exactly as the fleet job id and the asker's
// name travel; Record keeps a session its caller set, so the wire value beats the node's own environment, and
// fills the pids of the process that recorded it.

func openRowLedger(t *testing.T, p *Pipeline) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	t.Cleanup(func() { p.led = nil; _ = led.Close() })
	return path
}

func rowOfJob(t *testing.T, path, fleetJobID string) ledger.Entry {
	t.Helper()
	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.FleetJobID == fleetJobID {
			return r
		}
	}
	t.Fatalf("no ledger row for fleet job %s among %d rows", fleetJobID, len(rows))
	return ledger.Entry{}
}

func TestWireOriginReachesTheLedgerRowNotTheNodesOwnSession(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)
	path := openRowLedger(t, p)

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput, Door: "fleet", FleetJobID: "agd-org-1", OriginSession: "sess-abc", RunClass: "bake"})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.AskerSession != "sess-abc" || res.Meta.AskerClass != "bake" {
		t.Fatalf("meta = asker session %q class %q, want sess-abc and bake", res.Meta.AskerSession, res.Meta.AskerClass)
	}
	// The control: a call no asker named keeps the node process's own origin, whatever that is here.
	res = p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput, Door: "fleet", FleetJobID: "agd-org-2"})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}

	row := rowOfJob(t, path, "agd-org-1")
	if row.OriginSession != "sess-abc" || row.RunClass != "bake" {
		t.Fatalf("written row = origin_session %q run_class %q, want the asker's sess-abc and bake", row.OriginSession, row.RunClass)
	}
	if row.OriginPID != os.Getpid() {
		t.Fatalf("written row origin_pid = %d, want this (the recording) process %d", row.OriginPID, os.Getpid())
	}
	ctl := rowOfJob(t, path, "agd-org-2")
	if ctl.OriginSession == "sess-abc" || ctl.RunClass == "bake" {
		t.Fatalf("a call with no wire origin carries the asker's: origin_session %q run_class %q", ctl.OriginSession, ctl.RunClass)
	}
	if ctl.OriginPID != os.Getpid() {
		t.Fatalf("control row origin_pid = %d, want %d", ctl.OriginPID, os.Getpid())
	}
}

// The agent loop is its own row writer and takes the meta by value: the wire origin must reach it too.
func TestWireOriginReachesTheAgentRow(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	path := openRowLedger(t, p)

	req := agentTestRequest(t, testContract())
	req.Door, req.FleetJobID, req.OriginSession, req.RunClass = "fleet", "agd-org-agent", "sess-abc", "bake"
	if wire := decodeWire(t, p.Run(context.Background(), req)); wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	row := rowOfJob(t, path, "agd-org-agent")
	if row.Task != "agent" || row.OriginSession != "sess-abc" || row.RunClass != "bake" {
		t.Fatalf("agent row = task %q origin_session %q run_class %q, want agent, sess-abc and bake", row.Task, row.OriginSession, row.RunClass)
	}
}

// A request nobody asked for publishes a row with no asker columns beyond the ones every row has: the entry the
// pipeline builds from a plain meta names no session of its own (Record stamps the process's later).
func TestEntryFromCarriesTheAskersOriginAndNothingElse(t *testing.T) {
	e := entryFrom(core.TaskSummarize, core.Meta{AskerSession: "sess-abc", AskerClass: "bake"}, false, 10)
	if e.OriginSession != "sess-abc" || e.RunClass != "bake" {
		t.Fatalf("entry = origin_session %q run_class %q, want sess-abc and bake", e.OriginSession, e.RunClass)
	}
	e = entryFrom(core.TaskSummarize, core.Meta{}, false, 10)
	if e.OriginSession != "" || e.RunClass != "" || e.OriginPID != 0 {
		t.Fatalf("a plain meta names a session or class: %+v", e)
	}
}
