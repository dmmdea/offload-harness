// pin_test.go: a placement pin needs a reason; without one it is a hint (ADR 0078).
//
// route=local and route=remote were hard pins. On 2026-10-07 a session passed route:"local" to a 78-page
// offload_research call and the whole fan-out queued on one seat while four fleet nodes had free workers. A door
// that offers pin_reason (RunOptions.PinNeedsReason) now treats a local or remote route without one as a hint that
// placement may override, and keeps a reasoned pin authoritative, exactly as before. The tests here drive that at
// each place the decision is made: intake (the closed set), the deal (the idle seat's run-cap line, the remotes
// first), dispatch, the capacity wait, the retry, the ledger row, the result and the process accounting.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// pinCfg is testCfg with the run cap the ADR 0076 tests use: an idle local seat takes four runs.
func pinCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 4
	return cfg
}

// doorOpts is what a door that offers pin_reason (agent_delegate, offload_research, their CLI verbs) hands the engine.
func doorOpts(reason string) *RunOptions { return &RunOptions{PinReason: reason, PinNeedsReason: true} }

// countedOpts is doorOpts carrying a fresh tally, the way the MCP server's doors do: a call is counted in the tally
// it carries and nowhere else, so each test reads exact figures instead of deltas.
func countedOpts(reason string) (*RunOptions, *PinTally) {
	opts, tally := doorOpts(reason), NewPinTally()
	opts.PinTally = tally
	return opts, tally
}

// pinRows are the finished ledger rows of a run, keyed by job id.
func pinRows(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, row := range ledgerRows(t, path) {
		id, _ := row["job_id"].(string)
		out[id] = row
	}
	return out
}

// markerRows are the dispatch markers (phase "started") a run wrote.
func markerRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("ledger row %q: %v", line, err)
		}
		if m["phase"] == "started" {
			out = append(out, m)
		}
	}
	return out
}

// withHeadroom is acceptingNode publishing `ceiling` execution slots of which `running` are taken.
func withHeadroom(ceiling, running int) func(*fakeNode) {
	return func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth, f.jobsRunning, f.queueDepth = ceiling, 2*ceiling, running, running
	}
}

// ---- intake: the closed set ----------------------------------------------------------------------------------------

func TestCheckPinReasonAcceptsTheClosedSetOnTheRoutesThatTakeEachReason(t *testing.T) {
	for _, tc := range []struct {
		route, reason string
		ok            bool
	}{
		{"local", PinPrivacy, true}, {"local", PinLocality, true}, {"local", PinMeasurement, true}, {"local", PinOperator, true},
		{"remote", PinMeasurement, true}, {"remote", PinOperator, true},
		{"remote", PinPrivacy, false}, {"remote", PinLocality, false},
		// no reason is never a validation error: whether a bare pin is a hint is the door's business
		{"local", "", true}, {"remote", "", true}, {"auto", "", true}, {"spread", "", true}, {"queue", "", true}, {"", "", true},
	} {
		err := CheckPinReason(tc.route, tc.reason)
		if (err == nil) != tc.ok {
			t.Errorf("CheckPinReason(%q, %q) = %v, want ok=%v", tc.route, tc.reason, err, tc.ok)
		}
	}
}

func TestCheckPinReasonRefusesAValueOutsideTheSetAndListsTheValidSet(t *testing.T) {
	for _, bad := range []string{"because", "Privacy", " privacy", "privacy ", "operator,measurement", "none", "x"} {
		err := CheckPinReason("local", bad)
		if err == nil {
			t.Errorf("CheckPinReason(local, %q) accepted a value outside the closed set", bad)
			continue
		}
		for _, want := range []string{"privacy (route local)", "locality (route local)", "measurement (route local or remote)", "operator (route local or remote)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("CheckPinReason(local, %q) = %q, want it to list %q", bad, err, want)
			}
		}
	}
}

func TestCheckPinReasonRefusesAReasonOnARouteThatIsNotAPin(t *testing.T) {
	for _, route := range []string{"auto", "spread", "queue", ""} {
		err := CheckPinReason(route, PinOperator)
		if err == nil || !strings.Contains(err.Error(), "only a pin can have a reason") || !strings.Contains(err.Error(), "operator (route local or remote)") {
			t.Errorf("CheckPinReason(%q, operator) = %v, want a refusal that says only a pin can have a reason and lists the set", route, err)
		}
	}
	// the route that takes no such reason is named, with the set
	err := CheckPinReason("remote", PinPrivacy)
	if err == nil || !strings.Contains(err.Error(), "valid with route local only") || !strings.Contains(err.Error(), "measurement (route local or remote)") {
		t.Errorf("CheckPinReason(remote, privacy) = %v, want a refusal naming local as its only route and listing the set", err)
	}
}

func TestPinReasonsIsTheClosedSetInTheOrderTheSchemasListIt(t *testing.T) {
	got := strings.Join(PinReasons(), ",")
	if want := "privacy,locality,measurement,operator"; got != want {
		t.Fatalf("PinReasons() = %s, want %s", got, want)
	}
}

// TestARefusedPinReasonSpendsNothing: a bad pin_reason is refused at intake, before any placement: the fleet is not
// read, nothing is dispatched, the local seat is not asked and no ledger row is written. The same holds for the
// batched entry (offload_research's), which reaches the same guard.
func TestARefusedPinReasonSpendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, route, reason string
	}{
		{"a value outside the set", "local", "because"},
		{"privacy with remote", "remote", PinPrivacy},
		{"a reason with auto", "auto", PinOperator},
		{"a reason with no route", "", PinOperator},
		{"a reason with spread", "spread", PinMeasurement},
		{"a reason with queue", "queue", PinMeasurement},
	} {
		for _, batched := range []bool{false, true} {
			name := tc.name
			if batched {
				name += " (batched)"
			}
			t.Run(name, func(t *testing.T) {
				node, url := acceptingNode(t, "node-a", "zorblax", withHeadroom(4, 0))
				cfg := pinCfg(t)
				var err error
				if batched {
					_, _, err = RunBatched(t.Context(), cfg, neverLocal(t), contracts(2), tc.route, []string{url}, doorOpts(tc.reason))
				} else {
					_, _, err = RunWith(t.Context(), cfg, neverLocal(t), contracts(2), tc.route, []string{url}, doorOpts(tc.reason))
				}
				if err == nil || !strings.Contains(err.Error(), "pin_reason") || !strings.Contains(err.Error(), "operator (route local or remote)") {
					t.Fatalf("err = %v, want a pin_reason refusal that lists the valid set", err)
				}
				if node.healths.Load() != 0 || node.dispatches.Load() != 0 {
					t.Errorf("the fleet was touched (health %d, dispatch %d) by a call refused at intake", node.healths.Load(), node.dispatches.Load())
				}
				if _, statErr := os.Stat(cfg.LedgerPath); statErr == nil {
					t.Errorf("a ledger was written for a call refused at intake")
				}
			})
		}
	}
}

