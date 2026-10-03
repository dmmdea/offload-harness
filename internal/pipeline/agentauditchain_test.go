package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// TestContractDoorChainsAndClosesItsRun (register SF-08): with audit_chain on, a writing
// contract leaves one chained run on the trail, closed by its run_end, that verifies
// clean; with it off the rows are unchained, as before.
func TestContractDoorChainsAndClosesItsRun(t *testing.T) {
	for _, chain := range []bool{true, false} {
		home := t.TempDir()
		homeAt(t, home)
		srv := writingFake().server(t)
		p := writeDoorPipeline(t, srv.URL, true)
		p.cfg.AuditAllDoors, p.cfg.AuditChain = "warn", chain
		wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, writeContract("."))))
		srv.Close()
		if wire.Deferred {
			t.Fatalf("chain=%v: deferred: %s", chain, wire.Reason)
		}
		rep, err := agent.VerifyAuditFile(filepath.Join(home, ".local-offload", "agent-audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case chain && (rep.Runs != 1 || len(rep.Open) != 0 || len(rep.Breaks) != 0 || rep.Chained < 2 || rep.Unchained != 0):
			t.Errorf("chain on: %+v, want one closed, clean chained run", rep)
		case !chain && (rep.Runs != 0 || rep.Unchained == 0):
			t.Errorf("chain off: %+v, want unchained rows only", rep)
		}
	}
}
