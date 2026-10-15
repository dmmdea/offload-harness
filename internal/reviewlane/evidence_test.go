package reviewlane

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

const evDiff = "diff --git a/run.go b/run.go\n--- a/run.go\n+++ b/run.go\n@@ -1,3 +1,4 @@\n+for i := 0; i <= len(xs); i++ {\n"

const (
	evBase = "0123456789abcdef0123456789abcdef01234567"
	evHead = "89abcdef0123456789abcdef0123456789abcdef"
)

// cleanSeat is the receipt of a seat that ran, was not deferred and named itself, with the
// second fingerprint check made and in agreement.
func cleanSeat(ev Evidence) Evidence {
	ev.Seat = &SeatReceipt{Seat: "fake-seat", Steps: 2, StopReason: "done"}
	ev.RecheckSHA256 = ev.DiffSHA256
	return ev
}

func mustEvidence(t *testing.T, diff string, handed ...GateReceipt) Evidence {
	t.Helper()
	ev, err := NewEvidence(diff, evBase, evHead, handed)
	if err != nil {
		t.Fatalf("NewEvidence: %v", err)
	}
	return ev
}

// ACCEPT: NOT_RUN exists, and it is what a missing clean seat produces. A run the lane could
// not turn into a review is never a PASS, whatever else is true of the diff.
func TestNoCleanSeatIsNotRunNeverPass(t *testing.T) {
	ev := mustEvidence(t, evDiff)
	cases := map[string]Verdict{
		"the node deferred (gpu busy)": Decide(ev, Delivery{Delivered: false, Reason: "gpu busy: card leased"}),
		"delivered with no receipt":    Decide(ev, Delivery{Delivered: true}),
		"receipt names no seat":        Decide(func() Evidence { e := ev; e.Seat = &SeatReceipt{Steps: 1}; return e }(), Delivery{Delivered: true}),
		"the seat contradicted itself": Decide(cleanSeat(ev), Delivery{Delivered: true, Findings: 0, Filtered: 3}),
		"the binding was refused":      Decide(func() Evidence { e := cleanSeat(ev); e.Refusal = "base_sha is not hex"; return e }(), Delivery{Delivered: true}),
	}
	for name, v := range cases {
		if v.Outcome != OutcomeNotRun {
			t.Errorf("%s: outcome = %q, want %q", name, v.Outcome, OutcomeNotRun)
		}
		if strings.TrimSpace(v.Reason) == "" {
			t.Errorf("%s: a NOT_RUN must say why", name)
		}
	}
	if v := Decide(ev, Delivery{Delivered: false, Reason: "gpu busy: card leased"}); !strings.Contains(v.Reason, "gpu busy") {
		t.Errorf("the node's own defer reason must survive into the verdict: %q", v.Reason)
	}
}

// ACCEPT: PASS is refused when the diff exceeds MaxDiffBytes, even for a seat that answered
// with a clean NONE. The boundary is inclusive: exactly MaxDiffBytes may still pass.
func TestPassRefusedWhenTheDiffIsOversize(t *testing.T) {
	big := evDiff + strings.Repeat("+x\n", (MaxDiffBytes-len(evDiff))/3+2)
	if len(big) <= MaxDiffBytes {
		t.Fatalf("fixture is not oversize: %d", len(big))
	}
	ev := cleanSeat(mustEvidence(t, big))
	if ev.DiffBytes != len(big) || ev.DiffSHA256 == "" {
		t.Fatalf("an oversize diff must still be fingerprinted so the NOT_RUN is bound to it: %+v", ev)
	}
	v := Decide(ev, Delivery{Delivered: true, Findings: 0})
	if v.Outcome == OutcomePass {
		t.Fatal("PASS on an oversize diff")
	}
	if v.Outcome != OutcomeNotRun || !strings.Contains(v.Reason, "exceeds") {
		t.Errorf("oversize must be NOT_RUN naming the ceiling, got %+v", v)
	}
	// Findings on an oversize diff are no more trustworthy than a clean answer.
	if v := Decide(ev, Delivery{Delivered: true, Findings: 2}); v.Outcome != OutcomeNotRun {
		t.Errorf("findings on an oversize diff: outcome = %q, want NOT_RUN", v.Outcome)
	}
	exact := evDiff + strings.Repeat("+", MaxDiffBytes-len(evDiff))
	if len(exact) != MaxDiffBytes {
		t.Fatalf("boundary fixture is %d bytes", len(exact))
	}
	if v := Decide(cleanSeat(mustEvidence(t, exact)), Delivery{Delivered: true}); v.Outcome != OutcomePass {
		t.Errorf("exactly MaxDiffBytes must still be reviewable: %+v", v)
	}
}

// ACCEPT: the fingerprint is the sha256 of the diff bytes and changes when the diff does.
func TestFingerprintChangesWhenTheDiffChanges(t *testing.T) {
	sum := sha256.Sum256([]byte(evDiff))
	if got, want := Fingerprint(evDiff), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("Fingerprint = %q, want sha256 of the bytes %q", got, want)
	}
	if Fingerprint(evDiff) != Fingerprint(evDiff) {
		t.Error("the fingerprint must be stable for the same bytes")
	}
	if Fingerprint(evDiff) == Fingerprint(evDiff+"+one more line\n") {
		t.Error("the fingerprint must change when the diff changes")
	}
	ev := mustEvidence(t, evDiff)
	if ev.DiffSHA256 != Fingerprint(evDiff) || ev.DiffBytes != len(evDiff) {
		t.Errorf("evidence must carry the fingerprint and size of what it was built from: %+v", ev)
	}
	if ev.BaseSHA != evBase || ev.HeadSHA != evHead {
		t.Errorf("evidence must carry base and head: %+v", ev)
	}
}