// ---- the acceptance: a reasonless local fan-out with a free remote dispatches remotely and the row says so -----------

// TestAReasonlessLocalFanOutWithAFreeRemoteDispatchesRemotelyAndTheRowSaysSo: 8 subtasks, route local, no pin_reason,
// an idle local seat whose run cap is 4 and one node with 4 free slots. The hint is honoured for the seat's line and
// overridden for the rest: 4 run on the seat, 4 on the node, and each result and ledger row says which.
func TestAReasonlessLocalFanOutWithAFreeRemoteDispatchesRemotelyAndTheRowSaysSo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(8), "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 8 || localCalls.Load() != 4 || node.dispatches.Load() != 4 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, want 8 successes split 4 and 4: the hint is the seat's line, not the whole call", sum, localCalls.Load(), node.dispatches.Load())
	}
	rows := pinRows(t, cfg.LedgerPath)
	for i, pr := range results {
		row := rows[pr.JobID]
		if row == nil {
			t.Fatalf("subtask %d (%s) left no ledger row", i, pr.JobID)
		}
		var wantClause string
		if i < 4 {
			if !pr.ranLocal {
				t.Errorf("subtask %d ran on %s, want the idle seat: it wins the first 4", i, pr.Node)
			}
			wantClause = "route=local was a hint (no pin_reason), honoured: placed on the local seat"
		} else {
			if pr.ranLocal {
				t.Errorf("subtask %d ran local, want the node: the seat's run-cap line is spent", i)
			}
			wantClause = "route=local was a hint (no pin_reason), overridden: placed on node-a"
		}
		if !strings.HasPrefix(pr.PlacementReason, wantClause) {
			t.Errorf("subtask %d placement = %q, want it to open with %q", i, pr.PlacementReason, wantClause)
		}
		if i >= 4 && !strings.Contains(pr.PlacementReason, "the idle local seat's run-cap line is spent by this deal") {
			t.Errorf("subtask %d placement = %q, want the spent line named behind the clause", i, pr.PlacementReason)
		}
		if pr.PinReason != "" {
			t.Errorf("subtask %d carries pin_reason %q: a hint has no reason", i, pr.PinReason)
		}
		// the ledger row: the route applied, the route asked, no reason, and the clause in the 120-byte short form
		if row["route"] != "auto" || row["route_asked"] != "local" || row["pin_reason"] != nil {
			t.Errorf("subtask %d row route=%v route_asked=%v pin_reason=%v, want auto / local / absent", i, row["route"], row["route_asked"], row["pin_reason"])
		}
		if placement, _ := row["placement"].(string); !strings.HasPrefix(placement, wantClause) {
			t.Errorf("subtask %d row placement = %q, want it to open with %q: the clause must survive the ledger's 120-byte short form", i, placement, wantClause)
		}
	}
	if st := tally.Snapshot(); st.Hints != 8 || st.HintsOverridden != 4 || st.Unreasoned != 0 {
		t.Errorf("tally %+v, want 8 hints of which 4 overridden", st)
	}
	for _, m := range markerRows(t, cfg.LedgerPath) {
		if m["route"] != "auto" || m["route_asked"] != "local" || m["pin_reason"] != nil {
			t.Errorf("a dispatch marker says route=%v route_asked=%v pin_reason=%v, want auto / local / absent", m["route"], m["route_asked"], m["pin_reason"])
		}
	}
}

// TestAReasonedLocalPinBehavesAsItAlwaysDid: the same call with a pin_reason is authoritative. All 8 run on the seat,
// the fleet is never read, and the result and the row carry the reason.
func TestAReasonedLocalPinBehavesAsItAlwaysDid(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts(PinMeasurement)
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(8), "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 8 || localCalls.Load() != 8 || node.dispatches.Load() != 0 || node.healths.Load() != 0 {
		t.Fatalf("summary %+v, local runs %d, dispatches %d, health reads %d, want all 8 on the seat and the fleet untouched", sum, localCalls.Load(), node.dispatches.Load(), node.healths.Load())
	}
	rows := pinRows(t, cfg.LedgerPath)
	for i, pr := range results {
		if pr.PlacementReason != "route=local forced" {
			t.Errorf("subtask %d placement = %q, want the unchanged %q", i, pr.PlacementReason, "route=local forced")
		}
		if pr.PinReason != PinMeasurement {
			t.Errorf("subtask %d carries pin_reason %q, want measurement", i, pr.PinReason)
		}
		row := rows[pr.JobID]
		if row["route"] != "local" || row["route_asked"] != "local" || row["pin_reason"] != PinMeasurement || row["placement"] != "route=local forced" {
			t.Errorf("subtask %d row = route %v / asked %v / reason %v / placement %v, want local / local / measurement / the unchanged note", i, row["route"], row["route_asked"], row["pin_reason"], row["placement"])
		}
	}
	if st := tally.Snapshot(); st.Reasoned[PinMeasurement] != 8 || st.Hints != 0 || st.HintsOverridden != 0 || st.Unreasoned != 0 {
		t.Errorf("tally %+v, want 8 subtasks reasoned under measurement and nothing else", st)
	}
}

// TestACallThroughADoorWithNoReasonChannelKeepsItsAuthoritativePin: fleet-smoke, the review lane's remote
// fallthrough, offload_ask and agent_run hand the engine a bare route and have no way to give a reason. They stay
// pins, and the rows say nothing new; the process counts them as unreasoned so the gap is visible.
func TestACallThroughADoorWithNoReasonChannelKeepsItsAuthoritativePin(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	tally := NewPinTally()
	for _, opts := range []*RunOptions{nil, {}, {PinTally: tally}} {
		results, _, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(8), "local", []string{url}, opts)
		if err != nil {
			t.Fatal(err)
		}
		for i, pr := range results {
			if !pr.ranLocal || pr.PlacementReason != "route=local forced" {
				t.Errorf("subtask %d: ranLocal %v, placement %q, want a forced local run", i, pr.ranLocal, pr.PlacementReason)
			}
		}
	}
	if node.dispatches.Load() != 0 || node.healths.Load() != 0 {
		t.Fatalf("the fleet was touched (health %d, dispatch %d) by a pin", node.healths.Load(), node.dispatches.Load())
	}
	for id, row := range pinRows(t, cfg.LedgerPath) {
		if row["route"] != "local" || row["route_asked"] != nil || row["pin_reason"] != nil {
			t.Errorf("row %s = route %v / asked %v / reason %v, want a plain local row", id, row["route"], row["route_asked"], row["pin_reason"])
		}
	}
	if st := tally.Snapshot(); st.Unreasoned != 8 || st.Hints != 0 || st.HintsOverridden != 0 {
		t.Errorf("tally %+v, want the 8 subtasks of the call that carried it counted as unreasoned and no hint", st)
	}
	// and the same route through a door that does offer a reason is a hint, not a pin
	var hinted atomic.Int64
	if _, _, err := RunWith(t.Context(), cfg, passingLocal(&hinted), pages(8), "local", []string{url}, doorOpts("")); err != nil {
		t.Fatal(err)
	}
	if node.dispatches.Load() != 4 {
		t.Errorf("the door that offers pin_reason sent %d to the node, want 4: the same bare route is a hint there", node.dispatches.Load())
	}
}

