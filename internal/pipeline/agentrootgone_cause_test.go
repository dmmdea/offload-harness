package pipeline

// Register C-79, a lost root beside a cause of the loop's own.
//
// A write-door run whose tree is found gone when the door closes must publish no diff
// (a snapshot of a tree that is not there reads as every file deleted). What it is FILED as
// depends on what ended it. A run the loop ended cleanly takes the lost root as its cause
// (TestRunAgentTaskAWriteDoorRunWhoseRootVanishedAfterItsLastToolCallDefersWithoutADiff). A run
// the loop ended on a cause of its own keeps that cause: the tree going is not what stopped it, and
// filing the run as root-gone would hide the budget signal the delegator sizes contracts from.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The seat edits, then sits past the run's ceiling while the job directory is taken from under
// it. The run ended on the ceiling: a budget defer with the ceiling's own words, no diff, and a
// note that says why the write set is missing.
func TestRunAgentTaskAWriteDoorRunThatHitItsCeilingKeepsTheCeilingWhenItsRootIsGone(t *testing.T) {
	restore := compressLiveness(t, 60*time.Second, 30*time.Second, 2) // ceiling capped at 2 s
	defer restore()
	contract := writeContract(".")
	contract.OutputSchema = nil
	contract.TimeoutSec = 1
	req := agentTestRequest(t, contract)
	ctxDir, _ := req.Params["context_dir"].(string)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(editCallChat("e1", "util.go", "\treturn n\n", "\treturn n - 1\n")))
			return
		}
		_ = os.RemoveAll(ctxDir) // after the edit landed; no tool is called again
		select {                 // past the 2 s ceiling, inside the 60 s stall floor
		case <-time.After(6 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(doneChat("Fixed it.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))
	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "ceiling ") {
		t.Fatalf("deferred=%v reason=%q, want the ceiling defer: the loop ended on it, and the tree going was not what stopped the run", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Errorf("defer_class = %q, want %q: the budget signal must survive a tree that went too", wire.DeferClass, core.DeferClassBudget)
	}
	if strings.HasPrefix(wire.Reason, core.RootGoneReason) {
		t.Errorf("reason = %q: the run was filed as root-gone and the ceiling it hit is lost", wire.Reason)
	}
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Errorf("a run whose root vanished published a write set: diff=%q files=%v", wire.Diff, wire.DiffFiles)
	}
	if !strings.Contains(wire.WriteNote, "write set withheld") || !strings.Contains(wire.WriteNote, ctxDir) {
		t.Errorf("write_note = %q, want it to say the write set was withheld and name the root %q", wire.WriteNote, ctxDir)
	}
}