// ACCEPT (second check): a diff that changed while the seat reviewed makes the result STALE,
// never PASS and never FINDINGS about bytes nobody reviewed.
func TestARecheckThatDisagreesMakesTheResultStale(t *testing.T) {
	ev := cleanSeat(mustEvidence(t, evDiff))

	same := ev.Recheck(evDiff, nil)
	if same.Stale || same.RecheckSHA256 != ev.DiffSHA256 {
		t.Errorf("an unchanged diff must record the second fingerprint and stay fresh: %+v", same)
	}
	if v := Decide(same, Delivery{Delivered: true}); v.Outcome != OutcomePass {
		t.Errorf("a fresh, clean, gate-green review is a PASS: %+v", v)
	}

	changed := ev.Recheck(evDiff+"+extra\n", nil)
	if !changed.Stale || changed.RecheckSHA256 == ev.DiffSHA256 || changed.StaleReason == "" {
		t.Fatalf("a changed diff must be marked stale with the new fingerprint and a reason: %+v", changed)
	}
	for _, d := range []Delivery{{Delivered: true}, {Delivered: true, Findings: 3}} {
		if v := Decide(changed, d); v.Outcome != OutcomeStale {
			t.Errorf("stale evidence with %d findings: outcome = %q, want STALE", d.Findings, v.Outcome)
		}
	}

	gone := ev.Recheck("", errors.New("diff_path vanished"))
	if !gone.Stale || !strings.Contains(gone.StaleReason, "vanished") {
		t.Errorf("a diff that cannot be re-read cannot be shown unchanged: %+v", gone)
	}
}

// Deterministic gates attach to the evidence at construction, before any seat receipt exists,
// and a red one (or a receipt for another commit) stops PASS without stopping the review.
func TestGatesAttachBeforeTheSeatAndAFailedOneBlocksPass(t *testing.T) {
	ev := mustEvidence(t, evDiff,
		GateReceipt{Name: "go-test", Status: "pass", Detail: "ok", HeadSHA: evHead},
		GateReceipt{Name: "lint", Status: "FAIL", Detail: "2 findings", HeadSHA: evHead},
	)
	if ev.Seat != nil {
		t.Fatal("gates must be attached before any seat receipt exists")
	}
	names := []string{}
	for _, g := range ev.Gates {
		names = append(names, g.Name)
	}
	if got := strings.Join(names, ","); got != "diff-size,conflict-markers,go-test,lint" {
		t.Fatalf("the lane's own gates run first, handed receipts follow in order: %s", got)
	}
	if ev.Gates[2].Source != "caller" || ev.Gates[0].Source != "lane" {
		t.Errorf("receipts must say who produced them: %+v", ev.Gates)
	}
	if ev.GatesPassed() {
		t.Fatal("a failing lint receipt must stop GatesPassed")
	}
	v := Decide(cleanSeat(ev), Delivery{Delivered: true})
	if v.Outcome != OutcomeGateFailed || !strings.Contains(v.Reason, "lint") {
		t.Errorf("clean seat + red gate = GATE_FAILED naming the gate, got %+v", v)
	}
	// Findings are the lane's product and stay visible beside a red gate.
	if v := Decide(cleanSeat(ev), Delivery{Delivered: true, Findings: 1}); v.Outcome != OutcomeFindings {
		t.Errorf("findings beside a red gate: outcome = %q, want FINDINGS", v.Outcome)
	}
}

func TestAHandedReceiptForAnotherCommitCannotSupportPass(t *testing.T) {
	ev := mustEvidence(t, evDiff, GateReceipt{Name: "go-test", Status: "pass", HeadSHA: "fedcba9876543210fedcba9876543210fedcba98"})
	if !ev.Gates[len(ev.Gates)-1].Stale || ev.GatesPassed() {
		t.Fatalf("a receipt bound to a different head is stale evidence: %+v", ev.Gates)
	}
	if v := Decide(cleanSeat(ev), Delivery{Delivered: true}); v.Outcome == OutcomePass {
		t.Error("PASS on a gate receipt for a different commit")
	}
	// An abbreviated head still matches the full one it abbreviates.
	short := mustEvidence(t, evDiff, GateReceipt{Name: "go-test", Status: "pass", HeadSHA: evHead[:12]})
	if !short.GatesPassed() {
		t.Errorf("a prefix of the head is the same commit: %+v", short.Gates)
	}
	// With no head on the review the receipt cannot be shown to belong to the diff: it is
	// unbound, and says so.
	ev2, err := NewEvidence(evDiff, "", "", []GateReceipt{{Name: "go-test", Status: "pass", HeadSHA: evHead}})
	if err != nil || ev2.GatesPassed() || !ev2.Gates[len(ev2.Gates)-1].Unbound {
		t.Errorf("review with no head, receipt naming a commit: %v %+v", err, ev2.Gates)
	}
}

func TestTheLaneRunsItsOwnDeterministicGates(t *testing.T) {
	ok := mustEvidence(t, evDiff)
	if !ok.GatesPassed() || len(ok.Gates) != 2 {
		t.Fatalf("a clean in-bound diff passes both lane gates: %+v", ok.Gates)
	}
	marked := mustEvidence(t, evDiff+"+<<<<<<< HEAD\n+x := 1\n+=======\n+x := 2\n+>>>>>>> branch\n")
	if marked.GatesPassed() {
		t.Fatal("a conflict marker on an added line must fail the conflict-markers gate")
	}
	var detail string
	for _, g := range marked.Gates {
		if g.Name == "conflict-markers" && g.Status == "fail" {
			detail = g.Detail
		}
	}
	if !strings.Contains(detail, "line") {
		t.Errorf("the failing gate must point at the line: %q", detail)
	}
	// A removed line, a context line and a setext underline are not conflict markers.
	benign := mustEvidence(t, evDiff+"-<<<<<<< old\n >>>>>>> context\n+=======\n+<<<<<<<<< not seven exactly\n")
	if !benign.GatesPassed() {
		t.Errorf("removed/context lines and bare ======= are not markers: %+v", benign.Gates)
	}
	if v := Decide(cleanSeat(marked), Delivery{Delivered: true}); v.Outcome != OutcomeGateFailed {
		t.Errorf("a conflict-marked diff cannot PASS: %+v", v)
	}
}

