package delegate

// A refusal-marked row is never the end of a subtask (refusal_row_test.go): when nothing else takes the subtask, a row of its
// own says how it ended. settle() decides that from the placements ledger (pl.lastRowRefusal), the row of the LATEST attempt.
// exhaustedSettled used to decide it from the seed it was handed, and a seed that is a pseudo-attempt (a replacement the
// process gate turned away, which returns before finish and so has no row and no mark) hid the refused attempt before it: the
// wait then ended on the budget, the failure carried no job id and the only row of the lost subtask was the refusal-marked
// one, which the rigger and the fleet page ignore, so a lost subtask looked like nothing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// endAfterAGatedTurnAway runs the capacity wait of a subtask whose first attempt was refused (its row written, marked or not)
// and whose replacement was turned away by the process gate, with a budget under the floor of another attempt: the wait ends
// at its first look at the fleet, on the chain's own exhausted failure.
func endAfterAGatedTurnAway(t *testing.T, firstRowWasARefusal bool) (PlacedResult, config.Config) {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 5*time.Millisecond, 0)
	_, url := acceptingNode(t, "node-y", "answer from y", withHeadroom(2, 0))
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = 5
	pl := newPlacements()
	pl.countAttempt(PlacedResult{refusalRow: firstRowWasARefusal})
	pl.waitNote = "test: the dealt node was at its process gate"
	seed := PlacedResult{waitCapacity: true, gated: true, pendingReason: pl.waitNote}
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	contract := plainContract()
	pr := r.awaitCapacity(t.Context(), 0, contract, time.Now(), 5, pl, seed, []string{"node-x: dispatch: status 503: queue full"}, "")
	return pr, cfg
}

func TestAChainEndedAfterAGatedTurnAwayFilesATerminalRowWhenTheLastRowWasARefusal(t *testing.T) {
	pr, cfg := endAfterAGatedTurnAway(t, true)
	if !strings.HasPrefix(pr.Err, replacementExhaustedPrefix) {
		t.Fatalf("result %+v, want the placement-refused failure", pr)
	}
	if pr.JobID == "" {
		t.Fatal("the failure carries no job id: the refused attempt's row is a refusal, so no row says how the subtask ended")
	}
	var terminal int
	for _, m := range wireRows(t, cfg) {
		if _, marked := m["refusal"].(map[string]any); marked {
			t.Errorf("a refusal-marked row under the failure: %v", m)
			continue
		}
		if str(m, "job_id") == pr.JobID {
			terminal++
			if !strings.HasPrefix(str(m, "error"), replacementExhaustedPrefix) {
				t.Errorf("the terminal row's error = %q, want the placement-refused failure", str(m, "error"))
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("%d terminal row(s) under the published id %q, want exactly 1", terminal, pr.JobID)
	}
	if pr.refusalRow {
		t.Error("the published outcome still carries the refusal mark")
	}
}

// The control: when the last row written was an outcome (a defer, a landing), it stands as the subtask's record and the
// failure is not owed a second row.
func TestAChainEndedAfterAGatedTurnAwayAddsNoRowWhenTheLastRowWasNotARefusal(t *testing.T) {
	pr, cfg := endAfterAGatedTurnAway(t, false)
	if !strings.HasPrefix(pr.Err, replacementExhaustedPrefix) {
		t.Fatalf("result %+v, want the placement-refused failure", pr)
	}
	if rows := rowsIfAny(t, cfg); len(rows) != 0 {
		t.Fatalf("%d row(s) written, want none: the last attempt's own row stands for the subtask", len(rows))
	}
}

// rowsIfAny is wireRows for a corpus that may not exist yet.
func rowsIfAny(t *testing.T, cfg config.Config) []map[string]any {
	t.Helper()
	if _, err := os.Stat(filepath.Join(cfg.BaseDir(), "delegation-log")); os.IsNotExist(err) {
		return nil
	}
	return wireRows(t, cfg)
}

// A seed that is itself a refused attempt (it went through finish: its row is marked) is a closing row's owner too, whatever
// the ledger it is handed says: the two readings agree in every flow of the engine, and the seed is the one that cannot be
// stale.
func TestExhaustedSettledAlsoReadsTheSeedItIsHanded(t *testing.T) {
	cfg := testCfg(t)
	r := &runner{cfg: cfg}
	last := PlacedResult{Node: "node-a", Seat: "remote-seat", JobID: "agd-last", ranBase: "http://192.0.2.1:1",
		refused: true, refusalStatus: 503, Err: "dispatch: status 503: queue full", refusalRow: true}
	pr := r.exhaustedSettled(plainContract(), last, newPlacements(), []string{"node-a: dispatch: status 503: queue full"}, "why", time.Now())
	if pr.JobID == "" || pr.JobID == last.JobID {
		t.Fatalf("job id %q, want one of its own: the refused attempt's id belongs to its refusal-marked row", pr.JobID)
	}
	var terminal int
	for _, m := range rowsIfAny(t, cfg) {
		if str(m, "job_id") == pr.JobID {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("%d terminal row(s) under %q, want 1", terminal, pr.JobID)
	}
}