// TestASmallReasonlessLocalCallStaysLocalAndSaysSo: what the caller hinted is still what a call that fits the idle
// seat's line gets, and it reads no node's health, as an idle box never did.
func TestASmallReasonlessLocalCallStaysLocalAndSaysSo(t *testing.T) {
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(3), "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 3 || localCalls.Load() != 3 || node.dispatches.Load() != 0 || node.healths.Load() != 0 {
		t.Fatalf("summary %+v, local runs %d, dispatches %d, health reads %d, want 3 on the seat and the fleet unread", sum, localCalls.Load(), node.dispatches.Load(), node.healths.Load())
	}
	for i, pr := range results {
		if want := "route=local was a hint (no pin_reason), honoured: placed on the local seat; local idle"; pr.PlacementReason != want {
			t.Errorf("subtask %d placement = %q, want %q", i, pr.PlacementReason, want)
		}
	}
	if st := tally.Snapshot(); st.Hints != 3 || st.HintsOverridden != 0 {
		t.Errorf("tally %+v, want 3 hints and none overridden", st)
	}
}

// TestAReasonlessLocalCallDoesNotRunOnASeatAnotherSessionReserved: a text lease reserves the seat. A pinned local run
// goes there anyway (the caller's explicit, recorded choice); a hint does not, and the fleet takes the work.
func TestAReasonlessLocalCallDoesNotRunOnASeatAnotherSessionReserved(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := pinCfg(t)
	cfg.GPULockPath = dir

	var pinnedLocal atomic.Int64
	pinned, _, err := RunWith(t.Context(), cfg, passingLocal(&pinnedLocal), pages(2), "local", []string{url}, doorOpts(PinPrivacy))
	if err != nil {
		t.Fatal(err)
	}
	if pinnedLocal.Load() != 2 || node.dispatches.Load() != 0 {
		t.Fatalf("a reasoned pin ran %d local and %d remote, want 2 and 0: the pin is authoritative, a lease included", pinnedLocal.Load(), node.dispatches.Load())
	}
	for i, pr := range pinned {
		if pr.PinReason != PinPrivacy || pr.PlacementReason != "route=local forced" {
			t.Errorf("pinned subtask %d: reason %q placement %q", i, pr.PinReason, pr.PlacementReason)
		}
	}

	var hintedLocal atomic.Int64
	hinted, sum, err := RunWith(t.Context(), cfg, passingLocal(&hintedLocal), pages(2), "local", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 2 || hintedLocal.Load() != 0 || node.dispatches.Load() != 2 {
		t.Fatalf("summary %+v, local %d, dispatches %d, want both subtasks on the node: the seat is reserved and the caller gave no reason", sum, hintedLocal.Load(), node.dispatches.Load())
	}
	for i, pr := range hinted {
		if !strings.HasPrefix(pr.PlacementReason, "route=local was a hint (no pin_reason), overridden: placed on node-a") {
			t.Errorf("hinted subtask %d placement = %q, want it to say the hint was overridden", i, pr.PlacementReason)
		}
	}
}

// TestAHintedLocalCallMayRetryOnAFleetNodeWhereAPinMayNot: a failed verification is retried on a different node. A
// pinned local call has none to retry on (route local places nothing on another node); a hint is placed as auto and
// has the fleet.
func TestAHintedLocalCallMayRetryOnAFleetNodeWhereAPinMayNot(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var pinnedCalls atomic.Int64
	pinned, psum, err := RunWith(t.Context(), cfg, failingLocal(&pinnedCalls), contracts(1), "local", []string{url}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	if psum.Retried != 0 || node.dispatches.Load() != 0 || len(pinned[0].AcceptanceFailures) == 0 {
		t.Fatalf("pinned: summary %+v, dispatches %d, failures %v, want the failed verification left as it is: a pin has no other node", psum, node.dispatches.Load(), pinned[0].AcceptanceFailures)
	}
	var hintedCalls atomic.Int64
	hinted, hsum, err := RunWith(t.Context(), cfg, failingLocal(&hintedCalls), contracts(1), "local", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if hsum.Retried != 1 || hsum.RetryRecovered != 1 || hinted[0].RetriedOn != "node-a" || node.dispatches.Load() != 1 {
		t.Fatalf("hinted: summary %+v, retried on %q, dispatches %d, want the retry on node-a", hsum, hinted[0].RetriedOn, node.dispatches.Load())
	}
	// the published result is the retry, placed on the node: the hint was overridden for that attempt
	if !strings.HasPrefix(hinted[0].PlacementReason, "route=local was a hint (no pin_reason), overridden: placed on node-a") {
		t.Errorf("placement = %q, want it to say the hint was overridden", hinted[0].PlacementReason)
	}
}

// ---- route=remote -------------------------------------------------------------------------------------------------

// TestAReasonlessRemoteRouteIsPlacedFleetFirst: with a node that has room, a remote hint goes there, and the idle
// local seat does not win it as it would for route=auto.
func TestAReasonlessRemoteRouteIsPlacedFleetFirst(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(2), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 2 || node.dispatches.Load() != 2 {
		t.Fatalf("summary %+v, dispatches %d, want both on the node: remotes first even with the seat idle", sum, node.dispatches.Load())
	}
	for i, pr := range results {
		if !strings.HasPrefix(pr.PlacementReason, "route=remote was a hint (no pin_reason), honoured: placed on node-a; ") {
			t.Errorf("subtask %d placement = %q, want the honoured clause", i, pr.PlacementReason)
		}
		if strings.Contains(pr.PlacementReason, "local busy") {
			t.Errorf("subtask %d placement = %q says the local seat was busy: it was dealt as unavailable by preference, not read busy", i, pr.PlacementReason)
		}
	}
	for id, row := range pinRows(t, cfg.LedgerPath) {
		if row["route"] != "auto" || row["route_asked"] != "remote" || row["pin_reason"] != nil {
			t.Errorf("row %s = route %v / asked %v / reason %v, want auto / remote / absent", id, row["route"], row["route_asked"], row["pin_reason"])
		}
	}
	if st := tally.Snapshot(); st.Hints != 2 || st.HintsOverridden != 0 {
		t.Errorf("tally %+v, want 2 hints and none overridden", st)
	}
}

// TestAReasonlessRemoteRouteFallsBackToTheLocalSeatWhereRemoteWouldDefer: no remote can run the contract at all. A
// remote PIN defers loudly and touches no seat, as it always did; a hint lets the idle local seat run it at once and
// says so.
func TestAReasonlessRemoteRouteFallsBackToTheLocalSeatWhereRemoteWouldDefer(t *testing.T) {
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := off.server().URL
	cfg := pinCfg(t)

	pinned, psum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	if psum.Deferred != 1 || !pinned[0].Unplaced || !strings.HasPrefix(pinned[0].Result.Reason, "route=remote: ") || pinned[0].PinReason != PinOperator {
		t.Fatalf("pinned: summary %+v, unplaced %v, reason %q, pin_reason %q, want the loud route=remote defer carrying its reason", psum, pinned[0].Unplaced, pinned[0].Result.Reason, pinned[0].PinReason)
	}

	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	hinted, hsum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if hsum.Succeeded != 1 || localCalls.Load() != 1 || !hinted[0].ranLocal {
		t.Fatalf("hinted: summary %+v, local runs %d, ran local %v, want the idle seat to run it", hsum, localCalls.Load(), hinted[0].ranLocal)
	}
	for _, want := range []string{"route=remote was a hint (no pin_reason), overridden: placed on the local seat; ", "no eligible remote", "a remote hint falls back to the local seat"} {
		if !strings.Contains(hinted[0].PlacementReason, want) {
			t.Errorf("placement = %q, want it to contain %q", hinted[0].PlacementReason, want)
		}
	}
	if strings.Contains(hinted[0].PlacementReason, "local busy") {
		t.Errorf("placement = %q says the local seat was busy: it was idle", hinted[0].PlacementReason)
	}
	if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 1 {
		t.Errorf("tally %+v, want 1 hint, overridden", st)
	}
}

// TestAReasonlessRemoteRouteGivesTheIdleSeatWhatNoRemoteHasRoomFor: one node with room for two of four. The deal gives
// the node its headroom and the idle seat the other two, with the wait switched off (testCfg's default), which is
// where a remote pin would have deferred them. The seat's line is counted like a remote's headroom.
func TestAReasonlessRemoteRouteGivesTheIdleSeatWhatNoRemoteHasRoomFor(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 2)) // headroom 2
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(4), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || node.dispatches.Load() != 2 || localCalls.Load() != 2 {
		t.Fatalf("summary %+v, dispatches %d, local runs %d, want 2 on the node (its headroom) and 2 on the idle seat", sum, node.dispatches.Load(), localCalls.Load())
	}
	for i, pr := range results {
		switch {
		case i < 2 && (pr.ranLocal || !strings.Contains(pr.PlacementReason, "honoured: placed on node-a")):
			t.Errorf("subtask %d: ran local %v, placement %q, want the node first", i, pr.ranLocal, pr.PlacementReason)
		case i >= 2 && (!pr.ranLocal || !strings.Contains(pr.PlacementReason, "overridden: placed on the local seat") ||
			!strings.Contains(pr.PlacementReason, "a remote hint falling back where no remote has room")):
			t.Errorf("subtask %d: ran local %v, placement %q, want the seat named as the fallback", i, pr.ranLocal, pr.PlacementReason)
		}
	}
	if st := tally.Snapshot(); st.Hints != 4 || st.HintsOverridden != 2 {
		t.Errorf("tally %+v, want 4 hints of which 2 overridden", st)
	}

	// the control: a remote pin defers the same two (the wait is off), and the idle seat is never touched
	pinned, psum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(4), "remote", []string{url}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	if psum.Succeeded != 2 || psum.Deferred != 2 {
		t.Fatalf("pinned: summary %+v, want the node's two succeeded and the other two deferred for capacity", psum)
	}
	for _, pr := range pinned[2:] {
		if pr.Result.DeferClass != core.DeferClassCapacity {
			t.Errorf("pinned overflow defer class %q, want capacity", pr.Result.DeferClass)
		}
	}
}

// TestAReasonlessRemoteRouteNeverRunsOnASeatAnotherSessionReserved: the fallback is the seat, not a lease's cards. With
// a text lease held and the only node full, the hint waits in line and ends as the reserved-seat defer; it is not
// run over the holder.
func TestAReasonlessRemoteRouteNeverRunsOnASeatAnotherSessionReserved(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	// full in both senses the engine reads: no headroom for the deal, and a queue at its depth for the wait
	node, url := acceptingNode(t, "node-a", "zorblax from A", func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning, f.maxQueueDepth, f.queueDepth, f.jobsQueued = 1, 1, 2, 2, 1
	})
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := pinCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 1
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deferred != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("summary %+v, dispatches %d, want the subtask deferred with nothing run", sum, node.dispatches.Load())
	}
	if !strings.Contains(results[0].Result.Reason, "reserved") || !strings.HasPrefix(results[0].PlacementReason, "route=remote was a hint (no pin_reason): placed remotes-first, and no node took it") {
		t.Errorf("reason %q, placement %q, want the reserved-seat defer and a clause that says no node took it", results[0].Result.Reason, results[0].PlacementReason)
	}
}