func TestBadBindingsAreRefusedNamingTheField(t *testing.T) {
	cases := map[string]struct {
		base, head string
		handed     []GateReceipt
		want       string
	}{
		"base not hex":      {"not-a-sha", evHead, nil, "base_sha"},
		"head too short":    {evBase, "abc12", nil, "head_sha"},
		"unknown status":    {evBase, evHead, []GateReceipt{{Name: "t", Status: "maybe"}}, "status"},
		"receipt no name":   {evBase, evHead, []GateReceipt{{Status: "pass"}}, "name"},
		"too many receipts": {evBase, evHead, make17(), "receipts"},
	}
	for name, c := range cases {
		ev, err := NewEvidence(evDiff, c.base, c.head, c.handed)
		if !errors.Is(err, ErrBadBinding) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrBadBinding naming %q", name, err, c.want)
			continue
		}
		if ev.Refusal == "" || ev.DiffSHA256 == "" {
			t.Errorf("%s: a refused binding still fingerprints the diff and records why: %+v", name, ev)
		}
		if v := Decide(cleanSeat(ev), Delivery{Delivered: true}); v.Outcome != OutcomeNotRun {
			t.Errorf("%s: outcome = %q, want NOT_RUN", name, v.Outcome)
		}
	}
	// Upper-case hex is normalised, an empty pair is simply unbound.
	if ev, err := NewEvidence(evDiff, strings.ToUpper(evBase), "", nil); err != nil || ev.BaseSHA != evBase {
		t.Errorf("upper-case base: %v %+v", err, ev)
	}
}

func make17() []GateReceipt {
	out := make([]GateReceipt, MaxGateReceipts+1)
	for i := range out {
		out[i] = GateReceipt{Name: "g", Status: "pass"}
	}
	return out
}

func TestOutcomeLadder(t *testing.T) {
	ev := cleanSeat(mustEvidence(t, evDiff))
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome != OutcomePass || v.Reason != "" {
		t.Errorf("clean: %+v", v)
	}
	if v := Decide(ev, Delivery{Delivered: true, Findings: 2, Filtered: 1}); v.Outcome != OutcomeFindings {
		t.Errorf("findings (some lines filtered): %+v", v)
	}
}

func TestSeatReceiptNamesTheSeatAndHashesTheAnswerWithoutTiming(t *testing.T) {
	w := core.AgentWireResult{Seat: "qwen-27b", NodeID: "n1", Output: "NONE", Steps: 3, StopReason: "done", WallMs: 91234}
	r := SeatReceiptOf(w, map[string]any{"executed_on": "fleet", "node": "node-b"}, SalvagedWall)
	sum := sha256.Sum256([]byte("NONE"))
	if r.Seat != "qwen-27b" || r.NodeID != "n1" || r.Steps != 3 || r.StopReason != "done" || r.ExecutedOn != "fleet" || r.Salvaged != SalvagedWall {
		t.Errorf("receipt fields: %+v", r)
	}
	if r.AnswerSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("the receipt must bind the seat's raw answer by digest: %q", r.AnswerSHA256)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "91234") || strings.Contains(string(b), "wall_ms") {
		t.Errorf("a receipt is identity, not timing: %s", b)
	}
}

func TestEvidenceJSONIsAdditiveAndOmitsWhatIsUnknown(t *testing.T) {
	ev, err := NewEvidence(evDiff, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ev)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"base_sha", "head_sha", "seat", "recheck_sha256", "stale", "stale_reason", "refusal", "prompt_sha256"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be omitted while unknown: %s", k, b)
		}
	}
	for _, k := range []string{"diff_sha256", "diff_bytes", "gates"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%s missing: %s", k, b)
		}
	}
	// The pre-existing Finding wire shape is byte-identical.
	fb, _ := json.Marshal(Finding{Severity: "severe", File: "run.go", Line: 5, Claim: "c", Why: "w"})
	if string(fb) != `{"severity":"severe","file":"run.go","line":5,"claim":"c","why":"w"}` {
		t.Errorf("Finding wire shape changed: %s", fb)
	}
}

func readTrace(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("trace file: %v", err)
	}
	defer f.Close()
	var rows []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("trace line is not JSON: %v: %s", err, sc.Text())
		}
		rows = append(rows, m)
	}
	return rows
}

func sealedPayload(findings ...Finding) map[string]any {
	if findings == nil {
		findings = []Finding{}
	}
	return map[string]any{"findings": findings, "reviewed_bytes": len(evDiff), "seat": "fake-seat", "steps": 2}
}

