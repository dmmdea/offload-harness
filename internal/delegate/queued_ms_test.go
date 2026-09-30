package delegate

import (
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func runWithQueuedMs(t *testing.T) (string, []PlacedResult, Summary) {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	w := remoteWire("the qube answer", `{"answer":"42"}`)
	w.QueuedMs = 91234
	node := &fakeNode{
		t: t, token: "sekrit", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(n int64) (map[string]any, int) {
			if n == 1 {
				return map[string]any{"state": "running"}, http.StatusOK
			}
			return doneWire(t, w), http.StatusOK
		},
	}
	srv := node.server()
	cfg := testCfg(t)
	cfg.FleetAuthToken = "sekrit"
	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.LedgerPath, results, sum
}

// queued_ms from the node's wire result reaches the delegator's agent_delegate
// ledger row (the second of the two rows the ADR promises). Mutant: dropping
// `QueuedMs: pr.Result.QueuedMs` in runner.record.
func TestQueuedMsReachesTheDelegatorLedgerRow(t *testing.T) {
	path, results, _ := runWithQueuedMs(t)
	if results[0].Result.QueuedMs != 91234 {
		t.Fatalf("PlacedResult.Result.QueuedMs = %d: the node's wire key did not decode", results[0].Result.QueuedMs)
	}
	all, err := ledger.ReadAll(path)
	rows := ledger.JobRows(all) // one row per job: the dispatch marker is not a job (ADR 0064)
	if err != nil || len(rows) != 1 || rows[0].QueuedMs != 91234 {
		t.Fatalf("ledger rows = %+v (%v)", rows, err)
	}
}

// DECISION TEST — RED at 427ec6b1. The CLI/MCP-facing ResultWire carries
// contention_wait_sec and admission_wait_sec to the delegating caller but not
// queued_ms. Keep it if the ADR's "on the wire" was meant to reach the caller
// (then add QueuedMs to ResultWire and WireResponse); delete it if not.
func TestResultWireCarriesQueuedMs(t *testing.T) {
	_, results, sum := runWithQueuedMs(t)
	if _, has := wireOf(t, results, sum)["queued_ms"]; !has {
		t.Fatal("the result wire carries no queued_ms")
	}
}
