// offload_review_diff's SHA-bound evidence (RSI-OH-4). The outcome rules and the trace live in
// internal/reviewlane and are tested there; what is pinned HERE is the front door's own jobs:
// bind every result to the diff bytes it judged, attach gate receipts before the seat runs,
// re-check the diff right before returning, mark every deferral NOT_RUN, and leave the trace
// where the ledger lives. The seat's prompt must stay the task and the diff and nothing else.

package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/reviewlane"
)

const (
	evBase = "0123456789abcdef0123456789abcdef01234567"
	evHead = "89abcdef0123456789abcdef0123456789abcdef"
)

// reviewCall runs one review through the handler and decodes the published payload.
func reviewCall(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, args)))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	return decodeResult(t, res)
}

func traceRowsOf(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	cfg := s.p.Cfg()
	return traceRowsAt(t, reviewlane.TracePathFor(cfg.LedgerPath, cfg.BaseDir()))
}

func traceRowsAt(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the review left no trace file: %v", err)
	}
	defer f.Close()
	var rows []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("trace line is not JSON: %v", err)
		}
		rows = append(rows, m)
	}
	return rows
}

func evidenceOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	ev, ok := m["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("the result carries no evidence block: %v", m)
	}
	return ev
}

func neverSeat(t *testing.T) func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the seat was contacted although the review was refused before the run")
		return core.AgentWireResult{}, errors.New("must not run")
	}
}

// A review is bound to the exact bytes it judged, to the commits the caller names, and to the
// seat that answered; the finding it published also reaches the durable trace.
func TestReviewDiffResultIsBoundToTheDiffAndLeavesATrace(t *testing.T) {
	var got core.AgentContract
	s := askTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		got = c
		return seatFindings("severe | run.go:5 | off-by-one in the loop bound | indexes one past the end"), nil
	})
	m := reviewCall(t, s, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
		"base_sha": evBase, "head_sha": evHead,
		"gate_receipts": []map[string]any{{"name": "go-test", "status": "pass", "detail": "ok", "head_sha": evHead}},
	})
	if m["outcome"] != reviewlane.OutcomeFindings {
		t.Fatalf("outcome = %v, want FINDINGS: %v", m["outcome"], m)
	}
	ev := evidenceOf(t, m)
	if ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) || int(ev["diff_bytes"].(float64)) != len(reviewDiff) {
		t.Errorf("evidence must carry the fingerprint and size of the reviewed diff: %v", ev)
	}
	if ev["base_sha"] != evBase || ev["head_sha"] != evHead {
		t.Errorf("base/head must be echoed: %v", ev)
	}
	if ev["prompt_sha256"] != reviewlane.Fingerprint(got.Goal) {
		t.Errorf("the seat's exact prompt must be bound by digest: %v", ev["prompt_sha256"])
	}
	seat, _ := ev["seat"].(map[string]any)
	if seat["seat"] != "fake-seat" || seat["answer_sha256"] == "" || seat["answer_sha256"] == nil {
		t.Errorf("seat receipt: %v", seat)
	}
	var gates []string
	for _, g := range ev["gates"].([]any) {
		gates = append(gates, g.(map[string]any)["name"].(string))
	}
	if strings.Join(gates, ",") != "diff-size,conflict-markers,go-test" {
		t.Errorf("the lane's gates run first, the handed receipt follows: %v", gates)
	}
	if m["trace_error"] != nil {
		t.Errorf("trace_error: %v", m["trace_error"])
	}
	rows := traceRowsOf(t, s)
	if len(rows) != 2 || rows[0]["kind"] != "review" || rows[1]["kind"] != "finding" {
		t.Fatalf("want a review row and the finding's row: %v", rows)
	}
	if f, _ := rows[1]["finding"].(map[string]any); f["file"] != "run.go" || f["line"].(float64) != 5 {
		t.Errorf("finding row: %v", rows[1])
	}
	// Gate receipts and commit ids are evidence about the result; the seat's prompt stays the
	// task and the diff.
	for _, leak := range []string{evBase, evHead, "go-test", "gate"} {
		if strings.Contains(got.Goal, leak) {
			t.Errorf("%q must not ride in the seat's prompt", leak)
		}
	}
	// The pre-existing fields are untouched.
	if m["seat"] != "fake-seat" || m["reviewed_bytes"].(float64) != float64(len(reviewDiff)) || m["findings"] == nil {
		t.Errorf("existing payload fields changed: %v", m)
	}
}

