package delegate

// What a subtask the call deadline cuts keeps and what it says (ADR 0065, register
// C-67). The cut used to REPLACE the outcome with a fresh wire: a run that had
// generated for minutes, and said what stopped it, was published, ledgered and
// stored in the corpus with zero steps, zero tokens and none of its own words. The
// longest runs — the ones the deadline exists to cut, and the ones the wall sizing and
// the rigger most need measured — were the ones that lost their measurements.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// measuredWallCancel is what the real agent loop publishes when the caller's deadline
// ends a run that has been working: a budget defer naming the wall, carrying everything
// the run measured before it was stopped (pipeline/agenttask.go fills these fields
// before every defer branch on purpose).
func measuredWallCancel() core.AgentWireResult {
	w := cancelledLoop()
	w.Reason = "wall timeout after 300s (the caller's deadline, not this node's ceiling)"
	w.Steps, w.TokensOut, w.SeatTokensIn = 9, 4321, 812
	w.StopReason = "canceled"
	w.SeatTokS = 12.5
	w.Trace = []core.AgentTraceStep{{Step: 1, Tool: "read_file", Status: "committed"}, {Step: 2, Tool: "read_file", Status: "committed"}}
	return w
}

// corpusLines reads every delegation-log row the run wrote.
func corpusLines(t *testing.T, cfg config.Config) []delegationLogLine {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir(), "delegation-log")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("delegation-log: %v", err)
	}
	var out []delegationLogLine
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			var row delegationLogLine
			if err := json.Unmarshal([]byte(l), &row); err != nil {
				t.Fatalf("delegation-log line: %v (%s)", err, l)
			}
			out = append(out, row)
		}
	}
	return out
}

// TestCutKeepsWhatTheRunMeasured: a local run the deadline cancels after nine steps and
// 4,321 generated tokens is published as the call-deadline defer WITH those numbers, and
// the ledger row and the corpus row carry them too. The deadline changes what the outcome
// is called, never what was measured.
func TestCutKeepsWhatTheRunMeasured(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		return measuredWallCancel(), nil
	}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want one call-deadline defer", sum)
	}
	pr := results[0]
	w := pr.Result
	if !w.Deferred || w.DeferClass != core.DeferClassBudget || !strings.HasPrefix(w.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("result = %+v, want the call-deadline budget defer", w)
	}
	if w.Steps != 9 || w.TokensOut != 4321 || w.SeatTokensIn != 812 || w.StopReason != "canceled" || w.SeatTokS != 12.5 || len(w.Trace) != 2 {
		t.Fatalf("the cut lost what the run measured: steps %d tokens_out %d seat_tokens_in %d stop %q seat_tok_s %v trace %d",
			w.Steps, w.TokensOut, w.SeatTokensIn, w.StopReason, w.SeatTokS, len(w.Trace))
	}
	if pr.Node != "this-box" || pr.Seat != "local-seat" || pr.Unplaced {
		t.Fatalf("a run cut mid-run stays placed and named: node %q seat %q unplaced %v", pr.Node, pr.Seat, pr.Unplaced)
	}

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger rows = %d (%v), want the cut subtask's one row", len(rows), err)
	}
	row := rows[0]
	if !row.Deferred || !strings.HasPrefix(row.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("ledger row = deferred %v reason %q, want the call-deadline wording", row.Deferred, row.Reason)
	}
	if row.Steps != 9 || row.TokensOut != 4321 || row.SeatTokensIn != 812 || row.StopReason != "canceled" || row.TokPerSec != 12.5 {
		t.Fatalf("the ledger row lost what the run measured: steps %d tokens_out %d seat_tokens_in %d stop %q tok_per_s %v",
			row.Steps, row.TokensOut, row.SeatTokensIn, row.StopReason, row.TokPerSec)
	}

	lines := corpusLines(t, cfg)
	if len(lines) != 1 || lines[0].Result == nil {
		t.Fatalf("corpus rows = %d, want the cut subtask's one row with its result", len(lines))
	}
	if r := lines[0].Result; r.Steps != 9 || r.TokensOut != 4321 || len(r.Trace) != 2 || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("the corpus row lost what the run measured, or its wording: %+v", r)
	}
}

