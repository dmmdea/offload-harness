package ledger

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
)

// Register D-101 (ADR 0085): the card-side split of one session's tokens by where they ran. These tests pin the
// reader's rules one by one; each has a single-line mutant in the change that introduced it.

func shareOf(t *testing.T, shares []SessionShare, session string) SessionShare {
	t.Helper()
	for _, s := range shares {
		if s.Session == session {
			return s
		}
	}
	t.Fatalf("no entry for session %q in %+v", session, shares)
	return SessionShare{}
}

// The asker's rows split by where the work ran: a delegation that reached a node (its row carries the fleet job
// id) and a remote-lane row (it carries the node) are remote; a delegation that never left the box is local.
func TestSessionSharesSplitsAskerRowsByRemoteAndLocal(t *testing.T) {
	rows := []Entry{
		{Task: "agent_delegate", JobID: "agd-1", FleetJobID: "agd-1", CardsTokens: 700, OriginSession: "sess-abc"},
		{Task: "agent_delegate", JobID: "agd-2", CardsTokens: 300, OriginSession: "sess-abc"},
		{Task: "classify_image", Node: "node-a", NodeID: "node-a-fleet", FleetJobID: "text-1", CardsTokens: 200, OriginSession: "sess-abc"},
	}
	s := shareOf(t, SessionShares(rows), "sess-abc")
	if s.RemoteTokens != 900 || s.LocalTokens != 300 || s.CardsTokens != 1200 || s.Rows != 3 {
		t.Fatalf("split = %+v, want remote 900, local 300, total 1200 over 3 rows", s)
	}
	if math.Abs(s.RemoteShare-0.75) > 1e-9 {
		t.Fatalf("remote share = %v, want 0.75", s.RemoteShare)
	}
	// A row that names only the node (no fleet job id) is remote too.
	s = shareOf(t, SessionShares([]Entry{{Task: "vqa", Node: "node-a", CardsTokens: 50, OriginSession: "sess-n"}}), "sess-n")
	if s.RemoteTokens != 50 || s.LocalTokens != 0 {
		t.Fatalf("a row that names the node it ran on = %+v, want it remote", s)
	}
}

// A session that did no card work has a share of 0, never NaN (a NaN cannot be marshalled).
func TestSessionSharesOfAnIdleSessionIsZeroNotNaN(t *testing.T) {
	shares := SessionShares([]Entry{{Task: "summarize", CacheHit: true, OriginSession: "sess-idle"}})
	s := shareOf(t, shares, "sess-idle")
	if s.Rows != 1 || s.CardsTokens != 0 || s.RemoteShare != 0 {
		t.Fatalf("idle session = %+v, want one row, no tokens, share 0", s)
	}
	if _, err := json.Marshal(shares); err != nil {
		t.Fatalf("shares must marshal: %v", err)
	}
}

// A row from before cards_tokens existed carries it as 0 and the work in its older columns: the reader falls back
// to the one rule (CardsTokensOf) instead of reading it as free.
func TestSessionSharesFallsBackToTheOlderColumnsOnAPreCardsTokensRow(t *testing.T) {
	s := shareOf(t, SessionShares([]Entry{{Task: "summarize", TokensIn: 400, TokensOut: 100, OriginSession: "sess-old"}}), "sess-old")
	if s.LocalTokens != 500 || s.CardsTokens != 500 {
		t.Fatalf("pre-0.124 row = %+v, want its 500 prompt+output tokens", s)
	}
}

// A shared ledger holds the asker's row for a job and the node's row for the same job (a box that is both a
// session host and a fleet node). The node's row is SERVED work: it is reported under served_*, never added to
// the asker's own totals, so the job is not counted twice.
func TestSessionSharesNeverCountsAServedRowAsTheAskersOwn(t *testing.T) {
	rows := []Entry{
		{Task: "agent_delegate", Door: "offload_agent_delegate", JobID: "agd-1", FleetJobID: "agd-1", CardsTokens: 700, OriginSession: "sess-abc"},
		{Task: "agent", Door: "fleet", FleetJobID: "agd-1", CardsTokens: 690, OriginSession: "sess-abc", Requester: "node-a"},
	}
	s := shareOf(t, SessionShares(rows), "sess-abc")
	if s.CardsTokens != 700 || s.RemoteTokens != 700 || s.Rows != 1 {
		t.Fatalf("asker totals = %+v, want 700 over one row (not 1390)", s)
	}
	if s.ServedRows != 1 || s.ServedTokens != 690 {
		t.Fatalf("served = %d rows, %d tokens, want 1 and 690", s.ServedRows, s.ServedTokens)
	}
}

// A node's rows for an old asker (no session on the wire) are served work under the unattributed entry, which is
// reported on its own and never merged into a named session.
func TestSessionSharesReportsUnattributedRowsApartAndNeverMerges(t *testing.T) {
	rows := []Entry{
		{Task: "summarize", CardsTokens: 40, OriginSession: "sess-abc"},
		{Task: "summarize", CardsTokens: 11},
		{Task: "agent", Door: "fleet", FleetJobID: "agd-9", CardsTokens: 90},
	}
	shares := SessionShares(rows)
	if len(shares) != 2 {
		t.Fatalf("sessions = %+v, want the named one and the unattributed one", shares)
	}
	named, anon := shareOf(t, shares, "sess-abc"), shareOf(t, shares, "")
	if named.CardsTokens != 40 || named.ServedRows != 0 {
		t.Fatalf("named session = %+v, want only its own 40", named)
	}
	if anon.CardsTokens != 11 || anon.ServedRows != 1 || anon.ServedTokens != 90 {
		t.Fatalf("unattributed = %+v, want 11 own tokens and one served row of 90", anon)
	}
}