// ---- the stamp, the clause, the accounting --------------------------------------------------------------------------

func TestTheHintClauseSaysHonouredOverriddenOrThatNoNodeTookIt(t *testing.T) {
	ranLocal := PlacedResult{ranLocal: true, Node: "this-box", PlacementReason: "local idle"}
	ranNode := PlacedResult{ranBase: "http://node-a", Node: "node-a", PlacementReason: "route=auto → node-a (headroom)"}
	nowhere := PlacedResult{Unplaced: true, PlacementReason: "capacity wait: nothing freed"}
	for _, tc := range []struct {
		asked string
		pr    PlacedResult
		want  string
	}{
		{"local", ranLocal, "route=local was a hint (no pin_reason), honoured: placed on the local seat; local idle"},
		// no placement note to follow the clause: the clause stands alone, without a dangling separator
		{"local", PlacedResult{ranLocal: true}, "route=local was a hint (no pin_reason), honoured: placed on the local seat"},
		{"local", ranNode, "route=local was a hint (no pin_reason), overridden: placed on node-a; route=auto → node-a (headroom)"},
		{"local", nowhere, "route=local was a hint (no pin_reason): placed as route=auto, and no node took it; capacity wait: nothing freed"},
		{"remote", ranNode, "route=remote was a hint (no pin_reason), honoured: placed on node-a; route=auto → node-a (headroom)"},
		{"remote", ranLocal, "route=remote was a hint (no pin_reason), overridden: placed on the local seat; local idle"},
		{"remote", nowhere, "route=remote was a hint (no pin_reason): placed remotes-first, and no node took it; capacity wait: nothing freed"},
	} {
		r := &runner{pin: pinCall{asked: tc.asked, hint: true}}
		got := r.stampPin(tc.pr)
		if got.PlacementReason != tc.want {
			t.Errorf("%s hint on %+v: placement = %q, want %q", tc.asked, tc.pr.PlacementReason, got.PlacementReason, tc.want)
		}
		if again := r.stampPin(got); again.PlacementReason != got.PlacementReason {
			t.Errorf("stamping twice changed the placement to %q", again.PlacementReason)
		}
		if got.PinReason != "" {
			t.Errorf("a hint stamped pin_reason %q", got.PinReason)
		}
	}
	// a result that already named a placed remote with no node id still names where it went
	anon := PlacedResult{ranBase: "http://node-z:18811"}
	if got := (&runner{pin: pinCall{asked: "local", hint: true}}).stampPin(anon).PlacementReason; !strings.Contains(got, "overridden: placed on http://node-z:18811") {
		t.Errorf("placement = %q, want the dial base named when the node reported no id", got)
	}
}

