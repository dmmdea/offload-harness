package agent

import (
	"encoding/json"
	"strconv"
	"testing"
)

// The compaction ladder's fire count has to be RECORDED with the run, because
// nothing else on disk says how often the ladder ran (the final transcript keeps only the
// bodies it left behind). The count rides the pager report: the pager is already fed at every
// ladder pass, so one counter there reads the same pass sites without a second hook in the loop.
//
// These tests read the report through its JSON, the form the trace files and the wire carry.

// intPtrText is a *int for a failure message: the value, or "absent" for nil. A bare pointer
// printed with %v is an address, and dereferencing it in the message would panic on the nil
// that is the very failure being reported.
func intPtrText(p *int) string {
	if p == nil {
		return "absent"
	}
	return strconv.Itoa(*p)
}

type pagerPassesJSON struct {
	CompactionPasses *int   `json:"compaction_passes"`
	Basis            string `json:"basis"`
}

func decodePagerPasses(t *testing.T, r PagerReport) pagerPassesJSON {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out pagerPassesJSON
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("pager report is not JSON: %v: %s", err, b)
	}
	return out
}

// TestRunCountsItsCompactionPasses: the pager run overflows a 4k window, so the ladder ran at
// least once, and the report must say so as a number a reader can sum.
func TestRunCountsItsCompactionPasses(t *testing.T) {
	got := decodePagerPasses(t, pagerRun(t).Pager)
	if got.CompactionPasses == nil {
		t.Fatalf("compaction_passes is absent from the pager report of a run that compacted")
	}
	if *got.CompactionPasses < 1 {
		t.Fatalf("compaction_passes = %d, want >= 1 for a run that overflowed its window", *got.CompactionPasses)
	}
}

// TestAQuietRunRecordsZeroPassesNotAbsence: a run that never compacted is a MEASURED zero. The
// field is present with the value 0, so a reader can tell it from a trace written before the
// field existed (absent), the same split PrefillSteps draws between "nothing reused" and "nothing
// observed".
func TestAQuietRunRecordsZeroPassesNotAbsence(t *testing.T) {
	var ps *PagerStats = &PagerStats{}
	got := decodePagerPasses(t, ps.Report())
	if got.CompactionPasses == nil {
		t.Fatalf("a loop that never compacted must report compaction_passes: 0, not omit it")
	}
	if *got.CompactionPasses != 0 {
		t.Fatalf("compaction_passes = %d on a fresh instrument, want 0", *got.CompactionPasses)
	}
	if got.Basis != "insufficient_data" {
		t.Fatalf("basis = %q on a run that evicted nothing, want insufficient_data", got.Basis)
	}
}

// TestEveryNoteCompactionCallIsOnePass: a pass that evicts nothing the pager keeps track of
// (every body under the size floor) still ran the ladder, so it counts; a nil instrument and an
// empty before-slice are not passes.
func TestEveryNoteCompactionCallIsOnePass(t *testing.T) {
	ps := &PagerStats{}
	small := []Msg{{Role: "tool", Content: "tiny"}}
	ps.NoteCompaction(small, small)
	ps.NoteCompaction(small, nil)
	ps.NoteCompaction(nil, nil) // nothing to compact: not a pass
	var nilStats *PagerStats
	nilStats.NoteCompaction(small, small) // must not panic
	got := decodePagerPasses(t, ps.Report())
	if got.CompactionPasses == nil || *got.CompactionPasses != 2 {
		t.Fatalf("compaction_passes = %s, want 2", intPtrText(got.CompactionPasses))
	}
}
