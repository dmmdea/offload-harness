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

// showTokens prints a token total for a failure message: the number, or "nil" for an unresolved one.
func showTokens(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

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
		`{"ts":10,"task":"summarize","deferred":true,"cache_hit":true,"reason":"a hit is served, not failed"}`,
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
	// 10 job rows: the two cache hits and the passing row are jobs but not failures.
	if reg.JobRows != 10 || reg.FailureRows != 7 {
		t.Errorf("job_rows = %d, failure_rows = %d, want 10 and 7", reg.JobRows, reg.FailureRows)
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

// A stored 0 is a measurement only when the row itself did the work or provably never reached a node. Record fills
// cards_tokens from the row's own columns, and a delegator's row for a job a node owned carries none of the node's
// work, so its stored 0 says "nothing recorded here" and the register must carry it as unresolved (nil), not as a
// measured 0. A reason code that says no node ran the job (it was refused, never placed, or taken back) keeps the 0
// even when the row names a node, and so does a row the node wrote for itself. Rows are hand-written because Record
// cannot produce a delegator row with the key at 0 and a node on it without also owning the figure.
func TestFailureZeroOnADelegatorRowIsNotAMeasurement(t *testing.T) {
	zero, ten := ptr64(0), ptr64(10)
	cases := []struct {
		name string
		line string
		want *int64
	}{
		// the work ran on a node and the delegator's row has no figure for it
		{"poll_deadline", `{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"poll_deadline","fleet_job_id":"f1","cards_tokens":0}`, nil},
		{"node_unreachable", `{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"node_unreachable","node":"node-a","cards_tokens":0}`, nil},
		{"remote_error", `{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"remote_error","node_id":"node-b","cards_tokens":0}`, nil},
		{"failed_verification", `{"ts":4,"task":"agent_delegate","deferred":false,"acceptance_result":"fail","fleet_job_id":"f4","cards_tokens":0}`, nil},
		// a node that accepted the job and never confirmed taking it back may still run it: not provably unrun
		{"queue_deadline", `{"ts":5,"task":"agent_delegate","deferred":true,"reason_code":"queue_deadline","fleet_job_id":"f5","cards_tokens":0}`, nil},
		// the caller went away after the node held the job: the node may have worked
		{"canceled", `{"ts":6,"task":"agent_delegate","deferred":true,"reason_code":"canceled","fleet_job_id":"f6","cards_tokens":0}`, nil},
		// a code that says no node ran the job keeps the measured zero, node on the row or not
		{"capacity_wait", `{"ts":7,"task":"agent_delegate","deferred":true,"reason_code":"capacity_wait","fleet_job_id":"f7","cards_tokens":0}`, zero},
		{"node_busy", `{"ts":8,"task":"agent_delegate","deferred":true,"reason_code":"node_busy","node":"node-a","cards_tokens":0}`, zero},
		{"queue_withdrawn", `{"ts":9,"task":"agent_delegate","deferred":true,"reason_code":"queue_withdrawn","fleet_job_id":"f9","node":"node-b","cards_tokens":0}`, zero},
		{"dispatch_refused", `{"ts":10,"task":"agent_delegate","deferred":true,"reason_code":"dispatch_refused","node":"node-b","cards_tokens":0}`, zero},
		// no node on the row: nothing else ran, so the stored 0 is the row's own
		{"no_eligible_node", `{"ts":11,"task":"agent_delegate","deferred":true,"reason_code":"no_eligible_node","cards_tokens":0}`, zero},
		{"infrastructure", `{"ts":12,"task":"generate_image","deferred":true,"reason_code":"infrastructure","cards_tokens":0}`, zero},
		// a node's own row for a job another box asked for: its stored figure is the node's own measurement
		{"budget", `{"ts":13,"task":"agent","deferred":true,"reason_code":"budget","door":"fleet","fleet_job_id":"f13","cards_tokens":0}`, zero},
		{"stall_decode", `{"ts":14,"task":"agent","deferred":true,"reason_code":"stall_decode","requester":"node-a","fleet_job_id":"f14","cards_tokens":0}`, zero},
		// work in the older columns is work, whatever the stored figure says
		{"stall_tool", `{"ts":15,"task":"agent_delegate","deferred":true,"reason_code":"stall_tool","fleet_job_id":"f15","tokens_out":10,"cards_tokens":0}`, ten},
		// a stored figure above 0 is the ledger's own number, even above the row's own columns (it carries inner rows' work)
		{"seat_down", `{"ts":16,"task":"agent_delegate","deferred":true,"reason_code":"seat_down","fleet_job_id":"f16","cards_tokens":10}`, ten},
		{"abstention", `{"ts":17,"task":"agent_delegate","deferred":true,"reason_code":"abstention","fleet_job_id":"f17","tokens_out":40,"cards_tokens":940}`, ptr64(940)},
	}
	var lines []string
	for _, c := range cases {
		lines = append(lines, c.line)
	}
	reg, err := FailureRegisterFile(writeFailureLedger(t, lines...), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := classNamed(t, reg, c.name)
		switch {
		case c.want == nil && got.CardsTokens != nil:
			t.Errorf("%s: cards_tokens = %d over %d row(s), want nil (the stored 0 is not a measurement)", c.name, *got.CardsTokens, got.RowsWithTokens)
		case c.want != nil && (got.CardsTokens == nil || *got.CardsTokens != *c.want):
			t.Errorf("%s: cards_tokens = %s, want %d", c.name, showTokens(got.CardsTokens), *c.want)
		}
		if wantRows := map[bool]int{true: 1, false: 0}[c.want != nil]; got.RowsWithTokens != wantRows {
			t.Errorf("%s: rows_with_tokens = %d, want %d", c.name, got.RowsWithTokens, wantRows)
		}
		for _, g := range got.Groups {
			if (g.CardsTokens == nil) != (c.want == nil) {
				t.Errorf("%s: group cards_tokens = %s, want nil=%v", c.name, showTokens(g.CardsTokens), c.want == nil)
			}
		}
	}

	// A class that mixes the two sums what was measured and says how many rows that was.
	reg, err = FailureRegisterFile(writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"job_lost","fleet_job_id":"a","cards_tokens":0}`,
		`{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"job_lost","fleet_job_id":"b","cards_tokens":300}`,
	), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := classNamed(t, reg, "job_lost"); c.Count != 2 || c.CardsTokens == nil || *c.CardsTokens != 300 || c.RowsWithTokens != 1 {
		t.Errorf("job_lost = %d rows, %s tokens over %d, want 2 rows, 300 over 1 (a floor)", c.Count, showTokens(c.CardsTokens), c.RowsWithTokens)
	}
	if reg.CardsTokens == nil || *reg.CardsTokens != 300 || reg.RowsWithTokens != 1 {
		t.Errorf("register totals = %s over %d rows, want 300 over 1", showTokens(reg.CardsTokens), reg.RowsWithTokens)
	}
}
