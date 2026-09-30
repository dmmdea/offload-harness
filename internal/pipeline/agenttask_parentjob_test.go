package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register C-62: a run the delegator placed on this box's own seat carries the
// delegator's job id as parent_job_id, and the pipeline's ledger row for it is
// written as an INNER row (no cards_tokens of its own) — the delegator's
// agent_delegate row is the job's one record.
func TestRunAgentTaskStampsTheInnerRowWithItsParentJob(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	for _, tc := range []struct{ parent string }{{"agd-parent-1"}, {""}} {
		req := agentTestRequest(t, testContract())
		if tc.parent != "" {
			req.Params["parent_job_id"] = tc.parent
		}
		wire := decodeWire(t, p.Run(context.Background(), req))
		if wire.Deferred {
			t.Fatalf("deferred: %s", wire.Reason)
		}
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(rows))
	}
	if rows[0].ParentJobID != "agd-parent-1" || rows[0].CardsTokens != 0 {
		t.Fatalf("delegator-placed row: parent=%q cards_tokens=%d, want agd-parent-1 / 0", rows[0].ParentJobID, rows[0].CardsTokens)
	}
	if rows[1].ParentJobID != "" || rows[1].CardsTokens == 0 {
		t.Fatalf("a standalone run must stay a job of its own: parent=%q cards_tokens=%d", rows[1].ParentJobID, rows[1].CardsTokens)
	}
}
