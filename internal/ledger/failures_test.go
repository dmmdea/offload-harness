package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The failure-class register reader. Rows are hand-written JSONL (not built through Record), because
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

// Door, seat and model: the door as recorded, the seat as model_tier is recorded (<node>:<seat alias> on a delegation
// row, the model alias on a cascade row), the model the part after the LAST colon (on a delegation row the seat
// alias: the ledger holds no model name there). A second colon belongs to the seat, so only the final one cuts.
// Absent values are named, not dropped, and the breakdowns and groups come out count-descending then by name.
func TestFailureBreakdownsNameWhatIsMissingAndSortDeterministically(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-a:seat-1"}`,
		`{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-a:seat-1"}`,
		`{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-b:seat-1"}`,
		`{"ts":4,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":":seat-2"}`,
		`{"ts":5,"task":"summarize","deferred":true,"reason_code":"budget","door":"cli:summarize","model_tier":"model-z"}`,
		`{"ts":6,"task":"ocr","deferred":true,"reason_code":"budget"}`,
		`{"ts":7,"task":"agent_delegate","deferred":true,"reason_code":"budget","door":"agent_run","model_tier":"node-c:extra:seat-4"}`,
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
	if got, want := nameCounts(c.Doors), "agent_run=4,(unknown)=2,cli:summarize=1"; got != want {
		t.Errorf("doors = %s, want %s", got, want)
	}
	if got, want := nameCounts(c.Seats), "node-a:seat-1=2,(unrecorded)=1,model-z=1,node-b:seat-1=1,node-c:extra:seat-4=1,seat-2=1"; got != want {
		t.Errorf("seats = %s, want %s", got, want)
	}
	if got, want := nameCounts(c.Models), "seat-1=3,(unrecorded)=1,model-z=1,seat-2=1,seat-4=1"; got != want {
		t.Errorf("models = %s, want %s (the part after the LAST colon: the same alias on two nodes is one entry, and a second colon stays in the seat)", got, want)
	}
	if len(c.Groups) != 6 {
		t.Fatalf("groups = %+v, want 6 distinct (door, seat, model)", c.Groups)
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

// Every class in neverRanClasses keeps a stored 0 as the row's own measurement, on a row built so that nothing else in
// zeroIsOwn can vouch for it: the row names the node and a fleet job and is not a node's own row, so neither the
// no-node term nor the served-door and requester term applies. The classes are written out here and not read from the
// map: a loop over the map would lose a class together with the entry that dropped it. The map is held to the list in
// both directions, so a class cannot join the set without this test saying so.
func TestFailureEveryNeverRanClassKeepsAStoredZeroOnARowThatNamesANode(t *testing.T) {
	never := []string{
		ReasonNodeBusy, ReasonCapacityWait, ReasonQueueFull, ReasonNoEligibleNode,
		ReasonQueueWithdrawn, ReasonShed, ReasonDispatchRefused,
		ReasonConfig, ReasonContract, ReasonBadInput,
	}
	row := func(ts int, class string) string {
		return `{"ts":` + strconv.Itoa(ts) + `,"task":"agent_delegate","deferred":true,"reason_code":"` + class +
			`","door":"agent_run","fleet_job_id":"f` + strconv.Itoa(ts) + `","node":"node-a","cards_tokens":0}`
	}
	listed := map[string]bool{}
	for _, c := range never {
		listed[c] = true
	}
	if len(listed) != len(never) {
		t.Fatalf("the list of never-ran classes repeats a class: %v", never)
	}
	if len(neverRanClasses) != len(listed) {
		t.Errorf("neverRanClasses holds %d classes, this test lists %d", len(neverRanClasses), len(listed))
	}
	for c := range neverRanClasses {
		if !listed[c] {
			t.Errorf("neverRanClasses holds %q, which this test does not list", c)
		}
	}

	// The same row under a class that says a run happened is not a measurement: the shape alone proves nothing.
	// write is one of them on purpose: a node refuses the write door before a run (agent_allow_write is false) or
	// after one (the finished write set broke a door cap), and the class does not say which, so its stored 0 is the
	// delegator's gap and not a reading that nothing ran.
	controls := []string{ReasonPollDeadline, ReasonWrite}
	var lines []string
	for i, c := range controls {
		lines = append(lines, row(i+1, c))
	}
	for i, c := range never {
		lines = append(lines, row(i+1+len(controls), c))
	}
	reg, err := FailureRegisterFile(writeFailureLedger(t, lines...), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range controls {
		if got := classNamed(t, reg, control); got.CardsTokens != nil || got.RowsWithTokens != 0 {
			t.Errorf("%s: cards_tokens = %s over %d row(s), want nil: the row shape is meant to be vouched for by the class alone",
				control, showTokens(got.CardsTokens), got.RowsWithTokens)
		}
	}
	for _, c := range never {
		got := classNamed(t, reg, c)
		if got.Count != 1 || got.CardsTokens == nil || *got.CardsTokens != 0 || got.RowsWithTokens != 1 {
			t.Errorf("%s: %d row(s), cards_tokens = %s over %d, want 1 row measured at 0 (no node ran the job)",
				c, got.Count, showTokens(got.CardsTokens), got.RowsWithTokens)
		}
		for _, g := range got.Groups {
			if g.CardsTokens == nil || *g.CardsTokens != 0 {
				t.Errorf("%s: group cards_tokens = %s, want 0", c, showTokens(g.CardsTokens))
			}
		}
	}
}

// A node's capacity refusal at dispatch is a place in line, not an outcome (the row carries refusal:true). The
// subtask it belonged to landed elsewhere, or ended on a terminal row of its own, so counting the refusal as a failed
// job read one subtask as two failures and inflated queue_full and job_rows. The register sets a marked row apart and
// counts it as a refused attempt, inside the same window and with the same test-traffic rule as every other row.
func TestFailureRegisterCountsARefusedAttemptApartFromFailures(t *testing.T) {
	path := writeFailureLedger(t,
		// one subtask: refused twice, then it landed
		`{"ts":100,"task":"agent_delegate","deferred":true,"reason_code":"queue_full","reason":"dispatch: status 503","job_id":"r1","refusal":true,"cards_tokens":0}`,
		`{"ts":101,"task":"agent_delegate","deferred":true,"reason_code":"queue_full","reason":"dispatch: status 503","job_id":"r2","refusal":true,"cards_tokens":0}`,
		`{"ts":102,"task":"agent_delegate","deferred":false,"reason_code":"ok","acceptance_result":"pass","job_id":"landed","cards_tokens":40}`,
		// a different subtask: refused, then the wait ended as a capacity defer on a row of its own
		`{"ts":200,"task":"agent_delegate","deferred":true,"reason_code":"queue_full","reason":"dispatch: status 429","job_id":"r3","refusal":true,"cards_tokens":0}`,
		`{"ts":201,"task":"agent_delegate","deferred":true,"reason_code":"capacity_wait","reason":"no node had room","job_id":"lost","cards_tokens":0}`,
		// an explicit false is not a mark
		`{"ts":202,"task":"agent_delegate","deferred":true,"reason_code":"queue_full","reason":"every node refused","job_id":"final","refusal":false,"cards_tokens":0}`,
		// test traffic: a refusal that is not production is neither a refused attempt nor an excluded job
		`{"ts":203,"task":"agent_delegate","deferred":true,"reason_code":"queue_full","job_id":"t1","refusal":true,"run_class":"test","cards_tokens":0}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.RefusedAttempts != 3 {
		t.Errorf("refused_attempts = %d, want 3 (the production refusals)", reg.RefusedAttempts)
	}
	// 3 jobs: the landing, the lost subtask's terminal row and the final queue_full row; no refusal row is one.
	if reg.JobRows != 3 || reg.FailureRows != 2 {
		t.Errorf("job_rows = %d, failure_rows = %d, want 3 and 2 (refused attempts are neither)", reg.JobRows, reg.FailureRows)
	}
	if c := classNamed(t, reg, "queue_full"); c.Count != 1 {
		t.Errorf("queue_full = %d, want 1: only the row that is an outcome, not the three place-in-line rows", c.Count)
	}
	if len(reg.ExcludedNonProduction) != 0 {
		t.Errorf("excluded_non_production = %v: a refused attempt of test traffic is not an excluded job", reg.ExcludedNonProduction)
	}
	// The window applies to a refusal like to any row.
	late, err := FailureRegisterFile(path, 150, false)
	if err != nil {
		t.Fatal(err)
	}
	if late.RefusedAttempts != 1 {
		t.Errorf("refused_attempts inside since=150 = %d, want 1", late.RefusedAttempts)
	}
	// With the test traffic counted, its refusal joins the refused attempts.
	all, err := FailureRegisterFile(path, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if all.RefusedAttempts != 4 {
		t.Errorf("refused_attempts with test traffic = %d, want 4", all.RefusedAttempts)
	}
	b, _ := json.Marshal(reg)
	if !strings.Contains(string(b), `"refused_attempts":3`) {
		t.Errorf("the register JSON has no refused_attempts: %s", b)
	}
}

// Every failure class has a kind, from a closed set: the question "how much of this is the stack failing" is not
// answerable from a count that puts a full queue, a caller's mistake and a dead seat on one footing
// (reasoncode.go files congestion as "a place in line, not a failure"; core's defer classes keep the caller's
// contract and a node's capacity apart from a broken stack). A reason code added to the closed set with no kind
// fails here, and so does a kind written for a code the set does not hold.
func TestEveryFailureClassHasAKindFromTheClosedSet(t *testing.T) {
	closed := map[string]bool{}
	for _, k := range FailureKinds() {
		closed[k] = true
	}
	if len(closed) != 6 {
		t.Fatalf("the closed kind set = %v, want stack_fault, capacity, caller, ceiling, work and unclassified", FailureKinds())
	}
	isFailure := map[string]bool{}
	for _, code := range ReasonCodes() {
		kind := FailureKindOf(code)
		if code == ReasonOK || code == ReasonStarted {
			if kind != "" {
				t.Errorf("%q is not a failure and must have no kind, got %q", code, kind)
			}
			continue
		}
		isFailure[code] = true
		if !closed[kind] {
			t.Errorf("failure class %q has kind %q, want one of %v", code, kind, FailureKinds())
		}
	}
	if got := FailureKindOf("not_a_member"); got != "" {
		t.Errorf("a code outside the closed set has kind %q, want none", got)
	}
	for code := range failureKinds {
		if !isFailure[code] {
			t.Errorf("failureKinds names %q, which is not a failure member of the closed reason-code set", code)
		}
	}
	// The anchors the kinds are read against: the wire defer classes (agentwire.go) and BrokenStackDefer's pair.
	for code, want := range map[string]string{
		ReasonInfrastructure: KindStackFault, ReasonConfig: KindStackFault, ReasonNodeUnreachable: KindStackFault, ReasonStallDecode: KindStackFault,
		ReasonNodeBusy: KindCapacity, ReasonQueueFull: KindCapacity, ReasonCapacityWait: KindCapacity, ReasonShed: KindCapacity,
		ReasonContract: KindCaller, ReasonCanceled: KindCaller, ReasonBadInput: KindCaller,
		ReasonBudget: KindCeiling, ReasonPollDeadline: KindCeiling,
		ReasonAbstention: KindWork, ReasonFailedVerification: KindWork,
	} {
		if got := FailureKindOf(code); got != want {
			t.Errorf("kind of %q = %q, want %q", code, got, want)
		}
	}
}

// The register carries the kind on each class, counts the failures by kind, and reports the stack-fault share on
// its own line: the failures that say the stack itself failed, over all failure rows. With no failure row the
// share is absent (null), not 0.
func TestFailureRegisterReportsKindsAndTheStackFaultShare(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"node_busy","job_id":"a","cards_tokens":0}`,
		`{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"node_busy","job_id":"b","cards_tokens":0}`,
		`{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"stall_decode","job_id":"c","cards_tokens":0}`,
		`{"ts":4,"task":"agent_delegate","deferred":true,"reason_code":"infrastructure","job_id":"d","cards_tokens":0}`,
		`{"ts":5,"task":"agent_delegate","deferred":true,"reason_code":"abstention","job_id":"e","cards_tokens":0}`,
		`{"ts":6,"task":"agent_delegate","deferred":true,"reason_code":"contract","job_id":"f","cards_tokens":0}`,
		`{"ts":7,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"g","cards_tokens":0}`,
		`{"ts":8,"task":"agent_delegate","deferred":false,"reason_code":"ok","acceptance_result":"pass","job_id":"h","cards_tokens":0}`,
		`{"ts":9,"task":"agent_delegate","deferred":true,"reason_code":"node_busy","job_id":"i","cards_tokens":0}`,
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]string{"node_busy": KindCapacity, "stall_decode": KindStackFault, "infrastructure": KindStackFault,
		"abstention": KindWork, "contract": KindCaller, "budget": KindCeiling} {
		if got := classNamed(t, reg, class).Kind; got != want {
			t.Errorf("class %s kind = %q, want %q", class, got, want)
		}
	}
	if reg.StackFaultRows != 2 || reg.StackFaultShare == nil || *reg.StackFaultShare != 0.25 {
		t.Errorf("stack_fault_rows = %d, share = %v, want 2 and 0.25 (2 of 8 failure rows; capacity holds 3, so the two are told apart)", reg.StackFaultRows, reg.StackFaultShare)
	}
	want := []FailureKindCount{{Kind: KindCapacity, Count: 3, Share: 0.375}, {Kind: KindStackFault, Count: 2, Share: 0.25},
		{Kind: KindCaller, Count: 1, Share: 0.125}, {Kind: KindCeiling, Count: 1, Share: 0.125}, {Kind: KindWork, Count: 1, Share: 0.125}}
	if len(reg.Kinds) != len(want) {
		t.Fatalf("kinds = %+v, want %+v (count descending, then kind; kinds with no row absent)", reg.Kinds, want)
	}
	for i := range want {
		if reg.Kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %+v, want %+v", i, reg.Kinds[i], want[i])
		}
	}

	empty, err := FailureRegisterFile(writeFailureLedger(t, `{"ts":1,"task":"agent_delegate","reason_code":"ok","job_id":"x","cards_tokens":0}`), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(empty)
	if empty.StackFaultShare != nil || !strings.Contains(string(b), `"stack_fault_share":null`) || !strings.Contains(string(b), `"kinds":[]`) {
		t.Errorf("a register with no failure row: stack_fault_share = %v in %s, want null and an empty kinds list", empty.StackFaultShare, b)
	}
}

// A line the register cannot decode is not a row, and silently dropping it makes a damaged ledger look like a
// quiet one: a line cut off mid-file (a crash during an append, a copy that stopped), garbage, a line that is not a
// JSON object, and a last line with no end are each counted in unparsed_lines. A blank line is not a line at all.
func TestFailureRegisterCountsTheLinesItCouldNotRead(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"a"}`,
		`this is not json`,
		`{"ts":2,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"b"`, // cut off mid-file
		``,
		"  	 ", // whitespace only: blank as well
		`[1,2,3]`,
		`{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"abstention","job_id":"c"}`,
	)
	// a last line the writer had not finished
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":4,"task":"agent_delegate","defer`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reg.LedgerFound {
		t.Errorf("ledger_found = false for a ledger that exists")
	}
	if reg.UnparsedLines != 4 {
		t.Errorf("unparsed_lines = %d, want 4 (garbage, a line cut mid-file, a non-object, the unfinished last line; the blank line is none)", reg.UnparsedLines)
	}
	if reg.JobRows != 2 || reg.FailureRows != 2 {
		t.Errorf("job_rows = %d, failure_rows = %d, want the 2 rows that decode", reg.JobRows, reg.FailureRows)
	}
	b, _ := json.Marshal(reg)
	if !strings.Contains(string(b), `"unparsed_lines":4`) || !strings.Contains(string(b), `"ledger_found":true`) {
		t.Errorf("the JSON lacks the two fields: %s", b)
	}

	clean, err := FailureRegisterFile(writeFailureLedger(t, `{"ts":1,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"a"}`), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if clean.UnparsedLines != 0 {
		t.Errorf("a clean ledger: unparsed_lines = %d, want 0", clean.UnparsedLines)
	}
}

// A ledger that is not there is not a ledger with no failures: the report says so (ledger_found false) beside its
// zeros, and still exits clean, so a script that reads failure_rows has the second field to check.
func TestFailureRegisterSaysWhenTheLedgerIsMissing(t *testing.T) {
	reg, err := FailureRegisterFile(filepath.Join(t.TempDir(), "nope.jsonl"), 0, false)
	if err != nil {
		t.Fatalf("a missing ledger is a report, not an error: %v", err)
	}
	if reg.LedgerFound || reg.UnparsedLines != 0 || reg.FailureRows != 0 {
		t.Errorf("missing ledger: %+v, want ledger_found false and nothing counted", reg)
	}
	b, _ := json.Marshal(reg)
	if !strings.Contains(string(b), `"ledger_found":false`) {
		t.Errorf("the JSON must carry ledger_found false, got %s", b)
	}
	// An empty ledger that exists is found.
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if reg, err := FailureRegisterFile(empty, 0, false); err != nil || !reg.LedgerFound {
		t.Errorf("an empty ledger that exists: found = %v, err = %v, want found", reg.LedgerFound, err)
	}
}

// model_tier is `<node>:<seat alias>` ONLY on a delegation row (task agent_delegate: its two writers join the node
// and the seat with a colon). On every other row it is the model alias the call used, and a model alias can hold a
// colon of its own (`family:size`, `engine:file`): cutting it at the last colon turned `gemma:4b` into model `4b`
// and `comfyui:model-y.safetensors` into the file name, and merged different models under one entry.
func TestFailureModelIsCutAtTheColonOnlyOnADelegationRow(t *testing.T) {
	path := writeFailureLedger(t,
		`{"ts":1,"task":"summarize","deferred":true,"reason_code":"budget","model_tier":"gemma:4b"}`,
		`{"ts":2,"task":"summarize","deferred":true,"reason_code":"budget","model_tier":"qwen:4b"}`,
		`{"ts":3,"task":"generate_image","deferred":true,"reason_code":"budget","model_tier":"comfyui:model-y.safetensors"}`,
		`{"ts":4,"task":"agent","deferred":true,"reason_code":"budget","model_tier":"pool:fast"}`,
		`{"ts":5,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":"node-a:agent-pool"}`,
		`{"ts":6,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":"node-b:agent-pool"}`,
		`{"ts":7,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":":no-node-seat"}`,
		`{"ts":8,"task":"agent_delegate","deferred":true,"reason_code":"budget","model_tier":"node-c:"}`,
		`{"ts":9,"task":"summarize","deferred":true,"reason_code":"budget","model_tier":":kept-as-recorded"}`, // only a delegation row's missing node is trimmed
	)
	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	c := classNamed(t, reg, "budget")
	count := func(fc []FailureCount) map[string]int {
		m := map[string]int{}
		for _, x := range fc {
			m[x.Name] = x.Count
		}
		return m
	}
	wantSeats := map[string]int{"gemma:4b": 1, "qwen:4b": 1, "comfyui:model-y.safetensors": 1, "pool:fast": 1,
		"node-a:agent-pool": 1, "node-b:agent-pool": 1, "no-node-seat": 1, "node-c:": 1, ":kept-as-recorded": 1}
	if got := count(c.Seats); !reflect.DeepEqual(got, wantSeats) {
		t.Errorf("seats = %v, want %v", got, wantSeats)
	}
	// Non-delegation rows keep the whole alias as the model; delegation rows give the part after the last colon,
	// and an empty one reads as unrecorded.
	wantModels := map[string]int{"gemma:4b": 1, "qwen:4b": 1, "comfyui:model-y.safetensors": 1, "pool:fast": 1,
		"agent-pool": 2, "no-node-seat": 1, unrecorded: 1, ":kept-as-recorded": 1}
	if got := count(c.Models); !reflect.DeepEqual(got, wantModels) {
		t.Errorf("models = %v, want %v", got, wantModels)
	}
}
