package reviewlane

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

const evDiff = "diff --git a/run.go b/run.go\n--- a/run.go\n+++ b/run.go\n@@ -1,3 +1,4 @@\n+for i := 0; i <= len(xs); i++ {\n"

const (
	evBase = "0123456789abcdef0123456789abcdef01234567"
	evHead = "89abcdef0123456789abcdef0123456789abcdef"
)

// cleanSeat is the receipt of a seat that ran, was not deferred and named itself.
func cleanSeat(ev Evidence) Evidence {
	ev.Seat = &SeatReceipt{Seat: "fake-seat", Steps: 2, StopReason: "done"}
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
	// With no head on the review there is nothing to compare, and the receipt stands.
	ev2, err := NewEvidence(evDiff, "", "", []GateReceipt{{Name: "go-test", Status: "pass", HeadSHA: evHead}})
	if err != nil || !ev2.GatesPassed() {
		t.Errorf("unbound review, handed receipt: %v %+v", err, ev2.Gates)
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
