package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The agent door's wire result is what the delegator's standing corpus (the
// delegation log) stores, and it already carried the prefill accounting. It carried nothing of
// the pager gate or the compaction ladder, so the one workload that produces recorded agent runs
// on a fleet node left no trace of how often the ladder ran or whether evicted content came
// back. This pins that the loop's pager report reaches the wire, as an object even for a run
// that never compacted: an absent object means "a node that predates the field".

// TestAgentWireResultCarriesThePagerReport runs the real agent door over a scripted seat and
// reads the wire result as raw JSON, the form the delegation log stores.
func TestAgentWireResultCarriesThePagerReport(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	if !res.OK {
		t.Fatalf("res.OK = false (%q)", res.Reason)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &wire); err != nil {
		t.Fatalf("wire result is not JSON: %v", err)
	}
	raw, ok := wire["pager"]
	if !ok {
		t.Fatalf("wire result has no pager object; the loop's report was dropped at the door")
	}
	var pg struct {
		Basis            string `json:"basis"`
		Evictions        *int   `json:"evictions"`
		CompactionPasses *int   `json:"compaction_passes"`
	}
	if err := json.Unmarshal(raw, &pg); err != nil {
		t.Fatalf("pager object: %v: %s", err, raw)
	}
	if pg.Basis != "insufficient_data" {
		t.Fatalf("pager basis = %q for a run that evicted nothing, want insufficient_data", pg.Basis)
	}
	if pg.CompactionPasses == nil || *pg.CompactionPasses != 0 {
		t.Fatalf("compaction_passes = %v, want a present 0 for a run that never compacted: %s", pg.CompactionPasses, raw)
	}
	if pg.Evictions == nil || *pg.Evictions != 0 {
		t.Fatalf("evictions = %v, want a present 0: %s", pg.Evictions, raw)
	}
}

// TestWriteClassDeferralKeepsItsTraceAndCarriesNoPagerObject pins the one run that reached the
// loop and still has no pager object: the write door refusing the run's write set returns before
// the wire result is given the loop's pager report. ADR 0091 and the agent wire table state this
// per row class (a write-class deferral is a run with a step trace that reads unrecorded). A
// change that attaches the object ahead of the write door must flip this assertion and those
// statements together.
//
// The write tools stop a seat at the file cap, so the refusal is reached the way the door's own
// tests reach it: the tree snapshot sees files nobody asked for. The scripted seat makes one real
// write_file call (so the run has a step trace) while eight other files appear in the write root.
func TestWriteClassDeferralKeepsItsTraceAndCarriesNoPagerObject(t *testing.T) {
	c := writeContract(".")
	req := agentTestRequest(t, c)
	root, _ := req.Params["context_dir"].(string)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n != 1 {
				return doneChat("wrote one file")
			}
			for i := 0; i < core.AgentWriteMaxFiles; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("unasked%d.txt", i)), []byte("x\n"), 0o644); err != nil {
					t.Errorf("planting an unasked file: %v", err)
				}
			}
			return writeCallChat("w1", "asked.txt", "x\n")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))
	if !wire.Deferred || wire.DeferClass != core.DeferClassWrite {
		t.Fatalf("deferred=%v class=%q reason=%q, want a write-class deferral for %d files over the %d-file cap",
			wire.Deferred, wire.DeferClass, wire.Reason, core.AgentWriteMaxFiles+1, core.AgentWriteMaxFiles)
	}
	if len(wire.Trace) == 0 || wire.Steps == 0 {
		t.Errorf("a write-class deferral ran the loop: want a step trace and steps, got %d trace entries and %d steps", len(wire.Trace), wire.Steps)
	}
	if wire.Pager != nil {
		t.Errorf("a write-class deferral returns before the pager object is attached, got %+v", wire.Pager)
	}
}
