package ledger

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the ledger half of PR-14 (ADR 0064, register C-68): the full
// reason, the closed reason code, the fleet job id and the dispatch marker.

func openLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, p
}

// TestFullReasonIsNotCutAt120: the stored reason is the whole reason. It was cut
// at 120 bytes on write, which turned every long refusal, deadline and stall
// message into a mid-word stub ("... exceeds the availa") that no reader could
// classify — the ledger had recorded that something failed and thrown away why.
func TestFullReasonIsNotCutAt120(t *testing.T) {
	l, p := openLedger(t)
	long := "queue deadline after 5m0s: the node accepted the job but never started it — " + strings.Repeat("it waited in the node's backlog and never reached running; ", 12)
	if len(long) < 500 {
		t.Fatalf("fixture too short (%d bytes) to prove anything", len(long))
	}
	if err := l.Record(Entry{Task: "agent_delegate", JobID: "agd-1", Deferred: true, Reason: long}); err != nil {
		t.Fatal(err)
	}
	// A reason past the storage bound is still cut — on a rune boundary — so a
	// runaway upstream error cannot bloat the one-line record.
	huge := strings.Repeat("é", maxStoredReasonLen) // two bytes per rune
	if err := l.Record(Entry{Task: "agent_delegate", JobID: "agd-2", Deferred: true, Reason: huge}); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadAll(p)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d (%v), want 2", len(rows), err)
	}
	if rows[0].Reason != long {
		t.Fatalf("stored reason = %d bytes %q, want the full %d-byte reason", len(rows[0].Reason), rows[0].Reason, len(long))
	}
	if n := len(rows[1].Reason); n > maxStoredReasonLen || n < maxStoredReasonLen-3 {
		t.Fatalf("a runaway reason was stored as %d bytes, want it bounded at %d", n, maxStoredReasonLen)
	}
	if !json.Valid([]byte(rows[1].Reason)) && strings.ContainsRune(rows[1].Reason, '�') {
		t.Fatal("the bound split a multi-byte character")
	}
}

