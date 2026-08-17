// Task 6 (multi-node agent delegation): RunAgentContract — the delegator-side
// LOCAL placement entry. It must take byte-for-byte the fleet node's route
// (Pipeline.Run → runAgentTask) over a self-materialized context dir, and
// clean that dir up whatever the outcome. Reuses agenttask_test.go's scripted
// llama fake.

package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// agentContractPipeline mirrors agentTestPipeline but roots BaseDir in a temp
// dir: RunAgentContract materializes under BaseDir()/pipeline-jobs, and a test
// must never write into the real install root.
func agentContractPipeline(t *testing.T, base string) (*Pipeline, string) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:        home,
		Endpoint:    base,
		Model:       "workhorse",
		AgentModel:  agentTestSeat,
		FleetNodeID: "node-t",
		Temperature: 0.1,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil), home
}

func TestRunAgentContractLocalHappyPath(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, home := agentContractPipeline(t, srv.URL)
	contract := testContract()
	contract.Depth = 0 // delegator-side origin, unlike the wire fixture
	wire, err := p.RunAgentContract(context.Background(), contract)
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.Output != "The answer is 42." || wire.NodeID != "node-t" || wire.Seat != agentTestSeat {
		t.Fatalf("wire = %+v", wire)
	}
	if string(wire.Structured) != `{"answer":"42"}` {
		t.Fatalf("structured = %s", wire.Structured)
	}

	// The job-scoped materialization dir must be gone when the run returns.
	entries, rerr := os.ReadDir(filepath.Join(home, "pipeline-jobs"))
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("pipeline-jobs left %d entries behind; the job dir must live exactly as long as the run", len(entries))
	}
}

// TestRunAgentContractInvalidContractErrors: Validate gates before any
// materialization or model call.
func TestRunAgentContractInvalidContractErrors(t *testing.T) {
	p, home := agentContractPipeline(t, "http://127.0.0.1:1")
	bad := testContract()
	bad.Goal = "   "
	if _, err := p.RunAgentContract(context.Background(), bad); err == nil {
		t.Fatal("a goal-less contract must error before running")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "pipeline-jobs")); len(entries) != 0 {
		t.Fatal("an invalid contract must not leave a materialized job dir")
	}
}