func TestReviewDiffACleanBoundReviewIsAPass(t *testing.T) {
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(), nil
	})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"})
	if m["outcome"] != reviewlane.OutcomePass || m["outcome_reason"] != nil {
		t.Fatalf("a clean seat, an in-bound diff and green gates is a PASS: %v", m)
	}
	ev := evidenceOf(t, m)
	if ev["base_sha"] != nil || ev["head_sha"] != nil {
		t.Errorf("unbound commits are omitted, not published empty: %v", ev)
	}
	if ev["recheck_sha256"] != ev["diff_sha256"] {
		t.Errorf("the second fingerprint check must have run and agreed: %v", ev)
	}
	if rows := traceRowsOf(t, s); len(rows) != 1 || rows[0]["outcome"] != reviewlane.OutcomePass {
		t.Errorf("a clean review leaves its review row: %v", rows)
	}
}

// Every deferral is a NOT_RUN: the existing deferral shape is kept and the outcome, the
// evidence and a trace row ride beside it.
func TestReviewDiffEveryDeferralIsNotRun(t *testing.T) {
	deferred := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "fake-seat", Deferred: true,
		Reason: "gpu busy: the card is leased", DeferClass: core.DeferClassCapacity}
	cases := map[string]func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error){
		"a node deferral": func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
			return deferred, nil
		},
		"a runner error": func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
			return core.AgentWireResult{}, errors.New("seat unreachable")
		},
		"an unearned empty list": func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
			return seatRaw(""), nil
		},
	}
	for name, run := range cases {
		s := askTestServer(t, run)
		m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"})
		if m["deferred"] != true {
			t.Errorf("%s: the deferral shape must be kept: %v", name, m)
		}
		if m["outcome"] != reviewlane.OutcomeNotRun || m["outcome_reason"] == nil {
			t.Errorf("%s: outcome = %v, want NOT_RUN with a reason", name, m["outcome"])
		}
		if ev := evidenceOf(t, m); ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) {
			t.Errorf("%s: a NOT_RUN is still bound to the diff: %v", name, ev)
		}
		if rows := traceRowsOf(t, s); len(rows) != 1 || rows[0]["outcome"] != reviewlane.OutcomeNotRun {
			t.Errorf("%s: a NOT_RUN leaves its trace row: %v", name, rows)
		}
	}
	// The node's own reason is not replaced.
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return deferred, nil
	})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t"})
	if r, _ := m["outcome_reason"].(string); !strings.Contains(r, "gpu busy") {
		t.Errorf("the node's reason must reach the outcome: %q", r)
	}
}

// An oversize diff is refused before any seat runs, as before, and the refusal is a NOT_RUN
// bound to the bytes that were too big. It is never a PASS.
func TestReviewDiffOversizeIsNotRunAndBoundToTheBytes(t *testing.T) {
	s := askTestServer(t, neverSeat(t))
	big := reviewDiff + strings.Repeat("+x\n", reviewlane.MaxDiffBytes/3+1)
	m := reviewCall(t, s, map[string]any{"diff": big, "task": "t"})
	if m["deferred"] != true || m["outcome"] != reviewlane.OutcomeNotRun {
		t.Fatalf("oversize: %v", m)
	}
	if r, _ := m["reason"].(string); !strings.Contains(r, "too large") {
		t.Errorf("the builder's own refusal text must survive: %q", r)
	}
	ev := evidenceOf(t, m)
	if ev["diff_sha256"] != reviewlane.Fingerprint(big) || int(ev["diff_bytes"].(float64)) != len(big) {
		t.Errorf("the refusal is bound to the oversize bytes: %v", ev)
	}
}