// TestShortReasonKeepsReaderGroupingStable: readers that group by reason must not
// fragment a class into one group per job-specific number now that the stored
// reason is whole. Two rows whose reasons differ only past the short form are ONE
// class in the defer report.
func TestShortReasonKeepsReaderGroupingStable(t *testing.T) {
	l, p := openLedger(t)
	head := "queue deadline after 5m0s: the node accepted the job but never started it — it waited in the node's backlog and never reached running"
	if len(head) <= maxReasonLen+10 {
		t.Fatalf("fixture head is %d bytes: it must run past the short form for the two reasons to differ only after it", len(head))
	}
	for i, tail := range []string{" (31 poll(s) answered `accepted`)", " (44 poll(s) answered `accepted`)"} {
		if err := l.Record(Entry{Task: "agent_delegate", JobID: "agd-" + string(rune('a'+i)), Deferred: true, Reason: head + tail}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := TopDeferReasons(p, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("TopDeferReasons = %+v, want ONE class of 2 (grouped by the short form)", got)
	}
	if len(got[0].Reason) > maxReasonLen {
		t.Fatalf("the class label is %d bytes, want the short form (<= %d)", len(got[0].Reason), maxReasonLen)
	}
	if s := ShortReason(strings.Repeat("é", 200)); len(s) > maxReasonLen || strings.ContainsRune(s, '�') {
		t.Fatalf("ShortReason(200 x é) = %d bytes, want a rune-safe cut at <= %d", len(s), maxReasonLen)
	}
}

// TestReasonCodeIsAlwaysSet: Record is the one path every writer takes, so it is
// where "every agent_delegate row carries a code" is made true — a completed row
// says ok, a failed row a member of the closed set, and a code a writer invented
// cannot mint a new group.
func TestReasonCodeIsAlwaysSet(t *testing.T) {
	l, p := openLedger(t)
	rows := []Entry{
		{Task: "agent_delegate", JobID: "agd-ok"},                                   // completed, writer set no code
		{Task: "agent_delegate", JobID: "agd-fail", Deferred: true, Reason: "boom"}, // failed, writer set no code
		{Task: "agent_delegate", JobID: "agd-code", Deferred: true, ReasonCode: ReasonQueueDeadline},
		{Task: "agent_delegate", JobID: "agd-typo", Deferred: true, ReasonCode: "queue_deadlne"},
		{Task: "agent_delegate", JobID: "agd-mark", Phase: PhaseStarted}, // the dispatch marker
		{Task: "summarize", TokensIn: 10, TokensOut: 2},                  // not a delegate row
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadAll(p)
	if err != nil || len(got) != len(rows) {
		t.Fatalf("rows = %d (%v), want %d", len(got), err, len(rows))
	}
	want := map[string]string{
		"agd-ok": ReasonOK, "agd-fail": ReasonOther, "agd-code": ReasonQueueDeadline,
		"agd-typo": ReasonOther, "agd-mark": ReasonStarted,
	}
	for _, e := range got[:5] {
		if !IsReasonCode(e.ReasonCode) {
			t.Errorf("%s: reason_code %q is not in the closed set", e.JobID, e.ReasonCode)
		}
		if e.ReasonCode != want[e.JobID] {
			t.Errorf("%s: reason_code = %q, want %q", e.JobID, e.ReasonCode, want[e.JobID])
		}
	}
	// A row that is not a delegate row is left exactly as it was: no code key.
	raw, _ := json.Marshal(got[5])
	if strings.Contains(string(raw), "reason_code") {
		t.Errorf("a cascade row gained a reason_code: %s", raw)
	}
	// The set itself: every listed code is valid and there are no duplicates.
	seen := map[string]bool{}
	for _, c := range ReasonCodes() {
		if !IsReasonCode(c) || seen[c] {
			t.Errorf("reason code %q is invalid or listed twice", c)
		}
		seen[c] = true
	}
	if IsReasonCode("") || IsReasonCode("nonsense") {
		t.Error("IsReasonCode accepts a value outside the set")
	}
}

// TestFleetJobIDPhaseAndReasonCodeRoundTrip: the new columns are additive — they
// appear as their own JSON keys when set and not at all when not, so a reader one
// release behind decodes every row unchanged.
func TestFleetJobIDPhaseAndReasonCodeRoundTrip(t *testing.T) {
	l, p := openLedger(t)
	if err := l.Record(Entry{Task: "agent_delegate", JobID: "agd-9", FleetJobID: "agd-9", Phase: PhaseStarted, Door: "agent_delegate"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "summarize", TokensIn: 5}); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadAll(p)
	if got[0].FleetJobID != "agd-9" || got[0].Phase != PhaseStarted || got[0].Door != "agent_delegate" {
		t.Fatalf("round trip lost a column: %+v", got[0])
	}
	plain, _ := json.Marshal(got[1])
	for _, key := range []string{"fleet_job_id", "phase", "reason_code"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("an ordinary cascade row carries %q: %s", key, plain)
		}
	}
}

// TestStartedRowsAreNotJobs: the dispatch marker carries the job's id and no
// outcome, so no job counter may count it — and it must not register as a PARENT,
// or the inner row of a job whose finished row never landed (the delegator died
// mid-run) would be dropped as the inner row of a parent that is only a marker,
// and the job would vanish from every count.
func TestStartedRowsAreNotJobs(t *testing.T) {
	l, p := openLedger(t)
	mark := func(id string) Entry { return Entry{Task: "agent_delegate", JobID: id, Phase: PhaseStarted} }
	// job-a: marker + finished row. job-b: marker only (a hang). job-c: marker + an
	// orphan inner row (the delegator died before it wrote the finished row).
	for _, e := range []Entry{
		mark("job-a"), {Task: "agent_delegate", JobID: "job-a", TokensOut: 40},
		mark("job-b"),
		mark("job-c"), {Task: "agent", JobID: "local-1", ParentJobID: "job-c", TokensIn: 200, TokensOut: 30},
	} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := ReadAll(p)
	if ids := ParentJobIDs(entries); ids["job-b"] || !ids["job-a"] || ids["job-c"] {
		t.Fatalf("ParentJobIDs = %v: a marker must not register as a parent (job-b), a finished row must (job-a)", ids)
	}
	rows := JobRows(entries)
	var jobs []string
	for _, e := range rows {
		jobs = append(jobs, e.JobID)
		if e.Phase == PhaseStarted {
			t.Fatalf("JobRows returned a marker row: %+v", e)
		}
	}
	// job-a's finished row and job-c's orphan inner row are the two jobs; job-b never finished.
	if len(rows) != 2 || jobs[0] != "job-a" || jobs[1] != "local-1" {
		t.Fatalf("JobRows = %v, want [job-a local-1]: the finished row, and the orphan inner row that is job-c's only record", jobs)
	}
	s, err := SummarizeFile(p, 0, DefaultPrices)
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 2 || s.ByTask["agent_delegate"] != 1 || s.ByTask["agent"] != 1 {
		t.Fatalf("Summarize = %+v, want 2 calls (job-a's finished row, job-c's orphan inner row) and no marker", s)
	}
	if s.TokensSaved != 200 {
		t.Fatalf("TokensSaved = %d, want 200 (the orphan inner row's savings; a marker adds none)", s.TokensSaved)
	}
}