func TestAReasonedPinStampsItsReasonAndLeavesThePlacementAlone(t *testing.T) {
	r := &runner{pin: pinCall{asked: "remote", reason: PinOperator}}
	pr := r.stampPin(PlacedResult{ranBase: "http://node-a", PlacementReason: "route=remote forced → node-a"})
	if pr.PinReason != PinOperator || pr.PlacementReason != "route=remote forced → node-a" {
		t.Fatalf("stamped %+v, want the reason added and the placement untouched", pr)
	}
	none := (&runner{}).stampPin(PlacedResult{PlacementReason: "x"})
	if none.PinReason != "" || none.PlacementReason != "x" {
		t.Fatalf("a call with no pin was stamped: %+v", none)
	}
}

// TestTheLedgerKeepsTheHintClauseInItsShortFormOfThePlacement: the ledger cuts a placement note to 120 bytes, so the
// clause opens the note: whatever follows it is what is cut.
func TestTheLedgerKeepsTheHintClauseInItsShortFormOfThePlacement(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	_, url := acceptingNode(t, "node-with-a-rather-long-identifier-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	results, _, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(8), "local", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	rows := pinRows(t, cfg.LedgerPath)
	long := results[7]
	if len(long.PlacementReason) <= 120 {
		t.Fatalf("fixture: placement %q is short enough to prove nothing", long.PlacementReason)
	}
	placement, _ := rows[long.JobID]["placement"].(string)
	if len(placement) > 120 || !strings.HasPrefix(placement, "route=local was a hint (no pin_reason), overridden: placed on node-with-a-rather-long-identifier-a") {
		t.Errorf("ledger placement = %q (%d bytes), want the clause and the node inside the 120-byte short form", placement, len(placement))
	}
}

func TestAPinnedResultCarriesItsReasonOnTheWire(t *testing.T) {
	pinned := WireResponse([]PlacedResult{{PinReason: PinPrivacy, PlacementReason: "route=local forced"}, {PlacementReason: "x"}}, Summary{}, nil)
	raw, err := json.Marshal(pinned)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Results[0]["pin_reason"] != PinPrivacy {
		t.Errorf("result 0 = %v, want pin_reason privacy", back.Results[0])
	}
	if _, has := back.Results[1]["pin_reason"]; has {
		t.Errorf("result 1 = %v, want no pin_reason key on a result with none: every other result publishes byte-identically", back.Results[1])
	}
}

func TestAPinTallyListsEveryReasonOfTheClosedSetAndCountsOnlyPublishedResults(t *testing.T) {
	tally := NewPinTally()
	st := tally.Snapshot()
	for _, reason := range PinReasons() {
		if n, ok := st.Reasoned[reason]; !ok || n != 0 {
			t.Errorf("a fresh tally's Reasoned[%q] = %d, %v, want a zero entry: the shape must not depend on what has been seen", reason, n, ok)
		}
	}
	if st.Since.IsZero() || st.Since.After(time.Now()) {
		t.Errorf("Since = %v, want the moment the tally started", st.Since)
	}
	if nilSnap := (*PinTally)(nil).Snapshot(); len(nilSnap.Reasoned) != len(PinReasons()) || nilSnap.Hints != 0 {
		t.Errorf("a nil tally's snapshot = %+v, want an empty one of the same shape", nilSnap)
	}
	// a hint that nothing ran is a hint and not an override; one placed the other way is both
	r := &runner{pin: pinCall{asked: "local", hint: true}, tally: tally}
	r.tallyPins([]PlacedResult{{ranLocal: true}, {ranBase: "http://n"}, {Unplaced: true}, {ranBase: "http://n", Unplaced: true}})
	if st = tally.Snapshot(); st.Hints != 4 || st.HintsOverridden != 1 {
		t.Errorf("tally %+v, want 4 hints and 1 override: only a result placed on a node overrides a local hint", st)
	}
	r = &runner{pin: pinCall{asked: "remote", hint: true}, tally: tally}
	r.tallyPins([]PlacedResult{{ranLocal: true}, {ranBase: "http://n"}, {Unplaced: true}})
	if st = tally.Snapshot(); st.Hints != 7 || st.HintsOverridden != 2 {
		t.Errorf("tally %+v, want 7 hints and 2 overrides: only a result placed on the local seat overrides a remote hint", st)
	}
	r = &runner{pin: pinCall{asked: "local", reason: PinPrivacy}, tally: tally}
	r.tallyPins([]PlacedResult{{ranLocal: true}, {ranLocal: true}})
	if st = tally.Snapshot(); st.Reasoned[PinPrivacy] != 2 || st.Hints != 7 {
		t.Errorf("tally %+v, want 2 subtasks reasoned under privacy and the hints unchanged", st)
	}
	r = &runner{pin: pinCall{asked: "remote"}, tally: tally}
	r.tallyPins([]PlacedResult{{ranBase: "http://n"}})
	if st = tally.Snapshot(); st.Unreasoned != 1 {
		t.Errorf("tally %+v, want the reasonless pin counted as unreasoned", st)
	}
	// a call that is not a pin is not counted, and a call with no tally counts nowhere (and does not panic)
	before := tally.Snapshot()
	(&runner{tally: tally}).tallyPins([]PlacedResult{{ranLocal: true}})
	(&runner{pin: pinCall{asked: "local", hint: true}}).tallyPins([]PlacedResult{{ranLocal: true}})
	if after := tally.Snapshot(); after.Hints != before.Hints || after.Unreasoned != before.Unreasoned || after.Reasoned[PinPrivacy] != before.Reasoned[PinPrivacy] {
		t.Errorf("a call that is no pin changed the tally: %+v -> %+v", before, after)
	}
}

// TestResolvePinAppliesAutoToAHintAndTheCallersRouteToEverythingElse is the intake decision as a table: only a local
// or remote route with no reason, through a door that offers one, becomes a hint (applied as auto). A reasoned pin, a
// bare pin through a door with no reason channel, and every other route keep the route the caller named.
func TestResolvePinAppliesAutoToAHintAndTheCallersRouteToEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		route string
		opts  *RunOptions
		want  string // the route applied
		pin   pinCall
	}{
		{"local", doorOpts(""), "auto", pinCall{asked: "local", hint: true}},
		{"remote", doorOpts(""), "auto", pinCall{asked: "remote", hint: true}},
		{"local", doorOpts(PinPrivacy), "local", pinCall{asked: "local", reason: PinPrivacy}},
		{"remote", doorOpts(PinOperator), "remote", pinCall{asked: "remote", reason: PinOperator}},
		{"local", nil, "local", pinCall{asked: "local"}},
		{"remote", &RunOptions{}, "remote", pinCall{asked: "remote"}},
		{"local", &RunOptions{PinReason: PinLocality}, "local", pinCall{asked: "local", reason: PinLocality}},
		{"auto", doorOpts(""), "auto", pinCall{}},
		{"spread", doorOpts(""), "spread", pinCall{}},
		{"queue", doorOpts(""), "queue", pinCall{}},
	} {
		pin, applied, err := resolvePin(tc.route, tc.opts)
		if err != nil {
			t.Errorf("resolvePin(%q, %+v): %v", tc.route, tc.opts, err)
			continue
		}
		if applied != tc.want || pin != tc.pin {
			t.Errorf("resolvePin(%q, %+v) = %+v applied %q, want %+v applied %q", tc.route, tc.opts, pin, applied, tc.pin, tc.want)
		}
	}
	if _, _, err := resolvePin("remote", doorOpts(PinPrivacy)); err == nil {
		t.Error("resolvePin accepted privacy with remote: the engine must refuse what the door refuses")
	}
}