func TestReviewDiffARefusedBindingNeverReachesTheSeat(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"base not hex":     {"base_sha": "main"},
		"head not hex":     {"head_sha": "zzzzzzz"},
		"unknown status":   {"gate_receipts": []map[string]any{{"name": "lint", "status": "green"}}},
		"receipt unnamed":  {"gate_receipts": []map[string]any{{"status": "pass"}}},
		"too many receipt": {"gate_receipts": manyReceipts(reviewlane.MaxGateReceipts + 1)},
	} {
		s := askTestServer(t, neverSeat(t))
		args["diff"], args["task"] = reviewDiff, "t"
		m := reviewCall(t, s, args)
		if m["deferred"] != true || m["outcome"] != reviewlane.OutcomeNotRun {
			t.Errorf("%s: %v", name, m)
		}
		if ev := evidenceOf(t, m); ev["refusal"] == nil || ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) {
			t.Errorf("%s: the refusal is recorded beside the fingerprint: %v", name, ev)
		}
	}
}

func manyReceipts(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"name": "g", "status": "pass"}
	}
	return out
}

// A red gate does not stop the review, and does not let a clean answer be a PASS.
func TestReviewDiffARedGateBlocksPassButNotTheReview(t *testing.T) {
	ran := false
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		ran = true
		return seatFindings(), nil
	})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t",
		"gate_receipts": []map[string]any{{"name": "lint", "status": "fail", "detail": "2 findings"}}})
	if !ran {
		t.Error("the seat must still review: the lane is advisory and gates do not stop it")
	}
	if m["outcome"] != reviewlane.OutcomeGateFailed || !strings.Contains(m["outcome_reason"].(string), "lint") {
		t.Errorf("clean seat + red gate: %v", m)
	}
	if m["findings"] == nil {
		t.Error("the review's own payload is still published")
	}
}

// The second fingerprint check: the diff file is rewritten while the seat reads it. The
// findings are about bytes that no longer exist, so the result is STALE, never PASS.
func TestReviewDiffADiffThatChangesDuringTheReviewIsStale(t *testing.T) {
	dir := t.TempDir()
	diffFile := filepath.Join(dir, "change.diff")
	if err := os.WriteFile(diffFile, []byte(reviewDiff), 0o600); err != nil {
		t.Fatal(err)
	}
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		if err := os.WriteFile(diffFile, []byte(reviewDiff+"+an edit made while the seat was reviewing\n"), 0o600); err != nil {
			t.Error(err)
		}
		return seatFindings(), nil
	})
	m := reviewCall(t, s, map[string]any{"diff_path": "change.diff", "read_root": dir, "task": "t"})
	if m["outcome"] != reviewlane.OutcomeStale {
		t.Fatalf("outcome = %v, want STALE: %v", m["outcome"], m)
	}
	ev := evidenceOf(t, m)
	if ev["stale"] != true || ev["recheck_sha256"] == ev["diff_sha256"] || ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) {
		t.Errorf("evidence must show both fingerprints: %v", ev)
	}

	// The same file, untouched, seals as PASS: the check is not just always-stale.
	if err := os.WriteFile(diffFile, []byte(reviewDiff), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(), nil
	})
	if m2 := reviewCall(t, s2, map[string]any{"diff_path": "change.diff", "read_root": dir, "task": "t"}); m2["outcome"] != reviewlane.OutcomePass {
		t.Errorf("an unchanged diff_path review: %v", m2["outcome"])
	}

	// A diff file deleted mid-review cannot be shown unchanged.
	s3 := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		_ = os.Remove(diffFile)
		return seatFindings(), nil
	})
	if m3 := reviewCall(t, s3, map[string]any{"diff_path": "change.diff", "read_root": dir, "task": "t"}); m3["outcome"] != reviewlane.OutcomeStale {
		t.Errorf("a vanished diff_path: %v", m3["outcome"])
	}
}

// The advertised shape must match what the door now publishes and accepts.
func TestReviewDiffAdvertisesTheEvidenceFields(t *testing.T) {
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_review_diff" {
			continue
		}
		schema, _ := json.Marshal(tool.InputSchema)
		for _, want := range []string{"base_sha", "head_sha", "gate_receipts"} {
			if !strings.Contains(string(schema), want) {
				t.Errorf("input schema must offer %s", want)
			}
		}
		for _, want := range []string{"outcome", "evidence", "NOT_RUN", "STALE"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("description must name %q so a lead can read the new fields", want)
			}
		}
		return
	}
	t.Fatal("offload_review_diff not advertised")
}

