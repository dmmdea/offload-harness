package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// RSI-OH-1: the failure-class register reader. Rows are hand-written JSONL (not built through Record), because
// Record always writes the cards_tokens key and so cannot produce the one case that matters here: a row that
// predates it.

func writeFailureLedger(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func classNamed(t *testing.T, reg FailureRegister, class string) FailureClass {
	t.Helper()
	for _, c := range reg.Classes {
		if c.Class == class {
			return c
		}
	}
	t.Fatalf("class %q not in %+v", class, reg.Classes)
	return FailureClass{}
}

// A total that no row measured is nil (null in JSON); a row that carries the cards_tokens key with 0 measured a
// zero; a row without the key reads its older columns, and one that has none of them measured nothing.
func TestFailureTokensAreNilUntilARowMeasuredThem(t *testing.T) {
	path := writeFailureLedger(t,
		// no key, no older columns: unresolved
		`{"ts":10,"task":"vqa","deferred":true,"reason":"empty vision output","reason_code":"abstention"}`,
		// key present with 0: a measured zero
		`{"ts":11,"task":"agent_delegate","deferred":true,"reason_code":"node_busy","cards_tokens":0,"job_id":"a"}`,
		// no key, older columns: 120 + 30
		`{"ts":12,"task":"triage","deferred":true,"reason_code":"budget","tokens_in":120,"tokens_out":30}`,
		// orphan inner row: stored 0 by design, the work is in the older columns
		`{"ts":13,"task":"agent","deferred":true,"reason_code":"stall_decode","cards_tokens":0,"seat_tokens_in":40,"tokens_out":10,"parent_job_id":"gone"}`,
		// orphan inner row with no work in any column: unresolved even though the key says 0
		`{"ts":14,"task":"agent","deferred":true,"reason_code":"stall_tool","cards_tokens":0,"parent_job_id":"gone"}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*int64{"abstention": nil, "node_busy": ptr64(0), "budget": ptr64(150), "stall_decode": ptr64(50), "stall_tool": nil}
	for class, w := range want {
		c := classNamed(t, reg, class)
		switch {
		case w == nil && c.CardsTokens != nil:
			t.Errorf("%s: cards_tokens = %d, want nil (nothing measured)", class, *c.CardsTokens)
		case w != nil && (c.CardsTokens == nil || *c.CardsTokens != *w):
			t.Errorf("%s: cards_tokens = %v, want %d", class, c.CardsTokens, *w)
		}
	}
	if reg.CardsTokens == nil || *reg.CardsTokens != 200 || reg.RowsWithTokens != 3 || reg.FailureRows != 5 {
		t.Errorf("totals = %v over %d of %d rows, want 200 over 3 of 5", reg.CardsTokens, reg.RowsWithTokens, reg.FailureRows)
	}
	b, _ := json.Marshal(classNamed(t, reg, "abstention"))
	if !strings.Contains(string(b), `"cards_tokens":null`) {
		t.Errorf("an unresolved total must serialize as null, got %s", b)
	}
}

func ptr64(n int64) *int64 { return &n }

// Rows from before the reason code was stamped (F22) are classed with the ledger's own total classifier at read
// time and counted as derived; a stamped code outside the closed set, or one that says ok on a deferred row, is
// not trusted; an acceptance failure that did not defer is still a failed verification.
func TestFailureClassOfARowThatCarriesNoCode(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"generate_image","deferred":true,"err_class":"not_configured","reason":"no image model configured"}`,
		`{"ts":2,"task":"transcribe","deferred":true,"reason":"empty transcript"}`,
		`{"ts":3,"task":"ocr","deferred":true,"reason":"nobody classified this"}`,
		`{"ts":4,"task":"agent_delegate","deferred":true,"reason_code":"not_a_member","reason":"gpu busy: lease"}`,
		`{"ts":5,"task":"agent_delegate","deferred":true,"reason_code":"ok","reason":"seat busy: cap"}`,
		`{"ts":6,"task":"agent_delegate","deferred":false,"acceptance_result":"fail","reason_code":"failed_verification","job_id":"v"}`,
		`{"ts":7,"task":"agent_delegate","deferred":false,"acceptance_result":"fail","job_id":"w"}`,
		`{"ts":8,"task":"agent_delegate","deferred":false,"acceptance_result":"pass","reason_code":"ok","job_id":"p"}`,
		`{"ts":9,"task":"summarize","deferred":false,"cache_hit":true}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, c := range reg.Classes {
		got[c.Class] = c.Count
	}
	want := map[string]int{"config": 1, "abstention": 1, "other": 1, "node_busy": 2, "failed_verification": 2}
	if len(got) != len(want) {
		t.Fatalf("classes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("class %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	// 9 rows, 8 jobs the register looks at: the cache hit is a job but not a failure, nothing is skipped.
	if reg.JobRows != 9 || reg.FailureRows != 7 {
		t.Errorf("job_rows = %d, failure_rows = %d, want 9 and 7", reg.JobRows, reg.FailureRows)
	}
	// stamped and accepted as is: only the failed_verification row with a stamped code; every other failure row was classed here.
	if reg.ClassDerivedRows != 6 {
		t.Errorf("class_derived_rows = %d, want 6", reg.ClassDerivedRows)
	}
}

// since keeps the window but not at the price of the whole-file indexes: an inner row inside the window is
// still judged by a parent that is older than it.
func TestFailureRegisterWindowKeepsTheParentIndexOfTheWholeFile(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":100,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"old-parent"}`,
		`{"ts":200,"task":"agent","deferred":true,"reason_code":"budget","parent_job_id":"old-parent","seat_tokens_in":5}`,
		`{"ts":210,"task":"agent_delegate","deferred":true,"reason_code":"abstention","job_id":"new"}`,
	)
	reg, err := FailureRegisterFile(path, 150, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.FailureRows != 1 || len(reg.Classes) != 1 || reg.Classes[0].Class != "abstention" {
		t.Fatalf("window since=150: %+v, want the new abstention row only (the inner row's parent is outside the window but present)", reg.Classes)
	}
}

// Door, seat and model: the door as recorded, the seat as model_tier is recorded (<node>:<seat> on a delegation
// row, the model alias on a cascade row), the model the part after the last colon. Absent values are named, not
// dropped, and the breakdowns and groups come out count-descending then by name.
func TestFailureBreakdownsNameWhatIsMissingAndSortDeterministically(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-a:seat-1"}`,
		`{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-a:seat-1"}`,
		`{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-b:seat-1"}`,
		`{"ts":4,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":":seat-2"}`,
		`{"ts":5,"task":"summarize","deferred":true,"reason_code":"budget","door":"cli:summarize","model_tier":"model-z"}`,
		`{"ts":6,"task":"ocr","deferred":true,"reason_code":"budget"}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	c := classNamed(t, reg, "budget")
	nameCounts := func(fc []FailureCount) string {
		var parts []string
		for _, x := range fc {
			parts = append(parts, x.Name+"="+strconv.Itoa(x.Count))
		}
		return strings.Join(parts, ",")
	}
	if got, want := nameCounts(c.Doors), "agent_run=3,(unknown)=2,cli:summarize=1"; got != want {
		t.Errorf("doors = %s, want %s", got, want)
	}
	if got, want := nameCounts(c.Seats), "node-a:seat-1=2,(unrecorded)=1,model-z=1,node-b:seat-1=1,seat-2=1"; got != want {
		t.Errorf("seats = %s, want %s", got, want)
	}
	if got, want := nameCounts(c.Models), "seat-1=3,(unrecorded)=1,model-z=1,seat-2=1"; got != want {
		t.Errorf("models = %s, want %s (the part after the last colon, so the same alias on two nodes is one model)", got, want)
	}
	if len(c.Groups) != 5 {
		t.Fatalf("groups = %+v, want 5 distinct (door, seat, model)", c.Groups)
	}
	first := c.Groups[0]
	if first.Door != "agent_run" || first.Seat != "node-a:seat-1" || first.Model != "seat-1" || first.Count != 2 {
		t.Errorf("first group = %+v", first)
	}
}

func TestFailureRegisterOfAMissingLedgerIsEmptyNotAnError(t *testing.T) {
	reg, err := FailureRegisterFile(filepath.Join(t.TempDir(), "absent.jsonl"), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Classes == nil || len(reg.Classes) != 0 || reg.FailureRows != 0 || reg.CardsTokens != nil {
		t.Errorf("register of no ledger = %+v, want an empty class list and null tokens", reg)
	}
}
