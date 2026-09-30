package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register C-62 in production shape: the delegator hands its job id to
// LocalRunner (= p.RunAgentContract), which must carry it as
// params["parent_job_id"] into runAgentTask so the pipeline's ledger row is an
// INNER row. The delegate test stubs the runner and the pipeline test writes
// params by hand, so this link was in neither. Mutant: deleting the
// `params["parent_job_id"] = opts.ParentJobID` block in RunAgentContract keeps
// the full pipeline suite green and re-introduces the double count.
func TestRunAgentContractCarriesTheParentJobToTheInnerRow(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p, _ := agentContractPipeline(t, srv.URL)
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()
	contract := testContract()
	contract.Depth = 0
	if _, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{ParentJobID: "agd-42"}); err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].ParentJobID != "agd-42" || rows[0].CardsTokens != 0 {
		t.Fatalf("inner row: parent=%q cards_tokens=%d, want agd-42 / 0", rows[0].ParentJobID, rows[0].CardsTokens)
	}
}

// The first byte ends a busy hold in the job record a delegator polls, like it
// ends a prefill and a cold-load hold. Mutant: dropping PhaseQueued from the
// OnProgress heal in liveness.go (added at e894fcfd with no test).
func TestProgressObserverLeavesQueuedOnTheFirstByte(t *testing.T) {
	o := newProgressObserver(context.Background(), nil, 300)
	o.OnAllowance("queued", 5*time.Minute)
	o.OnProgress(10)
	o.mu.Lock()
	phase := o.p.Phase
	o.mu.Unlock()
	if phase == "queued" {
		t.Fatalf("job-record phase after a streamed byte = %q", phase)
	}
}