// evidenceServer is a review server whose ledger sits at ledgerPath, which may be nowhere near
// the harness home, so a trace path derived from the wrong one shows.
func evidenceServer(t *testing.T, home, ledgerPath string, local delegate.LocalRunner) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = ledgerPath
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

// RSI-OH-4#0: every early exit of the handler is sealed as a diff-less NOT_RUN. The refusal
// reason is the outcome reason, the evidence binds to no bytes (zero diff_bytes, no fingerprint),
// the seat is never contacted, and each refusal leaves its trace row.
func TestReviewDiffEveryEarlyRefusalIsASealedNotRun(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.diff"), []byte(strings.Repeat("+x\n", 50000)), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(elsewhere, "outside.diff")
	if err := os.WriteFile(outside, []byte(reviewDiff), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, args, wantReason string }{
		{"both diff and diff_path", reviewArgs(t, map[string]any{"diff": reviewDiff, "diff_path": "x.diff", "read_root": root, "task": "t"}), "not both"},
		{"neither", reviewArgs(t, map[string]any{"task": "t"}), "is required"},
		{"a whitespace-only diff", reviewArgs(t, map[string]any{"diff": " \n ", "task": "t"}), "is required"},
		{"a diff_path that does not exist", reviewArgs(t, map[string]any{"diff_path": "nope.diff", "read_root": root, "task": "t"}), "nope.diff"},
		{"a diff_path over the file cap", reviewArgs(t, map[string]any{"diff_path": "big.diff", "read_root": root, "task": "t"}), "cap"},
		{"a diff_path outside read_root", reviewArgs(t, map[string]any{"diff_path": outside, "read_root": root, "task": "t"}), "outside read_root"},
		{"arguments that do not decode", `{"diff":123,"task":"t"}`, "bad arguments: "},
	}
	for _, c := range cases {
		s := askTestServer(t, neverSeat(t))
		res, err := s.handleReviewDiff(context.Background(), callReq(c.args))
		if err != nil {
			t.Fatalf("%s: handleReviewDiff: %v", c.name, err)
		}
		m := decodeResult(t, res)
		reason, _ := m["reason"].(string)
		if m["deferred"] != true || !strings.Contains(reason, c.wantReason) {
			t.Errorf("%s: the refusal keeps its deferral shape and reason (want %q): %v", c.name, c.wantReason, m)
			continue
		}
		if m["outcome"] != reviewlane.OutcomeNotRun || m["outcome_reason"] != reason {
			t.Errorf("%s: outcome = %v / %v, want NOT_RUN carrying the refusal reason", c.name, m["outcome"], m["outcome_reason"])
		}
		ev := evidenceOf(t, m)
		if ev["refusal"] != reason || ev["diff_bytes"] != float64(0) || ev["diff_sha256"] != nil {
			t.Errorf("%s: the evidence binds to no bytes and names the refusal: %v", c.name, ev)
		}
		if m["trace_error"] != nil {
			t.Errorf("%s: trace_error = %v", c.name, m["trace_error"])
		}
		if rows := traceRowsOf(t, s); len(rows) != 1 || rows[0]["kind"] != "review" || rows[0]["outcome"] != reviewlane.OutcomeNotRun {
			t.Errorf("%s: a refusal leaves its review row: %v", c.name, rows)
		}
	}
}