// The doc comment's floor, pinned: a cascade call routed to a node over the chat lane writes no node row and its
// own row names neither the node nor a fleet job, so it reads as local; so does a delegation whose dispatch marker
// failed to land. The reader says so in its help text; this keeps the behaviour from changing silently.
func TestSessionSharesReadsAChatLaneRowAsLocalTheDocumentedFloor(t *testing.T) {
	rows := []Entry{
		{Task: "classify", Door: "offload_classify", CardsTokens: 120, OriginSession: "sess-abc"},
		{Task: "agent_delegate", JobID: "agd-5", Route: "remote", CardsTokens: 600, OriginSession: "sess-abc"}, // dispatched, marker lost: no fleet_job_id
	}
	s := shareOf(t, SessionShares(rows), "sess-abc")
	if s.RemoteTokens != 0 || s.LocalTokens != 720 {
		t.Fatalf("floor = %+v, want 720 local tokens and 0 remote", s)
	}
}

func writeShareLedger(t *testing.T, rows []Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// The file reader applies the same rules every production view applies: a dispatch marker and an inner row are
// not jobs, and a run of a non-production class is left out and counted per class unless asked for.
func TestSessionSharesFileLeavesOutRunClassMarkersAndInnerRows(t *testing.T) {
	p := writeShareLedger(t, []Entry{
		{TS: 100, Task: "agent_delegate", JobID: "agd-real", FleetJobID: "agd-real", CardsTokens: 500, OriginSession: "sess-abc"},
		{TS: 101, Task: "agent_delegate", JobID: "agd-bake", FleetJobID: "agd-bake", CardsTokens: 80, RunClass: "bake", OriginSession: "sess-abc"},
		{TS: 102, Task: "agent_delegate", JobID: "agd-mark", Phase: PhaseStarted, CardsTokens: 999, OriginSession: "sess-abc"},
		{TS: 103, Task: "agent", ParentJobID: "agd-real", TokensIn: 300, OriginSession: "sess-abc"},
		{TS: 104, Task: "summarize", CardsTokens: 60, OriginSession: "bake:run-7"}, // a class-prefixed origin, no stamped class
	})
	shares, excluded, err := SessionSharesFile(p, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 {
		t.Fatalf("sessions = %+v, want only sess-abc (the bake-prefixed one is left out)", shares)
	}
	s := shareOf(t, shares, "sess-abc")
	if s.CardsTokens != 500 || s.Rows != 1 || s.RemoteTokens != 500 {
		t.Fatalf("sess-abc = %+v, want only the real job: 500 tokens, 1 row (the bake, the marker and the inner row are not it)", s)
	}
	if excluded["bake"] != 2 || len(excluded) != 1 {
		t.Fatalf("excluded = %v, want two bake jobs counted (the labelled one and the class-prefixed one)", excluded)
	}

	shares, excluded, err = SessionSharesFile(p, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded) != 0 {
		t.Fatalf("with traffic included nothing is left out, got %v", excluded)
	}
	if s := shareOf(t, shares, "sess-abc"); s.CardsTokens != 580 || s.Rows != 2 {
		t.Fatalf("sess-abc with traffic = %+v, want the real job and the bake: 580 tokens, 2 rows", s)
	}
	if s := shareOf(t, shares, "bake:run-7"); s.CardsTokens != 60 {
		t.Fatalf("bake:run-7 with traffic = %+v", s)
	}
}

// --since keeps only rows from the window, and what it leaves out as traffic is counted for the window too.
func TestSessionSharesFileHonoursSince(t *testing.T) {
	p := writeShareLedger(t, []Entry{
		{TS: 100, Task: "summarize", CardsTokens: 10, OriginSession: "sess-abc"},
		{TS: 500, Task: "summarize", CardsTokens: 20, OriginSession: "sess-abc"},
		{TS: 100, Task: "agent_delegate", JobID: "agd-old", CardsTokens: 5, RunClass: "bake", OriginSession: "sess-abc"},
		{TS: 500, Task: "agent_delegate", JobID: "agd-new", CardsTokens: 6, RunClass: "bake", OriginSession: "sess-abc"},
	})
	shares, excluded, err := SessionSharesFile(p, 300, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := shareOf(t, shares, "sess-abc"); s.CardsTokens != 20 || s.Rows != 1 {
		t.Fatalf("window = %+v, want the one row inside it", s)
	}
	if excluded["bake"] != 1 {
		t.Fatalf("excluded = %v, want the one bake job inside the window", excluded)
	}
}

// A ledger that does not exist is an empty answer, not an error (nothing recorded yet).
func TestSessionSharesFileOfAMissingLedgerIsEmpty(t *testing.T) {
	shares, excluded, err := SessionSharesFile(filepath.Join(t.TempDir(), "none.jsonl"), 0, false)
	if err != nil || len(shares) != 0 || len(excluded) != 0 {
		t.Fatalf("missing ledger = %+v %v %v, want empty and no error", shares, excluded, err)
	}
}

// Sessions come back busiest first (own plus served tokens), ties by name, so the output is stable.
func TestSessionSharesAreOrderedBusiestFirst(t *testing.T) {
	shares := SessionShares([]Entry{
		{Task: "summarize", CardsTokens: 10, OriginSession: "sess-b"},
		{Task: "summarize", CardsTokens: 30, OriginSession: "sess-c"},
		{Task: "summarize", CardsTokens: 10, OriginSession: "sess-a"},
	})
	got := []string{}
	for _, s := range shares {
		got = append(got, s.Session)
	}
	want := []string{"sess-c", "sess-a", "sess-b"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