// TestCutSaysWhatTheRunItselfReported: the cut is the CALL's outcome, but the run's own
// verdict is evidence and rides in the reason. A stack failure that lands after the
// deadline (the engine refused the connection) is still published as the call-deadline
// budget defer — a call deadline is a result shape, never a failure — yet the sentence
// names what the run said, so the operator is not sent to look for a deadline problem
// while the engine was down.
func TestCutSaysWhatTheRunItselfReported(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "local-seat", Deferred: true,
			DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: llama-server unreachable: connection refused (engine is DOWN)"}, nil
	}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)
	r := results[0].Result
	if sum != (Summary{Deferred: 1}) || r.DeferClass != core.DeferClassBudget || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("summary %+v result %+v, want the call-deadline budget defer", sum, r)
	}
	for _, want := range []string{"the run itself reported infrastructure", "connection refused (engine is DOWN)"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason = %q, want it to carry the run's own verdict (%q)", r.Reason, want)
		}
	}
	if strings.Contains(r.Reason, "it was cancelled") {
		t.Errorf("reason = %q: the run did not report a cancellation, so the call must not author one", r.Reason)
	}
	// The corpus row is the full result; the ledger row caps a reason at 120 bytes (it keeps
	// the deadline wording and the measurements, not the tail).
	if lines := corpusLines(t, cfg); len(lines) != 1 || lines[0].Result == nil || !strings.Contains(lines[0].Result.Reason, "connection refused (engine is DOWN)") {
		t.Fatalf("the corpus row does not carry the run's verdict: %+v", lines)
	}
	rows, _ := ledger.ReadAll(cfg.LedgerPath)
	if len(rows) != 1 || !strings.HasPrefix(rows[0].Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("the ledger row does not carry the call-deadline wording: %+v", rows)
	}
}

// TestCutOfAFailureSaysWhatFailed: an outcome that was a FAILURE (a refused dispatch, a
// dead poll) when the deadline passed is published as the defer — and its text is not
// thrown away. A bare cancellation says nothing beyond the deadline and adds nothing.
func TestCutOfAFailureSaysWhatFailed(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(2)}
	got := r.cutByDeadline(PlacedResult{ranLocal: true, Err: "local run: engine returned garbage"})
	if got.Err != "" || !got.deadlineCut || !strings.Contains(got.Result.Reason, "the run itself failed: local run: engine returned garbage") {
		t.Fatalf("a failed local run was cut to %+v, want the call-deadline defer carrying what failed", got)
	}
	if strings.Contains(got.Result.Reason, "it was cancelled") {
		t.Fatalf("reason = %q: the run failed on its own, the call must not author a cancellation", got.Result.Reason)
	}
	// A bare cancellation is the deadline speaking: it is not quoted back, and the seat WAS
	// cancelled, so the reason says so.
	plain := r.cutByDeadline(PlacedResult{ranLocal: true, Err: "canceled: context deadline exceeded"})
	if strings.Contains(plain.Result.Reason, "canceled:") || strings.Contains(plain.Result.Reason, "the run itself") || !strings.Contains(plain.Result.Reason, "it was cancelled") {
		t.Fatalf("a bare cancellation was repeated (or not recognised) in the reason: %q", plain.Result.Reason)
	}
	// What a give-up appended after the cancellation clause is kept (a withdraw that was asked and not
	// confirmed): the clause is the deadline's, the rest is a fact.
	kept := r.cutByDeadline(PlacedResult{intentRecorded: true, ranBase: "http://node-a", JobID: "agd-x", Node: "node-a",
		Err: "canceled: context deadline exceeded; withdraw not confirmed: HTTP 405: the node has no withdraw route"})
	if !strings.Contains(kept.Result.Reason, "the run itself failed: withdraw not confirmed: HTTP 405") || strings.Contains(kept.Result.Reason, "canceled:") {
		t.Fatalf("a cancelled poll's appended verdict was lost or its cancel clause repeated: %q", kept.Result.Reason)
	}
	// An unplaced failure: the text rides in the reason; the placement narration still opens with the marker.
	unplaced := r.cutByDeadline(PlacedResult{Err: "dispatch http://node-a: refused (status 503)", PlacementReason: "route=remote → node-a"})
	if !unplaced.Unplaced || !strings.Contains(unplaced.Result.Reason, "the run itself failed: dispatch http://node-a: refused (status 503)") ||
		!strings.HasPrefix(unplaced.PlacementReason, callDeadlinePrefix) {
		t.Fatalf("an unplaced failure: reason %q placement %q, want its text in the reason and the marker first in the placement", unplaced.Result.Reason, unplaced.PlacementReason)
	}
	// The quote is bounded: a poll-deadline paragraph must not become the published reason.
	long := r.cutByDeadline(PlacedResult{Err: "dispatch: " + strings.Repeat("x", 4000)})
	if len(long.Result.Reason) > 700 {
		t.Fatalf("the reason grew to %d bytes: what a cut quotes must be bounded", len(long.Result.Reason))
	}
}

