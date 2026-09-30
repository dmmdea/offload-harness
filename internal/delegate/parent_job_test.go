package delegate

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Register C-62: the delegator's local placement must hand its own job id to
// the runner, so the runner's ledger row is written as an inner row of this
// job instead of a second job.
func TestRunLocalHandsTheRunnerItsParentJobID(t *testing.T) {
	pairAppDir(t)
	cfg := testCfg(t)
	cfg.Endpoint = "http://127.0.0.1:11434"
	var got LocalOptions
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		got = opts
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "local-seat"}, nil
	})
	res, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("results = %d", len(res))
	}
	if got.ParentJobID == "" {
		t.Fatal("the runner was handed no parent job id: its ledger row would count as a second job (C-62)")
	}
	if got.ParentJobID != res[0].JobID {
		t.Fatalf("parent job id %q != the delegator's job id %q", got.ParentJobID, res[0].JobID)
	}
}