// TestAHintedResearchSizedBatchIsOneDealAcrossTheFleet is the incident's shape on the batched entry offload_research
// uses: 12 pages, route local, no reason. The idle seat takes its run-cap line and the node the rest, in one batch,
// where the pinned call queued all 12 on the seat.
func TestAHintedResearchSizedBatchIsOneDealAcrossTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(8, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	_, sum, err := RunBatched(t.Context(), cfg, passingLocal(&localCalls), pages(12), "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 12 || sum.Batches != 1 || localCalls.Load() != 4 || node.dispatches.Load() != 8 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, want 12 pages in ONE batch, 4 on the seat and 8 on the node", sum, localCalls.Load(), node.dispatches.Load())
	}
	if st := tally.Snapshot(); st.Hints != 12 || st.HintsOverridden != 8 {
		t.Errorf("tally %+v, want 12 hints of which 8 overridden", st)
	}
}

// TestAReasonlessRemoteRouteCountsTheIdleSeatsLineLikeARemotesHeadroom: the fallback is the seat's run-cap line, not the
// whole overflow. Four subtasks, a node with room for two, a seat whose line takes one: two go to the node, one to the
// seat, and the fourth has nowhere to go and (the wait is off) defers for capacity. The seat is not dealt past its line.
func TestAReasonlessRemoteRouteCountsTheIdleSeatsLineLikeARemotesHeadroom(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 2)) // headroom 2
	cfg := pinCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(4), "remote", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 3 || sum.Deferred != 1 || node.dispatches.Load() != 2 || localCalls.Load() != 1 {
		t.Fatalf("summary %+v, dispatches %d, local runs %d, want 2 on the node, 1 on the seat (its line) and 1 deferred", sum, node.dispatches.Load(), localCalls.Load())
	}
	if last := results[3]; !last.Unplaced || last.Result.DeferClass != core.DeferClassCapacity {
		t.Errorf("the fourth subtask = unplaced %v, class %q, want a capacity defer: nothing had room for it", last.Unplaced, last.Result.DeferClass)
	}
}

// TestAReasonlessRemoteRouteDoesNotGiveTheSeatAContractItCannotRun: a contract that names a layer this box does not
// declare is not the seat's work (register A-108), so the fallback does not hand it over; the remote hint does not
// turn the seat into a place a layer-naming contract can land.
func TestAReasonlessRemoteRouteDoesNotGiveTheSeatAContractItCannotRun(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	// a node that declares the `fast` layer and is full; this box (pinCfg declares no layers) cannot run a contract naming it
	node, url := declaringNode(t, "node-a", &layerTape{}, withHeadroom(1, 1))
	cfg := pinCfg(t)
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{layerContract("digest page A", "fast")}, "remote", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 0 || sum.Deferred != 1 || results[0].ranLocal || node.dispatches.Load() != 0 {
		t.Fatalf("summary %+v, ran local %v, dispatches %d, want the layer-naming subtask left in line, never on a seat that declares no such layer", sum, results[0].ranLocal, node.dispatches.Load())
	}
	// Left in line is a CAPACITY defer. Dealt to the seat it would have been a contract-class defer from the seat's own
	// decision ("this box declares no layer fast"): the same nothing-ran outcome, blamed on the caller's contract.
	if got := results[0].Result.DeferClass; got != core.DeferClassCapacity {
		t.Errorf("defer class %q (%q), want capacity: the node that declares the layer is full, and the seat never had the contract", got, results[0].Result.Reason)
	}
}

// TestAReasonlessRemoteRouteFallsBackFromTheCapacityWaitToo: the deal gave the seat its line and one subtask overflowed
// it. That subtask waits (the wait is on), and at its first look the seat's line has a free slot (the fake local run is
// registered nowhere), so it runs there, and the placement says why in a remote hint's words, not "the seat was busy".
func TestAReasonlessRemoteRouteFallsBackFromTheCapacityWaitToo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-a", "zorblax from A", func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning, f.maxQueueDepth, f.queueDepth, f.jobsQueued = 1, 1, 2, 2, 1
	})
	cfg := pinCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	cfg.AgentPlacementWaitSec = 5
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(2), "remote", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 2 || localCalls.Load() != 2 || node.dispatches.Load() != 0 || sum.Waited != 1 {
		t.Fatalf("summary %+v, local runs %d, dispatches %d, want both on the seat, the second after the wait", sum, localCalls.Load(), node.dispatches.Load())
	}
	if want := "a remote hint falls back to it (capacity wait)"; !strings.Contains(results[1].PlacementReason, want) || strings.Contains(results[1].PlacementReason, "busy") {
		t.Errorf("placement = %q, want it to say %q and not that the seat was busy", results[1].PlacementReason, want)
	}
}