// ACCEPT: an advisory finding leaves a durable trace row even when nobody reads or applies
// it; a clean and a NOT_RUN review leave their review row too.
func TestAnAdvisoryFindingAlwaysLeavesATraceRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", TraceFile)
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "severe | run.go:5 | off-by-one", Steps: 2, StopReason: "done"}
	out := sealedPayload(
		Finding{Severity: "severe", File: "run.go", Line: 5, Claim: "off-by-one in the loop bound", Why: "indexes one past the end"},
		Finding{Severity: "minor", File: "run.go", Line: 9, Claim: "naming", Also: []string{"cosmetic", "style"}},
	)
	out["dropped_ungrounded"] = 1
	got := Seal(out, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	if got["outcome"] != OutcomeFindings {
		t.Fatalf("outcome = %v", got["outcome"])
	}
	if got["trace_error"] != nil {
		t.Fatalf("trace_error = %v", got["trace_error"])
	}
	rows := readTrace(t, path)
	if len(rows) != 3 {
		t.Fatalf("want 1 review row + 2 finding rows, got %d: %v", len(rows), rows)
	}
	review, f1, f2 := rows[0], rows[1], rows[2]
	if review["kind"] != "review" || review["outcome"] != OutcomeFindings {
		t.Errorf("review row: %v", review)
	}
	counts, _ := review["counts"].(map[string]any)
	if counts["findings"].(float64) != 2 || counts["dropped_ungrounded"].(float64) != 1 {
		t.Errorf("the review row must count what was shown and what was dropped: %v", counts)
	}
	ev, _ := review["evidence"].(map[string]any)
	if ev["diff_sha256"] != Fingerprint(evDiff) || ev["head_sha"] != evHead {
		t.Errorf("the review row is SHA-bound: %v", ev)
	}
	for i, r := range []map[string]any{f1, f2} {
		if r["kind"] != "finding" || r["review_id"] != review["review_id"] || r["review_id"] == "" {
			t.Errorf("finding row %d must join its review: %v", i, r)
		}
	}
	fnd, _ := f1["finding"].(map[string]any)
	if fnd["severity"] != "severe" || fnd["file"] != "run.go" || fnd["line"].(float64) != 5 || fnd["claim"] != "off-by-one in the loop bound" || fnd["sha256"] == "" {
		t.Errorf("finding row content: %v", fnd)
	}
	if a, _ := f2["finding"].(map[string]any); a["also"].(float64) != 2 {
		t.Errorf("a folded finding records how many restatements rode on it: %v", a)
	}

	// A second review appends; it never rewrites the first.
	Seal(sealedPayload(), mustEvidence(t, evDiff), Run{Delivered: false}, path)
	rows = readTrace(t, path)
	if len(rows) != 4 || rows[3]["outcome"] != OutcomeNotRun || rows[3]["kind"] != "review" {
		t.Errorf("a NOT_RUN leaves its review row and nothing is rewritten: %v", rows)
	}
}

func TestSealStampsOutcomeEvidenceAndANotRunOnADeferral(t *testing.T) {
	path := filepath.Join(t.TempDir(), TraceFile)
	out := Seal(map[string]any{"deferred": true, "reason": "gpu busy: card leased"}, mustEvidence(t, evDiff), Run{}, path)
	if out["deferred"] != true {
		t.Error("the existing deferral shape must be left alone")
	}
	if out["outcome"] != OutcomeNotRun || !strings.Contains(out["outcome_reason"].(string), "gpu busy") {
		t.Errorf("a deferral is NOT_RUN with the node's reason: %v", out)
	}
	ev, ok := out["evidence"].(Evidence)
	if !ok || ev.DiffSHA256 != Fingerprint(evDiff) || len(ev.Gates) == 0 {
		t.Errorf("evidence (with gate receipts, ran without a seat) must ride the deferral: %#v", out["evidence"])
	}
	if ev.Seat != nil {
		t.Errorf("no seat ran, so no seat receipt: %+v", ev.Seat)
	}
}

// The second fingerprint check runs inside Seal, after the seat and right before the result
// is returned, so a diff that moves during the review cannot be sealed as reviewed.
func TestSealRechecksTheDiffRightBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), TraceFile)
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "NONE", Steps: 1, StopReason: "done"}
	calls := 0
	out := Seal(sealedPayload(), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire,
		Recheck: func() (string, error) { calls++; return evDiff + "+edited while the seat ran\n", nil }}, path)
	if calls != 1 {
		t.Fatalf("recheck ran %d times, want exactly once", calls)
	}
	if out["outcome"] != OutcomeStale {
		t.Fatalf("a diff edited during the review: outcome = %v", out["outcome"])
	}
	ev := out["evidence"].(Evidence)
	if !ev.Stale || ev.RecheckSHA256 == ev.DiffSHA256 || ev.Seat == nil || ev.Seat.AnswerSHA256 == "" {
		t.Errorf("evidence: %+v", ev)
	}
	if rows := readTrace(t, path); rows[0]["outcome"] != OutcomeStale {
		t.Errorf("the trace records the stale verdict: %v", rows[0])
	}

	// No seat ran: nothing was reviewed, so there is nothing to recheck.
	calls = 0
	Seal(map[string]any{"deferred": true, "reason": "x"}, mustEvidence(t, evDiff), Run{Recheck: func() (string, error) { calls++; return evDiff, nil }}, path)
	if calls != 0 {
		t.Error("a review that never reached a seat has no second check to make")
	}

	// A clean, unchanged review seals as PASS.
	clean := Seal(sealedPayload(), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire, Recheck: func() (string, error) { return evDiff, nil }}, path)
	if clean["outcome"] != OutcomePass {
		t.Errorf("clean + unchanged: outcome = %v", clean["outcome"])
	}
}

func TestSealSaysSoWhenTheTraceCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "x", Steps: 1, StopReason: "done"}
	out := Seal(sealedPayload(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: "c"}), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, filepath.Join(blocker, TraceFile))
	if s, _ := out["trace_error"].(string); s == "" {
		t.Error("a trace that could not be written must be loud, not silent")
	}
	if out["outcome"] != OutcomeFindings {
		t.Error("the advisory result itself is still delivered")
	}
	if got := Seal(sealedPayload(), mustEvidence(t, evDiff), Run{}, ""); got["trace_error"] == nil {
		t.Error("no trace path is also a loud failure")
	}
}

func TestTraceTextIsBoundedAndRuneSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), TraceFile)
	long := strings.Repeat("é", 2000)
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "x", Steps: 1, StopReason: "done"}
	Seal(sealedPayload(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: long, Why: long}), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	rows := readTrace(t, path)
	fnd := rows[1]["finding"].(map[string]any)
	for _, k := range []string{"claim", "why"} {
		s := fnd[k].(string)
		if len(s) > 512 || !strings.HasPrefix(long, s) {
			t.Errorf("%s must be cut to a bounded, valid UTF-8 prefix: %d bytes", k, len(s))
		}
	}
	if fnd["sha256"] == "" {
		t.Error("the digest covers the full text, so a cut claim is still identifiable")
	}
}

// A review refused before any diff was read is bound to nothing and says why. It is
// a NOT_RUN with the refusal as its reason, zero bytes, and no fingerprint (a fingerprint would
// bind the refusal to bytes nobody chose), and Seal gives it its trace row like any other.
func TestARefusalBeforeAnyDiffIsADiffLessNotRun(t *testing.T) {
	const reason = "pass diff OR diff_path, not both"
	ev := Refused(reason)
	if ev.Refusal != reason || ev.DiffBytes != 0 || ev.DiffSHA256 != "" || len(ev.Gates) != 0 {
		t.Fatalf("a diff-less refusal carries the reason and nothing about any diff: %+v", ev)
	}
	// Whatever else a caller hands Decide, a refusal wins: there is nothing to PASS.
	seated := ev
	seated.Seat = &SeatReceipt{Seat: "fake-seat", Steps: 1}
	for name, d := range map[string]Delivery{"no run": {}, "a delivered clean run": {Delivered: true}} {
		v := Decide(seated, d)
		if v.Outcome != OutcomeNotRun || v.Reason != reason {
			t.Errorf("%s: %+v, want NOT_RUN naming the refusal", name, v)
		}
	}

	path := filepath.Join(t.TempDir(), TraceFile)
	out := Seal(map[string]any{"deferred": true, "reason": reason}, ev, Run{}, path)
	if out["deferred"] != true || out["reason"] != reason {
		t.Errorf("the deferral shape must be kept: %v", out)
	}
	if out["outcome"] != OutcomeNotRun || out["outcome_reason"] != reason {
		t.Errorf("outcome = %v / %v, want NOT_RUN with the refusal reason", out["outcome"], out["outcome_reason"])
	}
	if out["trace_error"] != nil {
		t.Fatalf("trace_error = %v", out["trace_error"])
	}
	rows := readTrace(t, path)
	if len(rows) != 1 || rows[0]["kind"] != "review" || rows[0]["outcome"] != OutcomeNotRun {
		t.Fatalf("a refusal leaves its review row: %v", rows)
	}
	if e, _ := rows[0]["evidence"].(map[string]any); e["refusal"] != reason || e["diff_bytes"].(float64) != 0 || e["diff_sha256"] != nil {
		t.Errorf("the trace row's evidence: %v", e)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), `"diff_bytes":0`) || strings.Contains(string(b), "diff_sha256") {
		t.Errorf("a diff-less evidence publishes diff_bytes 0 and no fingerprint: %s", b)
	}
}

// A run the node filed as a deferral, which the lane read as a
// clean NONE, is NOT_RUN and never PASS. Findings salvaged from the same kind of run stay
// FINDINGS: that is what the salvage is for.
func TestANodeDeferredRunReadAsACleanNoneIsNotRunNeverPass(t *testing.T) {
	deferred := core.AgentWireResult{Seat: "fake-seat", Output: "NONE", Steps: 2, StopReason: "done",
		Deferred: true, SchemaMiss: true, DeferClass: core.DeferClassBudget, Reason: "structured re-pack skipped"}
	path := filepath.Join(t.TempDir(), TraceFile)

	out := sealedPayload()
	out["salvaged"] = SalvagedWall
	got := Seal(out, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &deferred}, path)
	if got["outcome"] != OutcomeNotRun {
		t.Fatalf("outcome = %v, want NOT_RUN for a deferred run read as clean", got["outcome"])
	}
	reason, _ := got["outcome_reason"].(string)
	if !strings.Contains(reason, "deferral") || !strings.Contains(reason, "never a PASS") {
		t.Errorf("the reason must say the node filed a deferral: %q", reason)
	}
	for _, banned := range []string{"wall", "clock", "timed out", "second"} {
		if strings.Contains(strings.ToLower(reason), banned) {
			t.Errorf("a verdict reason carries no timing term, found %q in %q", banned, reason)
		}
	}
	seat := got["evidence"].(Evidence).Seat
	if seat == nil || !seat.Deferred || seat.Salvaged != SalvagedWall {
		t.Errorf("the receipt records the attempt as it was: %+v", seat)
	}

	// The same deferred run with a surviving finding is a review with findings.
	withFinding := sealedPayload(Finding{Severity: "severe", File: "run.go", Line: 5, Claim: "c", Why: "w"})
	withFinding["salvaged"] = SalvagedWall
	if got := Seal(withFinding, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &deferred}, path); got["outcome"] != OutcomeFindings {
		t.Errorf("a salvaged deferral with findings: outcome = %v, want FINDINGS", got["outcome"])
	}

	// Decide reads the receipt itself, so a caller that builds its own flow cannot reach PASS.
	ev := mustEvidence(t, evDiff)
	ev.Seat = &SeatReceipt{Seat: "fake-seat", Steps: 2, Deferred: true}
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome != OutcomeNotRun {
		t.Errorf("Decide: %+v, want NOT_RUN", v)
	}
	ev.Seat.Deferred = false
	ev.RecheckSHA256 = ev.DiffSHA256
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome != OutcomePass {
		t.Errorf("the same receipt without the deferral is a PASS: %+v", v)
	}
}

