package ledger

import (
	"path/filepath"
	"testing"
)

// Register C-62: a route=local job wrote TWO rows — the pipeline's `agent`
// row and the delegator's `agent_delegate` row — and every reader counted
// both (1,002 of 1,002 local rows paired, 2026-09-19..27), so every share,
// success rate and token figure was ~2x. The inner row now names its parent
// and is written and read as part of that job, never as a second one.

func TestRecordInnerRowCarriesNoCardsTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "agent", ParentJobID: "agd-1", JobID: "agent-local-7", TokensIn: 900, TokensOut: 120}); err != nil {
		t.Fatal(err)
	}
	// A caller-supplied figure on an inner row is ignored too: the parent row
	// holds the card work, whatever the inner writer computed.
	if err := l.Record(Entry{Task: "agent", ParentJobID: "agd-2", TokensOut: 5, CardsTokens: 77}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "agent_delegate", JobID: "agd-1", TokensOut: 120, SeatTokensIn: 900}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	rows, err := ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ParentJobID != "agd-1" || rows[0].CardsTokens != 0 {
		t.Fatalf("inner row: parent=%q cards_tokens=%d, want agd-1 / 0", rows[0].ParentJobID, rows[0].CardsTokens)
	}
	if rows[1].CardsTokens != 0 {
		t.Fatalf("inner row with a caller figure: cards_tokens=%d, want 0", rows[1].CardsTokens)
	}
	if rows[2].CardsTokens != 1020 {
		t.Fatalf("parent row cards_tokens=%d, want 900+120", rows[2].CardsTokens)
	}
}

func TestSummarizeCountsALocalJobOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// One local job: the inner row carries the savings, the parent the job.
	_ = l.Record(Entry{Task: "agent", ParentJobID: "agd-1", TokensIn: 900, TokensOut: 120})
	_ = l.Record(Entry{Task: "agent_delegate", JobID: "agd-1", TokensOut: 120, SeatTokensIn: 900})
	// A standalone agent row (a door that runs the loop itself) is a job.
	_ = l.Record(Entry{Task: "agent", JobID: "agent-local-9", TokensIn: 50, TokensOut: 10})
	// A deferred inner row adds nothing; its parent carries the defer.
	_ = l.Record(Entry{Task: "agent", ParentJobID: "agd-3", TokensIn: 400, Deferred: true, Reason: "stalled"})
	_ = l.Record(Entry{Task: "agent_delegate", JobID: "agd-3", Deferred: true, Reason: "stalled"})
	l.Close()

	s, err := SummarizeFile(p, 0, Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 3 || s.Completed != 2 || s.Deferred != 1 {
		t.Fatalf("calls/completed/deferred = %d/%d/%d, want 3/2/1 (each local job once): %+v", s.Calls, s.Completed, s.Deferred, s)
	}
	if s.ByTask["agent_delegate"] != 2 || s.ByTask["agent"] != 1 {
		t.Fatalf("ByTask = %v, want agent_delegate 2 and only the standalone agent row", s.ByTask)
	}
	if s.TokensSaved != 950 {
		t.Fatalf("TokensSaved = %d, want 900 (inner) + 50 (standalone)", s.TokensSaved)
	}
	if s.TokensOut != 130 {
		t.Fatalf("TokensOut = %d, want 120 (parent) + 10 (standalone), never the inner row's 120 again", s.TokensOut)
	}

	reasons, err := TopDeferReasons(p, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || reasons[0].Count != 1 {
		t.Fatalf("defer reasons = %+v, want the one job's defer counted once", reasons)
	}
}

// An ORPHAN inner row — its parent row never landed (a failed write, a
// process killed between the two writes) — is its job's only record and
// counts as the job; a paired inner row never does.
func TestJobRowsKeepsOrphansAndDropsPairedInnerRows(t *testing.T) {
	rows := []Entry{
		{Task: "agent", JobID: "agent-local-1", ParentJobID: "agd-1"},
		{Task: "agent_delegate", JobID: "agd-1"},
		{Task: "agent", JobID: "agent-local-2", ParentJobID: "agd-missing"},
		{Task: "summarize"},
	}
	got := JobRows(rows)
	if len(got) != 3 {
		t.Fatalf("job rows = %d, want the parent, the orphan and the plain row", len(got))
	}
	for _, e := range got {
		if e.JobID == "agent-local-1" {
			t.Fatal("a paired inner row was counted as a job")
		}
	}
}

func TestSummarizeCountsAnOrphanInnerRowAsItsJob(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Record(Entry{Task: "agent", ParentJobID: "agd-gone", TokensIn: 300, TokensOut: 40, Deferred: true, Reason: "stalled"})
	l.Close()
	s, err := SummarizeFile(p, 0, Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 || s.Deferred != 1 || s.ByTask["agent"] != 1 {
		t.Fatalf("an orphan inner row vanished from the counts: %+v", s)
	}
	reasons, err := TopDeferReasons(p, 0, 5)
	if err != nil || len(reasons) != 1 {
		t.Fatalf("the orphan's defer reason must be counted: %+v (%v)", reasons, err)
	}
}
