package report

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// `local-offload stats` (register C-62, 0.143.0): a delegator-local job is one
// job — its inner `agent` row is skipped while the parent row is present.
func TestStatsCountALocalJobOnce(t *testing.T) {
	rows := []ledger.Entry{
		{Task: "agent", JobID: "agent-local-1", ParentJobID: "agd-1", TokensOut: 120, Deferred: true},
		{Task: "agent_delegate", JobID: "agd-1", TokensOut: 120, Deferred: true},
	}
	got := Summarize(rows, 0.5)
	if s, ok := got["agent"]; ok && s.N > 0 {
		t.Fatalf("the inner row was counted as an agent job: %+v", s)
	}
	if got["agent_delegate"].N != 1 || got["agent_delegate"].Deferred != 1 || got["agent_delegate"].TokensOut != 120 {
		t.Fatalf("agent_delegate = %+v, want the one job", got["agent_delegate"])
	}
}