// The seat receipt is pinned through Seal, not only through SeatReceiptOf: node, executed_on,
// the salvage flag and the deferral flag all have to arrive on the published evidence.
func TestSealPublishesTheSeatReceiptsProvenance(t *testing.T) {
	wire := core.AgentWireResult{Seat: "qwen-27b", NodeID: "id-1", Output: "NONE", Steps: 3, StopReason: "done", Deferred: true}
	out := sealedPayload()
	out["salvaged"] = SalvagedWall
	got := Seal(out, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire,
		Extra: map[string]any{"executed_on": "fleet", "node": "node-b"}}, filepath.Join(t.TempDir(), TraceFile))
	seat := got["evidence"].(Evidence).Seat
	if seat == nil {
		t.Fatal("no seat receipt")
	}
	if seat.Seat != "qwen-27b" || seat.NodeID != "id-1" || seat.Node != "node-b" || seat.ExecutedOn != "fleet" ||
		seat.Salvaged != SalvagedWall || !seat.Deferred || seat.Steps != 3 || seat.StopReason != "done" || seat.AnswerSHA256 != Fingerprint("NONE") {
		t.Errorf("seat receipt through Seal: %+v", seat)
	}
}

// The dropped_* counts of the payload become Delivery.Filtered inside Seal. A clean
// NONE beside lines the filters discarded is a seat that contradicted itself, never a PASS,
// whichever filter took them. The counts are Go ints, as the door publishes them.
func TestSealTurnsDroppedCountsIntoNotRun(t *testing.T) {
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "NONE", Steps: 2, StopReason: "done"}
	cases := map[string]struct {
		counts map[string]any
		sum    string
	}{
		"echo alone":          {map[string]any{"dropped_echo": 2}, "2 finding-shaped"},
		"hollow alone":        {map[string]any{"dropped_hollow": 3}, "3 finding-shaped"},
		"ungrounded alone":    {map[string]any{"dropped_ungrounded": 4}, "4 finding-shaped"},
		"all three, distinct": {map[string]any{"dropped_echo": 1, "dropped_hollow": 2, "dropped_ungrounded": 4}, "7 finding-shaped"},
	}
	for name, c := range cases {
		out := sealedPayload()
		for k, v := range c.counts {
			out[k] = v
		}
		got := Seal(out, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, filepath.Join(t.TempDir(), TraceFile))
		reason, _ := got["outcome_reason"].(string)
		if got["outcome"] != OutcomeNotRun || !strings.Contains(reason, c.sum) || !strings.Contains(reason, "discarded every one") {
			t.Errorf("%s: outcome %v, reason %q, want NOT_RUN naming %q", name, got["outcome"], reason, c.sum)
		}
	}
	// No drops, no findings: the seat's NONE stands and the same payload is a PASS.
	if got := Seal(sealedPayload(), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, filepath.Join(t.TempDir(), TraceFile)); got["outcome"] != OutcomePass {
		t.Errorf("a clean NONE with nothing dropped: outcome = %v", got["outcome"])
	}
	// A surviving finding means the filters did not empty the list.
	f := sealedPayload(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: "c", Why: "w"})
	f["dropped_hollow"] = 2
	if got := Seal(f, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, filepath.Join(t.TempDir(), TraceFile)); got["outcome"] != OutcomeFindings {
		t.Errorf("findings beside drops: outcome = %v", got["outcome"])
	}
}

// Once the review names a head commit, a handed gate receipt
// that names none is unbound, so it cannot support a PASS. The lane's own gates carry no commit
// and are never caught by this.
func TestAHandedReceiptWithNoCommitCannotSupportPassOnABoundReview(t *testing.T) {
	ev := mustEvidence(t, evDiff, GateReceipt{Name: "go-test", Status: "pass", Detail: "ok"})
	if ev.GatesPassed() {
		t.Fatalf("an unbound receipt cannot support a PASS on a review with a head: %+v", ev.Gates)
	}
	v := Decide(cleanSeat(ev), Delivery{Delivered: true})
	if v.Outcome != OutcomeGateFailed || !strings.Contains(v.Reason, "go-test") || !strings.Contains(v.Reason, "names no commit") {
		t.Errorf("clean seat + unbound receipt: %+v, want GATE_FAILED naming the receipt", v)
	}
	if v := Decide(cleanSeat(ev), Delivery{Delivered: true, Findings: 1}); v.Outcome != OutcomeFindings {
		t.Errorf("findings beside an unbound receipt stay FINDINGS: %+v", v)
	}
	if !ev.Gates[0].Passed() || !ev.Gates[1].Passed() {
		t.Errorf("the lane's own gates name no commit and must never be caught: %+v", ev.Gates[:2])
	}
	// A receipt that names the head (or an abbreviation of it) is bound.
	bound := mustEvidence(t, evDiff, GateReceipt{Name: "go-test", Status: "pass", HeadSHA: evHead})
	if !bound.GatesPassed() {
		t.Errorf("a receipt for the head is bound: %+v", bound.Gates)
	}
	// A review with no head has nothing to bind to: a receipt with none stands, and the PASS
	// says the review is tied to no commit.
	unbound, err := NewEvidence(evDiff, evBase, "", []GateReceipt{{Name: "go-test", Status: "pass"}})
	if err != nil || !unbound.GatesPassed() {
		t.Errorf("no head on the review, no commit on the receipt: %v %+v", err, unbound.Gates)
	}
	// A failing receipt with no commit still blocks PASS, and is named as red, not as unbound.
	red := mustEvidence(t, evDiff, GateReceipt{Name: "lint", Status: "fail"})
	if v := Decide(cleanSeat(red), Delivery{Delivered: true}); v.Outcome != OutcomeGateFailed || strings.Contains(v.Reason, "names no commit") {
		t.Errorf("a red receipt is named as red: %+v", v)
	}
}

