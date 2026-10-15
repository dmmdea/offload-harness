package delegate

// A node's capacity refusal at dispatch is a place in line, and the rows must say so.
//
// A fan-out meets a full node, the node answers 503 "queue full", the delegator outwaits it and the subtask
// lands elsewhere a few seconds later. The refused attempt wrote a corpus row like any other: deferred false,
// acceptance_pass false, an error, no marker. The rigger read it as a failed subtask on the serving stack (13 of
// them in one day, "fix the serving stack", when no seat was asked), and when the wait then ended as a capacity
// defer, that refusal row was the ONLY record of a subtask that really was lost: settle() wrote none "because the
// last attempt's row stands", and the defer carried the refused attempt's job id.
//
// The rows are read back as the JSON a delegator wrote (the wire shape the rigger reads), not through the writer's
// own struct, so the tests pin the contract between the two.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/rig"
)

// wireRows reads the corpus back as generic JSON objects, in write order.
func wireRows(t *testing.T, cfg config.Config) []map[string]any {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir(), "delegation-log")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("delegation-log: %v", err)
	}
	var out []map[string]any
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("delegation-log line: %v (%s)", err, l)
			}
			out = append(out, m)
		}
	}
	return out
}

// asRigRows decodes the corpus the way the rigger does and names the seat the rows carry.
func asRigRows(t *testing.T, cfg config.Config) ([]rig.Row, string) {
	t.Helper()
	var rows []rig.Row
	seat := ""
	for _, m := range wireRows(t, cfg) {
		b, _ := json.Marshal(m)
		var r rig.Row
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if seat == "" {
			seat = r.Seat
		}
		rows = append(rows, r)
	}
	return rows, seat
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// TestARefusedDispatchInsideTheWaitIsANonTerminalRow: the node refuses twice (503 with a one-second Retry-After) and takes the
// third dispatch. The two refused attempts are marked as refusals, the landing is not, and the rigger counts the
// refusals on their own line and finds no bad row.
func TestARefusedDispatchInsideTheWaitIsANonTerminalRow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := acceptingNode(t, "node-busy", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = freesAfter(2, http.StatusServiceUnavailable)
		f.dispatchRetryAfter = "1"
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || results[0].Result.Deferred {
		t.Fatalf("summary %+v result %+v, want the subtask landed", sum, results[0])
	}

	var refused, landed int
	for _, m := range wireRows(t, cfg) {
		ref, marked := m["refusal"].(map[string]any)
		switch {
		case strings.Contains(str(m, "error"), "status 503"):
			refused++
			if !marked {
				t.Errorf("a 503 refusal inside the wait is a bare error row, want a refusal marker: %v", m)
				continue
			}
			if st, _ := ref["status"].(float64); st != 503 {
				t.Errorf("refusal marker %v, want status 503", ref)
			}
			if ra, _ := ref["retry_after_sec"].(float64); ra != 1 {
				t.Errorf("refusal marker %v, want the node's Retry-After (1)", ref)
			}
		default:
			landed++
			if marked {
				t.Errorf("the row of the attempt that landed carries a refusal marker: %v", m)
			}
		}
	}
	if refused < 1 || landed != 1 {
		t.Fatalf("%d refused row(s) and %d landed row(s), want at least one refusal and exactly one landing", refused, landed)
	}

	rows, seat := asRigRows(t, cfg)
	rep, err := rig.Build(rows, seat, "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bad != 0 || rep.Rows != 1 || rep.Refusals != refused {
		t.Fatalf("rigger: rows %d bad %d refusals %d, want 1 landed row, none bad, and the %d refusals on their own line", rep.Rows, rep.Bad, rep.Refusals, refused)
	}
}

// TestAWaitThatEndsAsACapacityDeferAfterARefusalFilesATerminalRow: the node never frees. The wait ends on its TTL as
// a capacity defer, and the record of that loss must be a row that says so: a fresh job id (the refused attempt's
// row is a refusal, not the outcome), deferred, class capacity, no refusal marker; the ledger agrees.
func TestAWaitThatEndsAsACapacityDeferAfterARefusalFilesATerminalRow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	withCallReserve(t, 700*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, _, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil, nil)
	pr := results[0]
	if r := pr.Result; !r.Deferred || r.DeferClass != core.DeferClassCapacity || pr.deadlineCut {
		t.Fatalf("result %+v, want the capacity wait's own defer", r)
	}
	if pr.JobID == "" {
		t.Fatalf("the defer carries no job id")
	}

	var terminal, refusals int
	for _, m := range wireRows(t, cfg) {
		_, marked := m["refusal"].(map[string]any)
		if marked {
			refusals++
			if str(m, "job_id") == pr.JobID {
				t.Errorf("the published job id %q is the refused attempt's: two meanings for one id", pr.JobID)
			}
			continue
		}
		if str(m, "job_id") == pr.JobID {
			terminal++
			if d, _ := m["deferred"].(bool); !d || str(m, "defer_class") != core.DeferClassCapacity {
				t.Errorf("the terminal row = %v, want deferred, class capacity", m)
			}
		}
	}
	if refusals < 1 || terminal != 1 {
		t.Fatalf("%d refusal row(s) and %d terminal row(s) under the published id, want at least one refusal and exactly one terminal row", refusals, terminal)
	}

	led, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var closing int
	for _, row := range led {
		if row.JobID == pr.JobID {
			closing++
			if !row.Deferred || row.ReasonCode != ledger.ReasonCapacityWait {
				t.Errorf("ledger row for the published id = deferred %v code %q, want the capacity wait's", row.Deferred, row.ReasonCode)
			}
		}
	}
	if closing != 1 {
		t.Fatalf("%d ledger row(s) carry the published id, want exactly 1", closing)
	}

	// The capacity defer is the delegator's own outcome, attributed to its own box; the refusals are the node's.
	// The rigger reads each seat's rows on their own.
	rows, _ := asRigRows(t, cfg)
	seats := map[string]bool{}
	for _, r := range rows {
		seats[r.Seat] = true
	}
	if len(seats) != 2 {
		t.Fatalf("seats %v, want the node that refused and the box that deferred", seats)
	}
	var lost, refused int
	for seat := range seats {
		rep, err := rig.Build(rows, seat, "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		lost += rep.Bad
		refused += rep.Refusals
		for _, a := range rep.Axes {
			if a.Hits > 0 && a.Axis != rig.AxisCapacity {
				t.Errorf("seat %s: axis %s has %d hit(s): a wait that ran out is the capacity axis, nothing else", seat, a.Axis, a.Hits)
			}
		}
	}
	if lost != 1 || refused != refusals {
		t.Fatalf("rigger: %d bad row(s) and %d refusal(s), want the one lost subtask bad and the %d refusals beside it", lost, refused, refusals)
	}
}

// TestAPlacementRefusedByEveryNodeFilesATerminalRow: the capacity wait is off, the only node answers 503, and the
// subtask ends as the "placement refused" failure. The refused attempt's row is a refusal; the failure the caller
// is handed has a row of its own under the id it was given.
func TestAPlacementRefusedByEveryNodeFilesATerminalRow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t) // the wait is off

	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Failed != 1 || !strings.HasPrefix(pr.Err, replacementExhaustedPrefix) || pr.JobID == "" {
		t.Fatalf("summary %+v result %+v, want the placement-refused failure with a job id", sum, pr)
	}
	var refusals, terminal int
	for _, m := range wireRows(t, cfg) {
		if _, marked := m["refusal"].(map[string]any); marked {
			refusals++
			if str(m, "job_id") == pr.JobID {
				t.Errorf("the failure carries the refused attempt's job id %q", pr.JobID)
			}
			continue
		}
		if str(m, "job_id") == pr.JobID {
			terminal++
			if !strings.HasPrefix(str(m, "error"), replacementExhaustedPrefix) {
				t.Errorf("the terminal row's error = %q, want the placement-refused failure", str(m, "error"))
			}
		}
	}
	if refusals != 1 || terminal != 1 {
		t.Fatalf("%d refusal row(s) and %d terminal row(s), want 1 and 1", refusals, terminal)
	}
}

