package pipeline

import (
	"context"
	"encoding/json"
	"testing"
)

// RSI-OH-2: the agent door's wire result is what the delegator's standing corpus (the
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