// The trace sits beside the ledger, derived from the ledger's own path.
func TestTracePathSitsBesideTheLedger(t *testing.T) {
	dir := t.TempDir()
	if got, want := TracePathFor(filepath.Join(dir, "led", "ledger.jsonl"), filepath.Join(dir, "home")), filepath.Join(dir, "led", TraceFile); got != want {
		t.Errorf("TracePathFor = %q, want the ledger's directory: %q", got, want)
	}
	// No ledger path configured: the harness home, never the working directory.
	if got, want := TracePathFor("", filepath.Join(dir, "home")), filepath.Join(dir, "home", TraceFile); got != want {
		t.Errorf("TracePathFor(no ledger) = %q, want %q", got, want)
	}
}

// Boundaries that the first tests only approached from one side.
func TestConflictMarkerBoundaries(t *testing.T) {
	// The only marker is an added closing line: the opening marker is not what trips the gate.
	closing := mustEvidence(t, evDiff+"+>>>>>>> branch\n")
	if closing.GatesPassed() {
		t.Error("an added closing conflict marker must fail the conflict-markers gate")
	}
	bare := mustEvidence(t, evDiff+"+>>>>>>>\n")
	if bare.GatesPassed() {
		t.Error("a closing marker with nothing after it is still a marker")
	}
	for _, g := range closing.Gates {
		if g.Name == "conflict-markers" && !strings.Contains(g.Detail, "line 6") {
			t.Errorf("the gate must point at the marker's diff line: %q", g.Detail)
		}
	}
}

func TestReceiptAndCommitIDBounds(t *testing.T) {
	exactly := make([]GateReceipt, MaxGateReceipts)
	for i := range exactly {
		exactly[i] = GateReceipt{Name: "g", Status: "pass", HeadSHA: evHead}
	}
	if ev, err := NewEvidence(evDiff, evBase, evHead, exactly); err != nil || len(ev.Gates) != 2+MaxGateReceipts {
		t.Fatalf("exactly %d receipts are accepted: %v, %d gates", MaxGateReceipts, err, len(ev.Gates))
	}
	hex := func(n int) string { return strings.Repeat("a", n) }
	for _, n := range []int{7, 40, 64} {
		if ev, err := NewEvidence(evDiff, "", hex(n), nil); err != nil || ev.HeadSHA != hex(n) {
			t.Errorf("a %d-character commit id is valid: %v", n, err)
		}
	}
	for _, n := range []int{6, 65, 80} {
		if _, err := NewEvidence(evDiff, "", hex(n), nil); !errors.Is(err, ErrBadBinding) || !strings.Contains(err.Error(), "head_sha") {
			t.Errorf("a %d-character commit id is refused naming head_sha: %v", n, err)
		}
	}
}

// Two sealed reviews of the same diff are two reviews: their rows must not share a join key.
func TestReviewIDsAreDistinctForTheSameDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), TraceFile)
	wire := core.AgentWireResult{Seat: "fake-seat", Output: "NONE", Steps: 1, StopReason: "done"}
	for i := 0; i < 3; i++ {
		Seal(sealedPayload(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: "c", Why: "w"}), mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	}
	seen := map[string]bool{}
	rows := readTrace(t, path)
	for _, r := range rows {
		if r["kind"] == "review" {
			id, _ := r["review_id"].(string)
			if id == "" || seen[id] {
				t.Errorf("review_id %q is empty or reused", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 3 || len(rows) != 6 {
		t.Fatalf("want 3 distinct reviews over 6 rows, got %d ids over %d rows", len(seen), len(rows))
	}
	for i := 0; i < len(rows); i += 2 {
		if rows[i]["review_id"] != rows[i+1]["review_id"] {
			t.Errorf("a finding row joins its own review: %v / %v", rows[i], rows[i+1])
		}
	}
}

// The trace holds the seat's short statements about someone else's code: owner-only. POSIX file
// modes cannot be asserted on Windows, so this test is skipped there rather than passing vacuously.
func TestTraceFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), TraceFile)
	if err := AppendTrace(path, TraceRow{Kind: "review", ReviewID: "x", Outcome: OutcomeNotRun}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("trace file mode = %o, want 600", mode)
	}
}