// TestAHintedCallRefusedBeforePlacementKeepsItsReasonCode: the hint clause is prefixed to the placement note, and the
// ledger's reason code is decided from that note when it is exactly "refused before placement". The code is read from the
// result as the engine made it, so a hint changes neither the code nor the fact the row opens with the clause.
func TestAHintedCallRefusedBeforePlacementKeepsItsReasonCode(t *testing.T) {
	cfg := pinCfg(t)
	bad := plainContract()
	bad.Goal = ""
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{bad}, "local", nil, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || !strings.HasPrefix(results[0].PlacementReason, "route=local was a hint (no pin_reason): placed as route=auto, and no node took it; refused before placement") {
		t.Fatalf("summary %+v, placement %q, want the failure and a clause that says no node took it", sum, results[0].PlacementReason)
	}
	rows := ledgerRows(t, cfg.LedgerPath)
	if len(rows) != 1 || rows[0]["reason_code"] != "contract" {
		t.Fatalf("rows = %v, want one row with reason_code contract: the clause must not hide a refusal from the reason-code rule", rows)
	}
}

// TestAReasonlessLocalCallLeavesABusySeatForTheFleet: the seat's own load, read through llama-swap, is a reason too. Four
// requests in flight on a seat whose run cap is four: a pinned local call would queue behind them; a hint is placed as
// auto, which reads the seat busy and deals the whole call to the node with room.
func TestAReasonlessLocalCallLeavesABusySeatForTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(8, 0))
	cfg := pinCfg(t)
	cfg.Endpoint = busySwap(t, "local-seat", 4)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(4), "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || localCalls.Load() != 0 || node.dispatches.Load() != 4 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, want all 4 on the node: the seat already holds its run cap", sum, localCalls.Load(), node.dispatches.Load())
	}
	for i, pr := range results {
		if !strings.HasPrefix(pr.PlacementReason, "route=local was a hint (no pin_reason), overridden: placed on node-a") {
			t.Errorf("subtask %d placement = %q, want it to say the hint was overridden", i, pr.PlacementReason)
		}
	}
	if st := tally.Snapshot(); st.Hints != 4 || st.HintsOverridden != 4 {
		t.Errorf("tally %+v, want 4 hints, all overridden", st)
	}

	// the same busy seat, pinned: the caller gave a reason, so the call goes to the seat whatever it holds
	pinned, psum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(4), "local", []string{url}, doorOpts(PinLocality))
	if err != nil {
		t.Fatal(err)
	}
	if psum.Succeeded != 4 || localCalls.Load() != 4 || node.dispatches.Load() != 4 || !pinned[0].ranLocal {
		t.Fatalf("pinned: summary %+v, local runs %d, node dispatches %d, want all 4 on the seat", psum, localCalls.Load(), node.dispatches.Load())
	}
}

// TestThePerSubtaskPlacementOfARemoteHintAlsoPrefersTheFleet: a runner placed without a joint deal (a white-box caller of
// runOne; RunWith always deals) reaches attempt()'s own placement, which has its own busy decision. A remote hint
// forces the seat unavailable there too, and says what happened in the hint's words.
func TestThePerSubtaskPlacementOfARemoteHintAlsoPrefersTheFleet(t *testing.T) {
	idle := func(context.Context) busyReading { return busyReading{} }
	remote, url := eligibleNode(t, "node-a", "zorblax answered")
	cfg := testCfg(t)
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", pin: pinCall{asked: "remote", hint: true}, remotes: []string{url},
		intent: openIntentLedger(cfg), localBusyProbe: idle,
	}
	pr := r.runOne(context.Background(), 0, remoteContract())
	if localCalls.Load() != 0 || remote.dispatches.Load() != 1 || pr.Node != "node-a" {
		t.Fatalf("local runs %d, dispatches %d, node %q, want the idle seat passed over for the node", localCalls.Load(), remote.dispatches.Load(), pr.Node)
	}
	if pr.PlacementReason != "remotes first; placed on node-a" {
		t.Errorf("placement = %q, want the remote hint's words, not %q", pr.PlacementReason, "local busy; placed on node-a")
	}

	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	var fallback atomic.Int64
	r2 := &runner{
		cfg: cfg, local: passingLocal(&fallback), route: "auto", pin: pinCall{asked: "remote", hint: true}, remotes: []string{off.server().URL},
		intent: openIntentLedger(cfg), localBusyProbe: idle,
	}
	pr2 := r2.runOne(context.Background(), 0, remoteContract())
	if fallback.Load() != 1 || !strings.Contains(pr2.PlacementReason, "a remote hint falls back to the local seat") || strings.Contains(pr2.PlacementReason, "local busy") {
		t.Fatalf("local runs %d, placement %q, want the idle seat to take what no remote can, saying so", fallback.Load(), pr2.PlacementReason)
	}
}

// TestADispatchMarkerCarriesThePinReasonOfAReasonedPin: the marker a delegator writes when it hands a job to a node is a
// row of its own (a hang is visible while it happens), so it says what the caller pinned too.
func TestADispatchMarkerCarriesThePinReasonOfAReasonedPin(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, doorOpts(PinOperator)); err != nil || sum.Succeeded != 1 {
		t.Fatalf("summary %+v, err %v, want the pinned call to succeed on the node", sum, err)
	}
	markers := markerRows(t, cfg.LedgerPath)
	if node.dispatches.Load() != 1 || len(markers) != 1 {
		t.Fatalf("dispatches %d, markers %d, want one of each", node.dispatches.Load(), len(markers))
	}
	if m := markers[0]; m["route"] != "remote" || m["route_asked"] != "remote" || m["pin_reason"] != PinOperator {
		t.Errorf("marker = route %v / asked %v / reason %v, want remote / remote / operator", m["route"], m["route_asked"], m["pin_reason"])
	}
}

// TestABrowseGrantPinsALocalRouteUnderTheImpliedReasonLocality: the browse tool drives THIS machine's own browser (ADR 0060),
// which is why the MCP door admits a browse grant only on route local. A bare local is a hint now, and a hint could
// place the run on another node's browser, so the grant is the reason: the call is pinned, under pin_reason locality, and
// says so. The seat's line takes ONE subtask here; without the grant the other two would be dealt to the node.
func TestABrowseGrantPinsALocalRouteUnderTheImpliedReasonLocality(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(8, 0))
	cfg := pinCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	browse := plainContract()
	browse.AllowBrowse, browse.BrowseHosts = true, []string{"docs.example"}
	plain := pages(2)
	subtasks := []core.AgentContract{plain[0], browse, plain[1]}
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), subtasks, "local", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 3 || localCalls.Load() != 3 || node.dispatches.Load() != 0 || node.healths.Load() != 0 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, health reads %d, want all 3 on the seat: the browse grant pins the call", sum, localCalls.Load(), node.dispatches.Load(), node.healths.Load())
	}
	for i, pr := range results {
		if pr.PinReason != PinLocality || pr.PlacementReason != "route=local forced" {
			t.Errorf("subtask %d: pin_reason %q, placement %q, want the implied locality and the unchanged forced placement", i, pr.PinReason, pr.PlacementReason)
		}
	}
	if st := tally.Snapshot(); st.Reasoned[PinLocality] != 3 || st.Hints != 0 {
		t.Errorf("tally %+v, want 3 subtasks reasoned under locality and no hint", st)
	}

	// the same two plain subtasks without a browse grant are the hint they were: the seat's line takes one, the node the other
	_, sum2, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), plain, "local", []string{url}, doorOpts(""))
	if err != nil || sum2.Succeeded != 2 || node.dispatches.Load() != 1 {
		t.Fatalf("control: summary %+v, err %v, node dispatches %d, want the second subtask dealt to the node", sum2, err, node.dispatches.Load())
	}
}

