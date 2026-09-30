package delegate

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// A delegator that is away for longer than a node's poll lease (a suspended host, a
// long partition) comes back to find its accepted job reaped, and a job a delegator
// withdrew can be read by another. Both are the node saying the job NEVER RAN, and
// the fact that makes a queue-deadline withdrawal safe to re-place makes these safe
// too: offering the subtask to another node cannot arrange a double run. Read as a
// plain remote job error they ended the subtask for work nobody had done (ADR 0064).

// terminalNode is a fleet node that acks a job, keeps it `accepted` for two polls
// and then answers the given terminal error.
func terminalNode(t *testing.T, id, terminal string) *fakeNode {
	t.Helper()
	return &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id,
		pollState: func(n int64) (map[string]any, int) {
			if n <= 2 {
				return map[string]any{"state": "accepted"}, http.StatusOK
			}
			return map[string]any{"state": "error", "error": terminal}, http.StatusOK
		},
	}
}

// TestRunAJobTheNodeNeverRanIsReplacedNotReportedAsARemoteError walks the two
// terminal texts a node writes for a job it took out of its backlog, and the one
// it writes for a job that ran and failed, which must stay a failure.
func TestRunAJobTheNodeNeverRanIsReplacedNotReportedAsARemoteError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
		replaced bool
	}{
		{"reaped", fleetnode.ErrReaped, true},
		{"withdrawn", fleetnode.ErrWithdrawn, true},
		{"never started (the node shut down with it queued)", fleetnode.ErrNeverStarted, true},
		{"a job that ran and failed", "the seat exploded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
			first := terminalNode(t, "node-first", tc.terminal)
			firstURL := first.server().URL
			idle, idleURL := acceptingNode(t, "node-idle", "answer from the idle node", nil)

			cfg := testCfg(t)
			results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{firstURL, idleURL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			r := results[0]
			firstJob, _ := first.lastJobID.Load().(string)
			closed, open := intentNotes(t, cfg.StateDir)

			if !tc.replaced {
				if !strings.HasPrefix(r.Err, "remote job error: ") || !strings.Contains(r.Err, tc.terminal) {
					t.Fatalf("err = %q, want the remote job error of a job that ran", r.Err)
				}
				if idle.dispatches.Load() != 0 {
					t.Fatalf("node-idle was asked %d time(s): a job that RAN and failed must never be re-placed", idle.dispatches.Load())
				}
				if closed[firstJob] != intentNoteTerminal || len(open) != 0 {
					t.Fatalf("intent closed as %q (open=%v), want %q", closed[firstJob], open, intentNoteTerminal)
				}
				return
			}

			if r.Err != "" || r.Node != "node-idle" {
				t.Fatalf("err = %q node = %q, want the subtask re-placed on node-idle: the node said the job never ran", r.Err, r.Node)
			}
			if r.Replacements != 1 || !strings.Contains(r.ReplacementNote, "node-first") || !strings.Contains(r.ReplacementNote, tc.terminal) {
				t.Fatalf("replacements = %d, note = %q, want one, naming node-first and what it said", r.Replacements, r.ReplacementNote)
			}
			if idle.dispatches.Load() != 1 {
				t.Fatalf("node-idle saw %d dispatches, want 1", idle.dispatches.Load())
			}
			// The intent says what the delegator saw, in the words recovery uses for the
			// same observation, and nothing is left open for the recovery pass.
			if want := intentNoteNeverStarted + tc.terminal; closed[firstJob] != want || len(open) != 0 {
				t.Fatalf("intent closed as %q (open=%v), want %q", closed[firstJob], open, want)
			}
			// The abandoned attempt's own row names it: a job the node took back, with the
			// id the node knew it by.
			var row ledger.Entry
			for _, e := range readRows(t, cfg.LedgerPath) {
				if e.Phase == "" && e.JobID == firstJob {
					row = e
				}
			}
			if row.ReasonCode != ledger.ReasonQueueWithdrawn || row.FleetJobID != firstJob {
				t.Fatalf("row of the taken-back attempt = reason_code %q fleet_job_id %q, want %q and %q", row.ReasonCode, row.FleetJobID, ledger.ReasonQueueWithdrawn, firstJob)
			}
		})
	}
}
