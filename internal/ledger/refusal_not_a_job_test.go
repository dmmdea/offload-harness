package ledger

import (
	"strings"
	"testing"
)

// A refused placement did no work (Entry.Refusal): the delegator outwaited it and placed the subtask again, and the
// subtask's outcome is another row. Counting the refusal row as a job inflated the savings summary's calls and
// deferred counts, put "dispatch: status 503: queue full" among the top defer reasons beside the real ones, and
// gave a session a row it never ran. These tests hold every reader that counts jobs to the one rule (CountsAsJob).
//
// Rows are hand-written JSONL, the way the failure register's tests are, so each field a reader sees is on the page.

// refusalFixture is three real jobs (a completed delegation, a deferred one, a cascade summarize), one refusal row
// of a session that ran nothing, and a test-class pair: a real job and its refusal.
func refusalFixture(t *testing.T) string {
	t.Helper()
	return writeFailureLedger(t,
		`{"ts":10,"task":"agent_delegate","job_id":"j1","node":"node-a","fleet_job_id":"f1","tokens_out":40,"cards_tokens":40,"origin_session":"sess-ran","reason_code":"ok"}`,
		`{"ts":11,"task":"agent_delegate","job_id":"j2","deferred":true,"reason":"stalled while decoding","reason_code":"stall_decode","cards_tokens":0,"origin_session":"sess-ran"}`,
		`{"ts":12,"task":"summarize","tokens_in":100,"tokens_out":20,"cards_tokens":100,"origin_session":"sess-ran"}`,
		`{"ts":13,"task":"agent_delegate","job_id":"r1","node":"node-a","deferred":true,"reason":"dispatch: status 503: queue full","reason_code":"queue_full","refusal":true,"cards_tokens":0,"origin_session":"sess-refused"}`,
		`{"ts":14,"task":"agent_delegate","job_id":"t1","deferred":true,"reason":"a test fixture deferred","reason_code":"budget","cards_tokens":0,"run_class":"test"}`,
		`{"ts":15,"task":"agent_delegate","job_id":"t2","node":"node-a","deferred":true,"reason":"dispatch: status 503: queue full","reason_code":"queue_full","refusal":true,"cards_tokens":0,"run_class":"test"}`,
	)
}

func TestARefusalRowIsNotAJobInAnyJobCounter(t *testing.T) {
	path := refusalFixture(t)

	t.Run("the savings summary counts the three real jobs", func(t *testing.T) {
		s, err := SummarizeFile(path, 0, Prices{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Calls != 3 || s.Completed != 2 || s.Deferred != 1 {
			t.Errorf("calls/completed/deferred = %d/%d/%d, want 3/2/1 (a refusal row is not a job)", s.Calls, s.Completed, s.Deferred)
		}
		if s.ByTask["agent_delegate"] != 2 {
			t.Errorf("by_task[agent_delegate] = %d, want 2 (the completed and the deferred delegation)", s.ByTask["agent_delegate"])
		}
		if s.TokensSaved != 100 || s.TokensOut != 60 {
			t.Errorf("tokens saved/out = %d/%d, want 100/60: a refusal row adds neither", s.TokensSaved, s.TokensOut)
		}
		// Test traffic is left out and counted per class: the test-class job is one, its refusal is not.
		if s.ExcludedNonProduction["test"] != 1 {
			t.Errorf("excluded_non_production = %v, want test:1 (a refusal row is not an excluded job either)", s.ExcludedNonProduction)
		}
	})

	t.Run("the top defer reasons do not list the refusal", func(t *testing.T) {
		reasons, err := TopDeferReasons(path, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(reasons) != 1 || !strings.Contains(reasons[0].Reason, "stalled while decoding") || reasons[0].Count != 1 {
			t.Errorf("top defer reasons = %+v, want only the one real defer", reasons)
		}
		for _, r := range reasons {
			if strings.Contains(r.Reason, "503") || strings.Contains(r.Reason, "queue full") {
				t.Errorf("the refusal %q is listed as a defer reason", r.Reason)
			}
		}
	})

	t.Run("the job rows leave the refusal out", func(t *testing.T) {
		all, err := ReadAll(path)
		if err != nil {
			t.Fatal(err)
		}
		kept, excluded := ProductionRows(all)
		if got := len(JobRows(kept)); got != 3 {
			t.Errorf("JobRows over the production rows = %d, want 3", got)
		}
		if excluded["test"] != 1 {
			t.Errorf("ProductionRows excluded = %v, want test:1 (a test-class refusal is not a job it skipped)", excluded)
		}
		parents := ParentJobIDs(all)
		for _, e := range all {
			if e.Refusal && CountsAsJob(e, parents) {
				t.Errorf("CountsAsJob is true for the refusal row %q", e.JobID)
			}
		}
	})

	t.Run("a session that only had a refusal has no row", func(t *testing.T) {
		shares, _, err := SessionSharesFile(path, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range shares {
			if s.Session == "sess-refused" {
				t.Errorf("session %q has %d row(s): a refusal is work it never ran", s.Session, s.Rows)
			}
		}
		if s := shareOf(t, shares, "sess-ran"); s.Rows != 3 {
			t.Errorf("session sess-ran rows = %d, want its 3 jobs", s.Rows)
		}
	})

	// The failure register already set the refusal aside; the new rule must leave its numbers exactly as they were.
	t.Run("the failure register still counts the refusal as a refused attempt", func(t *testing.T) {
		reg, err := FailureRegisterFile(path, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if reg.RefusedAttempts != 1 || reg.JobRows != 3 || reg.FailureRows != 1 {
			t.Errorf("refused_attempts/job_rows/failure_rows = %d/%d/%d, want 1/3/1", reg.RefusedAttempts, reg.JobRows, reg.FailureRows)
		}
		if reg.ExcludedNonProduction["test"] != 1 {
			t.Errorf("excluded_non_production = %v, want test:1", reg.ExcludedNonProduction)
		}
		all, err := FailureRegisterFile(path, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		if all.RefusedAttempts != 2 || all.JobRows != 4 {
			t.Errorf("with test traffic: refused_attempts/job_rows = %d/%d, want 2/4", all.RefusedAttempts, all.JobRows)
		}
	})
}

// A refusal row that names a parent job is an inner row of it: the register never counted such a row as a refused
// attempt (an inner row is not a job whatever else it is), and the new rule must not start to.
func TestAnInnerRefusalRowIsNeitherAJobNorARefusedAttempt(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","job_id":"p1","reason_code":"ok","cards_tokens":5}`,
		`{"ts":2,"task":"agent","parent_job_id":"p1","deferred":true,"reason_code":"queue_full","refusal":true,"cards_tokens":0}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.RefusedAttempts != 0 || reg.JobRows != 1 {
		t.Errorf("refused_attempts/job_rows = %d/%d, want 0/1", reg.RefusedAttempts, reg.JobRows)
	}
}