// TestAHintWhoseOnlyNodeRefusedItSaysNoNodeTookItAndCountsNoOverride: a node that refuses a dispatch has not taken the
// subtask. A refusal chain keeps the dial base of the node that answered last (so the retry note can name it) but clears
// the node and says in its error that none of them ran the subtask, and a refusal no other node is offered (a 401) is
// published as it came, naming the node that refused. The clause and the tally judge a result by where it was PLACED, so
// neither may read "placed on <the node that refused it>", on the result or on its ledger row, nor count as a hint that
// placement overrode.
func TestAHintWhoseOnlyNodeRefusedItSaysNoNodeTookItAndCountsNoOverride(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	localClause := "route=local was a hint (no pin_reason): placed as route=auto, and no node took it"
	remoteClause := "route=remote was a hint (no pin_reason): placed remotes-first, and no node took it"
	for _, tc := range []struct {
		name, route, clause string
		status              int
	}{
		{"a local hint, the node answers 409 and the chain ends", "local", localClause, http.StatusConflict},
		{"a remote hint, the node answers 409 and the chain ends", "remote", remoteClause, http.StatusConflict},
		{"a local hint, the node answers 401 and no other is offered", "local", localClause, http.StatusUnauthorized},
		{"a remote hint, the node answers 401 and no other is offered", "remote", remoteClause, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// the one node that declares `fast` refuses its dispatch, and this box declares no layer
			node, url := refusingNode(t, "fast-node", tc.status, func(f *fakeNode) { f.layers = oneCardRows(t) })
			cfg := pinCfg(t)
			cfg.AgentPlacementWaitSec = 5
			opts, tally := countedOpts("")
			results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), layerContracts(1, "fast"), tc.route, []string{url}, opts)
			if err != nil {
				t.Fatal(err)
			}
			pr := results[0]
			if sum.Failed != 1 || node.dispatches.Load() != 1 || !pr.refused || pr.ranBase == "" || pr.Err == "" {
				t.Fatalf("summary %+v, dispatches %d, refused %v, base %q, err %q, want one refused failure after one dispatch that kept its dial base", sum, node.dispatches.Load(), pr.refused, pr.ranBase, pr.Err)
			}
			if !strings.HasPrefix(pr.PlacementReason, tc.clause) {
				t.Errorf("placement = %q, want it to open with %q", pr.PlacementReason, tc.clause)
			}
			if strings.Contains(pr.PlacementReason, url) {
				t.Errorf("placement = %q names the dial base of the node that refused the subtask as where it was placed", pr.PlacementReason)
			}
			if placement, _ := pinRows(t, cfg.LedgerPath)[pr.JobID]["placement"].(string); !strings.HasPrefix(placement, tc.clause) {
				t.Errorf("ledger row placement = %q, want it to open with %q", placement, tc.clause)
			}
			if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 0 {
				t.Errorf("tally %+v, want 1 hint and no override: nothing was placed", st)
			}
		})
	}
}

// TestARemoteHintTurnedAwayByTheProcessGateReadsTheFleetBeforeItTakesTheIdleSeat: a remote hint is placed remotes-first.
// A subtask the process gate turned away has had no look at the fleet since the deal (its node is full for THIS process,
// which says nothing about the others), so the capacity wait reads the fleet once before it gives the subtask the idle
// seat; a fleet that has no room either hands it to the seat on the next tick. The controls are the ways into the wait
// that already know no remote has room (the deal's overflow), and a local hint, which is placed as auto places it: the
// idle seat first.
func TestARemoteHintTurnedAwayByTheProcessGateReadsTheFleetBeforeItTakesTheIdleSeat(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	remoteHint, localHint := pinCall{asked: "remote", hint: true}, pinCall{asked: "local", hint: true}
	gated := PlacedResult{waitCapacity: true, gated: true, pendingReason: "test: the dealt node was at its process gate"}
	overflow := PlacedResult{waitCapacity: true, overflow: true, pendingReason: "test: no remote had room at the deal"}
	hasRoom := withHeadroom(2, 0)
	isFull := func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning, f.maxQueueDepth, f.queueDepth, f.jobsQueued = 1, 1, 2, 2, 1
	}
	for _, tc := range []struct {
		name      string
		pin       pinCall
		seed      PlacedResult
		tune      func(*fakeNode)
		wantLocal bool
	}{
		{"a remote hint the gate turned away goes to the node that has room", remoteHint, gated, hasRoom, false},
		{"a remote hint the gate turned away takes the idle seat once the fleet has been read and has no room", remoteHint, gated, isFull, true},
		{"a remote hint the deal found no room for takes the idle seat at once", remoteHint, overflow, hasRoom, true},
		{"a local hint the gate turned away takes the idle seat first, as auto places it", localHint, gated, hasRoom, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, url := acceptingNode(t, "node-y", "answer from y", tc.tune)
			cfg := pinCfg(t)
			cfg.AgentPlacementWaitSec = 3
			var localCalls atomic.Int64
			r := &runner{cfg: cfg, route: "auto", pin: tc.pin, remotes: []string{url}, local: passingLocal(&localCalls)}
			pr := r.awaitCapacity(t.Context(), 0, plainContract(), time.Now(), 30, newPlacements(), tc.seed, nil, "")
			if pr.Err != "" || pr.Result.Deferred {
				t.Fatalf("result = err %q deferred %v (%q), want the subtask placed", pr.Err, pr.Result.Deferred, pr.Result.Reason)
			}
			switch {
			case tc.wantLocal && (localCalls.Load() != 1 || node.dispatches.Load() != 0):
				t.Errorf("local runs %d, node dispatches %d, want the idle seat to take it", localCalls.Load(), node.dispatches.Load())
			case !tc.wantLocal && (localCalls.Load() != 0 || node.dispatches.Load() != 1 || pr.Node != "node-y"):
				t.Errorf("local runs %d, node dispatches %d, node %q, want the node that has room to take it", localCalls.Load(), node.dispatches.Load(), pr.Node)
			}
		})
	}
}