// TestAJobTheNodeAckedAndTookBackIsNotARefusalRow: the 503 the delegator files for a withdrawn or never-run job is
// not the node's answer to a dispatch. The node held that job under its id, the intent ledger closed it, and the
// published result of a subtask nobody else took must keep that id (TestWithdrawAgainstTheRealNodeHandler).
func TestAJobTheNodeAckedAndTookBackIsNotARefusalRow(t *testing.T) {
	refused := PlacedResult{refused: true, refusalStatus: http.StatusServiceUnavailable, Err: "dispatch: status 503: queue full"}
	if !isRefusalRow(refused) {
		t.Fatalf("a dispatch the node answered 503 is not a refusal row")
	}
	for name, mark := range map[string]func(*PlacedResult){
		"withdrawn":  func(pr *PlacedResult) { pr.refuseAsWithdrawn(time.Second) },
		"never ran":  func(pr *PlacedResult) { pr.refuseAsNeverRan("reaped: not started", time.Second) },
		"not a 503":  func(pr *PlacedResult) { pr.refusalStatus = http.StatusNotFound },
		"no error":   func(pr *PlacedResult) { pr.Err = "" },
		"not refuse": func(pr *PlacedResult) { pr.refused = false },
	} {
		pr := refused
		mark(&pr)
		if isRefusalRow(pr) {
			t.Errorf("%s: classified as a refusal row", name)
		}
	}
}

