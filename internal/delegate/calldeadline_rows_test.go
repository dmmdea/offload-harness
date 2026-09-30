package delegate

// The rows a cut leaves behind (ADR 0065 decision 2: the wire result, the ledger row and
// the corpus row say the same thing). A cut that ends a subtask which had already made an
// attempt, and one whose goroutine the call gave up on, were both invisible to the
// telemetry — the readbacks that look for the deadline wording in the rows (the per-call
// wall check, "the only refusal wording is call deadline reached") could not see them.

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// TestAWaitCutAfterARefusalIsRecordedLikeAnyOtherCut: a node refuses the first dispatch
// (503), the subtask waits for capacity, and the call ends the wait. The published result
// is the call-deadline defer; the ledger and the corpus must say so too, under the job id
// the caller was given — a row of its own, because none of the refused attempt's rows can:
// theirs say "dispatch ... 503" and belong to a job no node ever held.
func TestAWaitCutAfterARefusalIsRecordedLikeAnyOtherCut(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30

	results, sum, _ := runWithin(t, 5*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)
	pr := results[0]
	if sum.Deferred != 1 || sum.Failed != 0 || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || pr.JobID == "" {
		t.Fatalf("summary %+v result %+v, want the call-deadline defer with a job id", sum, pr)
	}

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var closing, refused int
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.Reason, deadlinePrefix):
			closing++
			if row.JobID != pr.JobID || !row.Deferred {
				t.Fatalf("the closing row = job %q deferred %v, want the job id the caller was given (%q)", row.JobID, row.Deferred, pr.JobID)
			}
		case strings.Contains(row.Reason, "status 503"):
			refused++
			if row.JobID == pr.JobID {
				t.Fatalf("a refused attempt's row carries the job id the caller was given (%q): two rows would double-count one id", pr.JobID)
			}
		}
	}
	if closing != 1 || refused < 1 {
		t.Fatalf("ledger: %d closing row(s) with the deadline wording and %d refused-attempt row(s) among %d, want exactly 1 and at least 1", closing, refused, len(rows))
	}
	var corpus int
	for _, line := range corpusLines(t, cfg) {
		if line.JobID == pr.JobID {
			corpus++
			if line.Result == nil || !strings.HasPrefix(line.Result.Reason, deadlinePrefix+"1 unfinished") || !line.Deferred {
				t.Fatalf("the corpus row for the closing job = %+v, want the call-deadline defer", line)
			}
		}
	}
	if corpus != 1 {
		t.Fatalf("%d corpus row(s) carry the job id the caller was given, want exactly 1", corpus)
	}
}

// TestASettledOutcomeThatIsNotACutKeepsItsAttemptsRow: the closing row is for a CUT only.
// A capacity wait that ran out of its own TTL after a refused attempt (no deadline in
// sight) is still recorded by the attempt's row alone, exactly as before.
func TestASettledOutcomeThatIsNotACutKeepsItsAttemptsRow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, _, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(time.Hour), nil)
	if r := results[0].Result; !r.Deferred || r.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result %+v, want the capacity wait's own TTL defer", r)
	}
	rows, _ := ledger.ReadAll(cfg.LedgerPath)
	for _, row := range rows {
		if row.JobID == results[0].JobID && strings.HasPrefix(row.Reason, "capacity wait") {
			t.Fatalf("a wait that ended on its own TTL was recorded by a closing row too: %+v", row)
		}
	}
}