// TestCutOfAnUnplacedDeferNamesNoBox: a defer the deadline cuts before any node took the
// subtask names no node and no seat, on the result and on the wire it carries. A WAIT's own
// text ("no node had room within 30s", from settle) is not repeated as the outcome — the
// call ran out of time, the fleet was not proven full — and the placement narration keeps
// that history behind the deadline marker.
func TestCutOfAnUnplacedDeferNamesNoBox(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}
	wait := PlacedResult{
		Node: "node-full", Seat: "remote-seat", PlacementReason: "capacity wait: no node had room within 30s",
		Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-full", Seat: "remote-seat",
			Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "capacity wait: no node had room within 30s"},
	}
	got := r.cutOutcome(wait, false) // settle's call: an outcome no attempt produced
	if got.Node != "" || got.Seat != "" || got.Result.NodeID != "" || got.Result.Seat != "" || !got.Unplaced {
		t.Fatalf("an unplaced cut names a box: node %q seat %q wire node %q wire seat %q unplaced %v", got.Node, got.Seat, got.Result.NodeID, got.Result.Seat, got.Unplaced)
	}
	if strings.Contains(got.Result.Reason, "no node had room") {
		t.Fatalf("reason = %q: the call ran out of time, the fleet was not proven full", got.Result.Reason)
	}
	if !strings.HasPrefix(got.PlacementReason, callDeadlinePrefix) || !strings.Contains(got.PlacementReason, "no node had room within 30s") {
		t.Fatalf("placement = %q, want the deadline marker first and the wait's history after it", got.PlacementReason)
	}
	// An attempt's own unplaced outcome (attempt's finish) is quoted: it is a verdict, not a wait.
	verdict := r.cutByDeadline(PlacedResult{Node: "node-a", Seat: "remote-seat", Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion,
		NodeID: "node-a", Seat: "remote-seat", Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "gpu busy: a render holds the card"}})
	if verdict.Node != "" || verdict.Result.NodeID != "" || verdict.Result.Seat != "" || !strings.Contains(verdict.Result.Reason, "the run itself reported capacity: gpu busy: a render holds the card") {
		t.Fatalf("an attempt's unplaced defer: node %q wire node %q reason %q, want no box named and its verdict quoted", verdict.Node, verdict.Result.NodeID, verdict.Result.Reason)
	}
}

// TestCutByDeadlineTouchesOnlyAnOutcomeThatIsNotAnAnswer pins the gate around the
// rewrite: a call whose deadline has NOT passed, a result already cut, the capacity-wait
// sentinel (attempt() hands it to awaitCapacity, which must see it as it is), and a
// finished answer are all returned untouched.
func TestCutByDeadlineTouchesOnlyAnOutcomeThatIsNotAnAnswer(t *testing.T) {
	cutBefore := PlacedResult{deadlineCut: true, Result: cancelledLoop(), PlacementReason: "call deadline reached before a seat took it"}
	sentinel := PlacedResult{waitCapacity: true, pendingReason: "local seat reserved", PlacementReason: "local seat reserved"}
	live := &runner{cfg: testCfg(t), call: &callDeadline{at: time.Now().Add(time.Hour), grace: time.Second, total: 1}}
	live.call.frozen.Store(-1)
	if got := live.cutByDeadline(PlacedResult{Err: "canceled: the client went away"}); got.deadlineCut || got.Err == "" {
		t.Fatalf("an outcome produced before the deadline was rewritten: %+v", got)
	}
	over := &runner{cfg: testCfg(t), call: pastDeadline(3)}
	if got := over.cutByDeadline(cutBefore); got.PlacementReason != cutBefore.PlacementReason || got.Result.Reason != cutBefore.Result.Reason {
		t.Fatalf("a result that was already cut was cut again: %+v", got)
	}
	if got := over.cutByDeadline(sentinel); got.deadlineCut || !got.waitCapacity || got.Result.Deferred {
		t.Fatalf("the capacity-wait sentinel was rewritten: %+v", got)
	}
	// A rewritten failure drops the acceptance verdicts it carried: a cut outcome
	// has no answer to have failed verification on.
	got := over.cutByDeadline(PlacedResult{ranLocal: true, Err: "local run: x", AcceptanceFailures: []string{"contains:widget"}})
	if len(got.AcceptanceFailures) != 0 {
		t.Fatalf("a cut outcome kept acceptance failures: %v", got.AcceptanceFailures)
	}
}

// TestTheCancellationsOwnWordsAreNotQuotedBack: an outcome that says nothing beyond "a
// context ended" — the deadline's own doing — is not quoted as what "the run itself"
// reported. Every place that ends because a context ended words it differently: a cancelled
// poll, a cancelled request ("...: context deadline exceeded"), the agent loop's
// parent-cancelled defer, its wall-timeout defer that names the caller's deadline, and a
// reason that already opens with the call-deadline marker.
func TestTheCancellationsOwnWordsAreNotQuotedBack(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}
	for name, pr := range map[string]PlacedResult{
		"a cancelled poll":         {Err: "canceled: context deadline exceeded"},
		"a cancelled request":      {Err: `dispatch http://node-a: Post "http://node-a/fleet/dispatch": context deadline exceeded (status 0)`},
		"a request cancelled":      {Err: "poll: Get http://node-a/fleet/jobs/x: context canceled"},
		"the loop's parent cancel": {Result: cancelledLoop()},
		"the loop's caller wall": {Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget,
			Reason: "wall timeout after 300s (the caller's deadline, not this node's ceiling)"}},
		"a reason already cut": {Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget,
			Reason: "call deadline reached; 1 unfinished — this subtask never started"}},
	} {
		if own := ownVerdict(pr); own != "" {
			t.Errorf("%s: ownVerdict = %q, want nothing: the run reported only the cancellation", name, own)
		}
		got := r.cutByDeadline(pr)
		if strings.Contains(got.Result.Reason, "the run itself") {
			t.Errorf("%s: the cut quotes the cancellation back as the run's verdict: %q", name, got.Result.Reason)
		}
	}
}
