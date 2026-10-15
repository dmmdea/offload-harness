package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The agent door's wire result is what the delegator's standing corpus (the
// delegation log) stores, and it already carried the prefill accounting. It carried nothing of
// the pager gate or the compaction ladder, so the one workload that produces recorded agent runs
// on a fleet node left no trace of how often the ladder ran or whether evicted content came
// back. This pins that the loop's pager report reaches the wire, as an object even for a run
// that never compacted: an absent object means "a node that predates the field".

// intPtrText is a *int for a failure message: the value, or "absent" for nil. A bare pointer
// printed with %v is an address.
func intPtrText(p *int) string {
	if p == nil {
		return "absent"
	}
	return strconv.Itoa(*p)
}

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
		t.Fatalf("compaction_passes = %s, want a present 0 for a run that never compacted: %s", intPtrText(pg.CompactionPasses), raw)
	}
	if pg.Evictions == nil || *pg.Evictions != 0 {
		t.Fatalf("evictions = %s, want a present 0: %s", intPtrText(pg.Evictions), raw)
	}
}

// TestWriteClassDeferralKeepsItsTraceAndItsPagerObject pins that a run the write door refuses
// still carries the loop's pager report: the loop ran, so the report is a measurement, and the
// door failing to close cleanly (a cap breach or a failed snapshot) is no reason to lose it. The
// object is attached with the other fields set before every defer branch, ahead of the door, so
// the refusal returns with it and the run reads as recorded, not unrecorded.
//
// The write tools stop a seat at the file cap, so the refusal is reached the way the door's own
// tests reach it: the tree snapshot sees files nobody asked for. The scripted seat makes one real
// write_file call (so the run has a step trace) while eight other files appear in the write root.
func TestWriteClassDeferralKeepsItsTraceAndItsPagerObject(t *testing.T) {
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
	if wire.Pager == nil {
		t.Fatalf("a write-class deferral ran the loop and must carry its pager object, got none")
	}
	if wire.Pager.Basis != "insufficient_data" || wire.Pager.Evictions != 0 || wire.Pager.CompactionPasses != 0 {
		t.Errorf("a run that evicted nothing and never compacted must carry real zeros, got %+v", wire.Pager)
	}
}

// withServerTimings adds llama.cpp's own `timings` block to a scripted chat completion, so the loop
// counts the step as one the server measured (the prefill accounting reads nothing else).
func withServerTimings(body string) string {
	return strings.TrimSuffix(body, "}") + `,"timings":{"prompt_n":120,"prompt_ms":60,"cache_n":30,"predicted_n":5,"predicted_ms":10}}`
}

// TestWriteClassDeferralKeepsTheRunsStopNoteResponseShapeAndPrefill pins that a run the write door
// refuses still carries what the loop measured, on the wire result and on the ledger row. A run
// whose write set breaches the cap is usually a long one, and the long runs are the ones whose
// prefill accounting, stop evidence and response shape the measurement exists for: dropping them
// on the refusal would leave the corpus with only the cheap runs.
//
// The scripted seat makes one real write_file call while eight other files appear in the write
// root, then answers every later turn (the forced final step and its single re-ask) with a tool
// call, so the loop ends on its step budget with a stop note. Every turn carries server timings.
func TestWriteClassDeferralKeepsTheRunsStopNoteResponseShapeAndPrefill(t *testing.T) {
	c := writeContract(".")
	c.MaxSteps = 2
	req := agentTestRequest(t, c)
	root, _ := req.Params["context_dir"].(string)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n != 1 {
				return withServerTimings(toolChat(n))
			}
			for i := 0; i < core.AgentWriteMaxFiles; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("unasked%d.txt", i)), []byte("x\n"), 0o644); err != nil {
					t.Errorf("planting an unasked file: %v", err)
				}
			}
			return withServerTimings(writeCallChat("w1", "asked.txt", "x\n"))
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req)
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassWrite {
		t.Fatalf("deferred=%v class=%q reason=%q, want a write-class deferral for %d files over the %d-file cap",
			wire.Deferred, wire.DeferClass, wire.Reason, core.AgentWriteMaxFiles+1, core.AgentWriteMaxFiles)
	}
	if !strings.Contains(wire.StopNote, "forced final") {
		t.Errorf("stop_note = %q, want the loop's forced-final evidence on a write-class deferral", wire.StopNote)
	}
	if !strings.Contains(wire.ResponseShape, "completions=") {
		t.Errorf("response_shape = %q, want the seat's observed answer shape on a write-class deferral", wire.ResponseShape)
	}
	if wire.PrefillSteps == 0 || wire.PrefillTokens == 0 || wire.CacheTokens == 0 || wire.PrefillMS == 0 {
		t.Errorf("wire prefill = steps %d, prefill tokens %d, cache tokens %d, prefill ms %v; want the measured accounting of a run whose every step reported timings",
			wire.PrefillSteps, wire.PrefillTokens, wire.CacheTokens, wire.PrefillMS)
	}
	m := res.Meta
	if m.PrefillSteps == 0 || m.PrefillSteps != wire.PrefillSteps || m.PrefillTokens != wire.PrefillTokens || m.CacheTokens != wire.CacheTokens || m.PrefillMS != wire.PrefillMS {
		t.Errorf("ledger meta prefill = steps %d, prefill tokens %d, cache tokens %d, prefill ms %v; want the same non-zero accounting as the wire (steps %d, tokens %d, cache %d, ms %v)",
			m.PrefillSteps, m.PrefillTokens, m.CacheTokens, m.PrefillMS,
			wire.PrefillSteps, wire.PrefillTokens, wire.CacheTokens, wire.PrefillMS)
	}
}