// TestAnAttemptThatLandedClearsTheRefusalMark: the mark is about the LATEST attempt's row. A subtask refused once and
// then taken has its answer's row to stand for it, so a later wait of the same subtask (a verification retry that
// finds no seat) is not owed a closing row on account of the old refusal.
func TestAnAttemptThatLandedClearsTheRefusalMark(t *testing.T) {
	pl := newPlacements()
	pl.countAttempt(PlacedResult{refusalRow: true})
	if !pl.lastRowRefusal || pl.attempts != 1 {
		t.Fatalf("after a refusal: lastRowRefusal %v attempts %d, want the mark set and one attempt", pl.lastRowRefusal, pl.attempts)
	}
	pl.countAttempt(PlacedResult{})
	if pl.lastRowRefusal || pl.attempts != 2 {
		t.Fatalf("after a landing: lastRowRefusal %v attempts %d, want the mark cleared and two attempts", pl.lastRowRefusal, pl.attempts)
	}
}

// ledgerWire reads the ledger back as generic JSON objects, in write order: the keys a delegator wrote, not the
// ledger package's own struct, so a reader that predates a column sees exactly what was on disk.
func ledgerWire(t *testing.T, cfg config.Config) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(cfg.LedgerPath)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("ledger line: %v (%s)", err, l)
		}
		out = append(out, m)
	}
	return out
}

// TestARefusedAttemptsLedgerRowCarriesTheRefusalMark: the delegation log marks a refused dispatch as a place in line,
// and the ledger row the same attempt writes must say it too. Without the column a ledger reader (the failure
// register) counts the refusal as a failed job of its own beside the subtask's real outcome: one subtask that
// landed elsewhere read as two failures. The row of the attempt that landed carries no mark, and neither does
// a dispatch marker or any row the log did not mark.
func TestARefusedAttemptsLedgerRowCarriesTheRefusalMark(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := acceptingNode(t, "node-busy", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = freesAfter(2, http.StatusServiceUnavailable)
		f.dispatchRetryAfter = "1"
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	if _, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}); err != nil || sum.Succeeded != 1 {
		t.Fatalf("summary %+v err %v, want the subtask landed", sum, err)
	}

	marked := map[string]bool{}
	for _, m := range wireRows(t, cfg) {
		if _, ok := m["refusal"].(map[string]any); ok {
			marked[str(m, "job_id")] = true
		}
	}
	if len(marked) < 1 {
		t.Fatalf("the corpus marked no refusal: the scenario did not refuse anything")
	}
	var refusedRows, otherRows int
	for _, m := range ledgerWire(t, cfg) {
		if str(m, "task") != "agent_delegate" || str(m, "phase") != "" {
			continue
		}
		ref, has := m["refusal"]
		if marked[str(m, "job_id")] {
			refusedRows++
			if b, _ := ref.(bool); !has || !b {
				t.Errorf("the ledger row of refused attempt %q has refusal = %v (present %v), want true: %v", str(m, "job_id"), ref, has, m)
			}
			continue
		}
		otherRows++
		if has {
			t.Errorf("a ledger row of an attempt the corpus did not mark as a refusal carries refusal = %v: %v", ref, m)
		}
	}
	if refusedRows != len(marked) || otherRows != 1 {
		t.Fatalf("%d refused ledger row(s) for %d marked attempt(s) and %d other row(s), want one row per refused attempt and exactly one landing", refusedRows, len(marked), otherRows)
	}
}