// RSI-OH-4#6: the trace is written beside the ledger, derived from the ledger's own path, so it
// follows an operator who placed the ledger away from the harness home; with no ledger path it
// is the harness home.
func TestReviewDiffTraceSitsBesideTheConfiguredLedger(t *testing.T) {
	clean := func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(), nil
	}
	home := t.TempDir()
	ledgerDir := filepath.Join(t.TempDir(), "ledgers")
	s := evidenceServer(t, home, filepath.Join(ledgerDir, "ledger.jsonl"), clean)
	if m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t"}); m["trace_error"] != nil {
		t.Fatalf("trace_error: %v", m["trace_error"])
	}
	if rows := traceRowsAt(t, filepath.Join(ledgerDir, reviewlane.TraceFile)); len(rows) != 1 {
		t.Errorf("the trace must sit in the ledger's directory: %v", rows)
	}
	if _, err := os.Stat(filepath.Join(home, reviewlane.TraceFile)); err == nil {
		t.Error("the trace must not be written to the harness home when the ledger is elsewhere")
	}

	home2 := t.TempDir()
	s2 := evidenceServer(t, home2, "", clean)
	if m := reviewCall(t, s2, map[string]any{"diff": reviewDiff, "task": "t"}); m["trace_error"] != nil {
		t.Fatalf("trace_error: %v", m["trace_error"])
	}
	if rows := traceRowsAt(t, filepath.Join(home2, reviewlane.TraceFile)); len(rows) != 1 {
		t.Errorf("with no ledger path the trace sits in the harness home: %v", rows)
	}
}

// RSI-OH-4#5: once the review names its head, a receipt that names no commit cannot support PASS.
func TestReviewDiffAnUnboundGateReceiptCannotSupportPass(t *testing.T) {
	clean := func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(), nil
	}
	s := askTestServer(t, clean)
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t", "head_sha": evHead,
		"gate_receipts": []map[string]any{{"name": "go-test", "status": "pass"}}})
	reason, _ := m["outcome_reason"].(string)
	if m["outcome"] != reviewlane.OutcomeGateFailed || !strings.Contains(reason, "go-test") || !strings.Contains(reason, "names no commit") {
		t.Fatalf("a clean seat beside an unbound receipt on a bound review is GATE_FAILED: %v", m)
	}
	gates := evidenceOf(t, m)["gates"].([]any)
	if last := gates[len(gates)-1].(map[string]any); last["unbound"] != true {
		t.Errorf("the receipt is published as unbound: %v", last)
	}
	// The same receipt naming the head is bound and supports the PASS.
	s2 := askTestServer(t, clean)
	m2 := reviewCall(t, s2, map[string]any{"diff": reviewDiff, "task": "t", "head_sha": evHead,
		"gate_receipts": []map[string]any{{"name": "go-test", "status": "pass", "head_sha": evHead}}})
	if m2["outcome"] != reviewlane.OutcomePass {
		t.Errorf("a receipt for the head: %v", m2["outcome"])
	}
}

// deferredCleanWire is a run the node filed as a deferral (the structured re-pack was skipped for
// want of time) whose raw answer is a clean NONE: the lane salvages it as a review with no findings.
func deferredCleanWire() core.AgentWireResult {
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "fake-seat", Output: "NONE", Steps: 2, StopReason: "done",
		Deferred: true, SchemaMiss: true, DeferClass: core.DeferClassBudget, Reason: "structured re-pack skipped: nothing left of the budget"}
}

// RSI-OH-4#3 (conductor decision): a node-deferred run the lane salvages as a clean NONE is
// published as before, but its outcome is NOT_RUN and never PASS; the receipt records the attempt.
func TestReviewDiffANodeDeferredCleanNoneIsNotRunNeverPass(t *testing.T) {
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return deferredCleanWire(), nil
	})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t"})
	if m["salvaged"] != "wall" {
		t.Fatalf("fixture: the deferral must be salvaged as a published review: %v", m)
	}
	if m["outcome"] != reviewlane.OutcomeNotRun || m["outcome_reason"] == nil {
		t.Fatalf("outcome = %v / %v, want NOT_RUN with a reason", m["outcome"], m["outcome_reason"])
	}
	seat, _ := evidenceOf(t, m)["seat"].(map[string]any)
	if seat["deferred"] != true || seat["salvaged"] != "wall" || seat["seat"] != "fake-seat" {
		t.Errorf("the seat receipt records the attempt: %v", seat)
	}
	if rows := traceRowsOf(t, s); len(rows) != 1 || rows[0]["outcome"] != reviewlane.OutcomeNotRun {
		t.Errorf("trace: %v", rows)
	}
}