// PASS is bound to a fingerprint: a result with no diff_sha256 is bound to nothing, so it is
// not a PASS however clean the seat was, and the second check has to agree with the first.
func TestPassNeedsAFingerprintAndAMatchingRecheck(t *testing.T) {
	wire := core.AgentWireResult{Seat: "s", Output: "NONE", Steps: 1, StopReason: "done"}
	path := filepath.Join(t.TempDir(), TraceFile)

	bare := Seal(map[string]any{"findings": []Finding{}}, Evidence{}, Run{Delivered: true, Wire: &wire}, path)
	if bare["outcome"] != OutcomeNotRun {
		t.Fatalf("no fingerprint, no gates, no recheck: outcome = %v, want NOT_RUN", bare["outcome"])
	}
	if why, _ := bare["outcome_reason"].(string); !strings.Contains(why, "fingerprint") {
		t.Errorf("the reason names the missing fingerprint: %q", why)
	}
	if v := Decide(Evidence{Seat: &SeatReceipt{Seat: "s"}}, Delivery{Delivered: true}); v.Outcome != OutcomeNotRun {
		t.Errorf("Decide on evidence with no fingerprint: %+v", v)
	}

	// A fingerprint but no second check: nothing shows the diff stayed put.
	ev := mustEvidence(t, evDiff)
	ev.Seat = &SeatReceipt{Seat: "s"}
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome != OutcomeNotRun || !strings.Contains(v.Reason, "second") {
		t.Errorf("a PASS candidate with no recheck fingerprint: %+v, want NOT_RUN naming the second check", v)
	}
	// A recheck that disagrees without the stale flag is no PASS either.
	ev.RecheckSHA256 = Fingerprint("other bytes")
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome == OutcomePass {
		t.Errorf("a recheck fingerprint that differs: %+v", v)
	}
	ev.RecheckSHA256 = ev.DiffSHA256
	if v := Decide(ev, Delivery{Delivered: true}); v.Outcome != OutcomePass {
		t.Errorf("a fingerprint rechecked and equal is a PASS: %+v", v)
	}

	// A caller that holds the diff in memory passes no Recheck: the recheck is then the
	// fingerprint itself, so the result is still a PASS and the evidence says what was compared.
	held := Seal(map[string]any{"findings": []Finding{}}, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	if held["outcome"] != OutcomePass {
		t.Fatalf("an in-memory diff with a clean seat: %v", held)
	}
	if got := held["evidence"].(Evidence); got.RecheckSHA256 != got.DiffSHA256 || got.RecheckSHA256 == "" {
		t.Errorf("recheck_sha256 equals diff_sha256 for a diff held in memory: %+v", got)
	}
}

// A review that names no head_sha ties its gates to no commit, and says so; a receipt that
// names a commit on such a review is unbound and cannot support a PASS.
func TestAReviewWithNoHeadIsMarkedAndItsReceiptsAreUnbound(t *testing.T) {
	wire := core.AgentWireResult{Seat: "s", Output: "NONE", Steps: 1, StopReason: "done"}
	path := filepath.Join(t.TempDir(), TraceFile)

	ev, err := NewEvidence(evDiff, "", "", []GateReceipt{{Name: "go-test", Status: "pass", HeadSHA: "deadbee"}})
	if err != nil {
		t.Fatal(err)
	}
	last := ev.Gates[len(ev.Gates)-1]
	if !last.Unbound || last.Passed() || ev.GatesPassed() {
		t.Fatalf("a receipt naming a commit on a review that names none is unbound: %+v", ev.Gates)
	}
	for _, g := range ev.Gates[:2] {
		if g.Unbound {
			t.Errorf("the lane's own gates name no commit and are never unbound: %+v", g)
		}
	}
	out := Seal(map[string]any{"findings": []Finding{}}, ev, Run{Delivered: true, Wire: &wire}, path)
	if out["outcome"] != OutcomeGateFailed {
		t.Fatalf("a receipt naming deadbee, a review with no head, a clean seat: outcome = %v, want GATE_FAILED", out["outcome"])
	}
	if why, _ := out["outcome_reason"].(string); !strings.Contains(why, "go-test") {
		t.Errorf("the reason names the unbound receipt: %q", why)
	}

	// With no receipt at all the PASS stands but is marked, in the evidence and in the reason.
	plain := Seal(map[string]any{"findings": []Finding{}}, mustEvidenceNoHead(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	if plain["outcome"] != OutcomePass {
		t.Fatalf("a clean seat and the lane's own gates: %v", plain["outcome"])
	}
	if got := plain["evidence"].(Evidence); !got.HeadUnbound {
		t.Errorf("evidence must say the review names no head: %+v", got)
	}
	if why, _ := plain["outcome_reason"].(string); why != "no head_sha: gates not tied to a commit" {
		t.Errorf("a PASS without a head says so: %q", why)
	}
	// A PASS on a review that names its head carries no such reason and no flag.
	bound := Seal(map[string]any{"findings": []Finding{}}, mustEvidence(t, evDiff), Run{Delivered: true, Wire: &wire}, path)
	if _, has := bound["outcome_reason"]; has || bound["evidence"].(Evidence).HeadUnbound {
		t.Errorf("a bound PASS is unmarked: %v", bound)
	}
}

func mustEvidenceNoHead(t *testing.T, diff string) Evidence {
	t.Helper()
	ev, err := NewEvidence(diff, "", "", nil)
	if err != nil {
		t.Fatalf("NewEvidence: %v", err)
	}
	return ev
}

// capBytes trims a trailing partial rune and nothing else: one invalid byte early in the
// string must not cut everything after it.
func TestCapBytesTrimsOnlyAPartialRuneAtTheEnd(t *testing.T) {
	s := "abc\xffdef" + strings.Repeat("x", 600)
	if got := capBytes(s, 256); len(got) != 256 || got != s[:256] {
		t.Errorf("an invalid byte early in the string cut the rest: kept %d bytes, want 256", len(got))
	}
	// A multi-byte rune straddling the limit is dropped whole.
	e := strings.Repeat("a", 255) + "é" + "tail"
	if got := capBytes(e, 256); got != strings.Repeat("a", 255) {
		t.Errorf("a rune cut by the limit is dropped whole: %q", got[len(got)-3:])
	}
	if got := capBytes(e, 257); got != strings.Repeat("a", 255)+"é" {
		t.Errorf("a rune that fits is kept: len %d", len(got))
	}
	if got := capBytes("short", 256); got != "short" {
		t.Errorf("a short string is returned as it is: %q", got)
	}
	if got := capBytes("héllo", 0); got != "" {
		t.Errorf("n = 0 keeps nothing: %q", got)
	}
	// A run of continuation bytes with no start byte is not walked back to the beginning.
	junk := "ab" + strings.Repeat("\x80", 20)
	if got := capBytes(junk, 10); len(got) < 7 {
		t.Errorf("a continuation run must not erase what precedes it: kept %d bytes", len(got))
	}
}
