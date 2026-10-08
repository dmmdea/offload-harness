// pin_retry_test.go: a reasoned remote pin is authoritative to the end of the subtask (ADR 0078 decision 3).
//
// The first placement of a reasoned remote pin was always obeyed: no eligible remote defers, and a refused dispatch never
// falls back to the local seat. Its second chance was not. alternativeNode sent the retry of a first attempt that ran on a
// fleet node (a failed verification, an abstention, an admission defer, a seat-down defer) to the local seat, so a call
// pinned to a node under pin_reason measurement or operator published the local seat's answer with route=remote and the
// pin_reason on its rows, and offload_status counted it as a placement that obeyed. The retry of a reasoned remote pin is
// another fleet node, or none. The bare remote route of a caller with no reason channel keeps remote -> local, and so
// does a reasonless remote hint, which is placed as auto.

package delegate

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// remotePinFirstAttempts are the first attempts that retryable() sends to alternativeNode when the attempt ran on a fleet
// node: the node answered and the answer failed the contract's acceptance, it abstained, its own seat was caught at
// admission, and its seat went down under the run. Each one reaches the branch that used to return the local seat.
var remotePinFirstAttempts = []struct {
	name string
	// kept says what the published result must still be when the retry did not run
	kept  func(PlacedResult) bool
	first func(node string) core.AgentWireResult
}{
	{"a failed verification", func(pr PlacedResult) bool { return len(pr.AcceptanceFailures) > 0 },
		func(node string) core.AgentWireResult {
			w := remoteWire("wrong answer", `{"answer":"wrong answer"}`)
			w.NodeID = node
			return w
		}},
	{"an abstention", func(pr PlacedResult) bool {
		return pr.Result.Deferred && pr.Result.DeferClass == core.DeferClassAbstention
	}, func(node string) core.AgentWireResult {
		w := remoteWire("", "")
		w.NodeID = node
		w.Deferred, w.DeferClass, w.Reason = true, core.DeferClassAbstention, "output failed schema: missing required field answer"
		return w
	}},
	{"an admission defer", func(pr PlacedResult) bool { return IncoherentSeatDefer(pr.Result) },
		func(node string) core.AgentWireResult {
			w := remoteWire("", "")
			w.NodeID = node
			w.Output, w.Structured, w.StopReason = "", nil, "error"
			w.Deferred, w.DeferClass, w.Reason = true, core.DeferClassInfrastructure, core.IncoherentSeatReason+"the completion is empty at the 96-token cap"
			return w
		}},
	{"a seat-down defer", func(pr PlacedResult) bool { return SeatDownDefer(pr.Result) },
		func(node string) core.AgentWireResult { return seatDownWire(node, 6) }},
}

// TestAReasonedRemotePinIsNeverRetriedOnTheLocalSeat: the pinned node's first answer is retryable and the local seat is
// idle, unleased and able to answer correctly. With no other node the retry is not run, the published result is the first
// attempt, it still names the node it ran on and the pin_reason, and the retry_note names the pin. Nothing ran locally,
// and the ledger holds the one row of the one attempt.
func TestAReasonedRemotePinIsNeverRetriedOnTheLocalSeat(t *testing.T) {
	for _, reason := range []string{PinMeasurement, PinOperator} {
		for _, fa := range remotePinFirstAttempts {
			t.Run(reason+"/"+fa.name, func(t *testing.T) {
				compressPolls(t, 10*time.Millisecond, 2*time.Second)
				nodeA, urlA := eligibleNode(t, "node-a", "unused")
				scriptFirstJob(t, fa.first, "verified by the second node", nodeA)
				cfg := pinCfg(t)
				var localCalls atomic.Int64
				opts, tally := countedOpts(reason)
				results, sum, err := RunWith(t.Context(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA}, opts)
				if err != nil {
					t.Fatal(err)
				}
				pr := results[0]
				if localCalls.Load() != 0 || pr.ranLocal {
					t.Fatalf("the retry of a call pinned to a fleet node ran on the local seat (%d local calls, ran local %v); placement=%q note=%q",
						localCalls.Load(), pr.ranLocal, pr.PlacementReason, pr.RetryNote)
				}
				if nodeA.dispatches.Load() != 1 || sum.Retried != 0 || sum.RetryRecovered != 0 || pr.RetriedOn != "" {
					t.Fatalf("node dispatches %d, summary %+v, retried_on %q, want the one attempt and no retry", nodeA.dispatches.Load(), sum, pr.RetriedOn)
				}
				if pr.Node != "node-a" || !fa.kept(pr) {
					t.Fatalf("published node %q, result %+v, want the first attempt on node-a as it came", pr.Node, pr.Result)
				}
				if pr.PinReason != reason {
					t.Errorf("pin_reason = %q, want %q", pr.PinReason, reason)
				}
				want := "route=remote is pinned (pin_reason " + reason + "), which places nothing on the local seat"
				if !strings.Contains(pr.RetryNote, want) || !strings.HasPrefix(pr.RetryNote, "retry skipped: ") {
					t.Errorf("retry_note = %q, want a skipped retry that says %q", pr.RetryNote, want)
				}
				rows := pinRows(t, cfg.LedgerPath)
				if len(rows) != 1 {
					t.Errorf("the ledger holds %d finished rows, want the one of the one attempt", len(rows))
				}
				for id, row := range rows {
					if row["route"] != "remote" || row["route_asked"] != "remote" || row["pin_reason"] != reason {
						t.Errorf("row %s = route %v / asked %v / reason %v, want remote / remote / %s", id, row["route"], row["route_asked"], row["pin_reason"], reason)
					}
				}
				if st := tally.Snapshot(); st.Reasoned[reason] != 1 || st.Hints != 0 {
					t.Errorf("tally %+v, want the subtask counted once under %s", st, reason)
				}
			})
		}
	}
}

// TestAReasonedRemotePinRetriesOnAnotherFleetNodeWhenThereIsOne: the same first attempts with a second idle eligible node.
// The retry goes there, never to the seat, and the published result is the second node's, a remote, with the pin_reason
// intact and the placement saying the pin is why the retry stayed on a node.
func TestAReasonedRemotePinRetriesOnAnotherFleetNodeWhenThereIsOne(t *testing.T) {
	for _, reason := range []string{PinMeasurement, PinOperator} {
		for _, fa := range remotePinFirstAttempts {
			t.Run(reason+"/"+fa.name, func(t *testing.T) {
				compressPolls(t, 10*time.Millisecond, 2*time.Second)
				nodeA, urlA := eligibleNode(t, "node-a", "unused")
				nodeB, urlB := eligibleNode(t, "node-b", "unused")
				scriptFirstJob(t, fa.first, "verified by the second node", nodeA, nodeB)
				cfg := pinCfg(t)
				var localCalls atomic.Int64
				opts, tally := countedOpts(reason)
				results, sum, err := RunWith(t.Context(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA, urlB}, opts)
				if err != nil {
					t.Fatal(err)
				}
				pr := results[0]
				if localCalls.Load() != 0 || pr.ranLocal {
					t.Fatalf("the retry ran on the local seat (%d local calls) while a second fleet node sat idle; placement=%q note=%q", localCalls.Load(), pr.PlacementReason, pr.RetryNote)
				}
				if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
					t.Fatalf("dispatches A=%d B=%d, want the first attempt on one node and the retry on the other; note=%q", nodeA.dispatches.Load(), nodeB.dispatches.Load(), pr.RetryNote)
				}
				if sum.Retried != 1 || sum.RetryRecovered != 1 || pr.Result.Deferred || len(pr.AcceptanceFailures) > 0 {
					t.Fatalf("summary %+v, deferred %v, failures %v, want the retry to have recovered the subtask", sum, pr.Result.Deferred, pr.AcceptanceFailures)
				}
				if (pr.Node != "node-a" && pr.Node != "node-b") || pr.RetriedOn != pr.Node || pr.Result.Output != "verified by the second node" {
					t.Errorf("published node %q retried_on %q output %q, want the second fleet node's answer", pr.Node, pr.RetriedOn, pr.Result.Output)
				}
				if !strings.HasPrefix(pr.RetryNote, "first attempt on ") || !strings.HasSuffix(pr.RetryNote, "; this result is the retry; route=remote is pinned (pin_reason "+reason+"), which places nothing on the local seat") {
					t.Errorf("retry_note = %q, want the first attempt's outcome and then the pin that kept the retry on a fleet node", pr.RetryNote)
				}
				if pr.PinReason != reason {
					t.Errorf("pin_reason = %q, want %q", pr.PinReason, reason)
				}
				if want := "route=remote is pinned (pin_reason " + reason + "), which places nothing on the local seat"; !strings.Contains(pr.PlacementReason, "retry on "+pr.Node) || !strings.Contains(pr.PlacementReason, want) {
					t.Errorf("placement = %q, want the retry on %s and the pin named", pr.PlacementReason, pr.Node)
				}
				rows := pinRows(t, cfg.LedgerPath)
				if len(rows) != 2 {
					t.Errorf("the ledger holds %d finished rows, want one per attempt", len(rows))
				}
				for id, row := range rows {
					if row["route"] != "remote" || row["pin_reason"] != reason {
						t.Errorf("row %s = route %v / reason %v, want remote / %s: both attempts ran on a node", id, row["route"], row["pin_reason"], reason)
					}
				}
				if st := tally.Snapshot(); st.Reasoned[reason] != 1 {
					t.Errorf("tally %+v, want the retried subtask counted once under %s", st, reason)
				}
			})
		}
	}
}

// TestAReasonedRemotePinWhoseRetryFailsTooStaysOffTheLocalSeat: two nodes that both answer wrong. The retry goes to the second
// node and not to the seat, it fails too, so the published result is the first attempt, and its note says the retry failed and
// that the pin is why it ran where it did.
func TestAReasonedRemotePinWhoseRetryFailsTooStaysOffTheLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "wrong answer from A")
	nodeB, urlB := eligibleNode(t, "node-b", "wrong answer from B")
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts(PinMeasurement)
	results, sum, err := RunWith(t.Context(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA, urlB}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if localCalls.Load() != 0 || pr.ranLocal || nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("local runs %d, ran local %v, dispatches A=%d B=%d, want one attempt on each node and none on the seat", localCalls.Load(), pr.ranLocal, nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	if sum.Retried != 1 || sum.RetryRecovered != 0 || sum.FailedVerification != 1 || len(pr.AcceptanceFailures) == 0 || pr.PinReason != PinMeasurement {
		t.Fatalf("summary %+v, failures %v, pin_reason %q, want the first attempt published as a failed verification that was retried once", sum, pr.AcceptanceFailures, pr.PinReason)
	}
	if !strings.Contains(pr.RetryNote, " also failed_verification") || !strings.HasSuffix(pr.RetryNote, "; this result is the first attempt; route=remote is pinned (pin_reason measurement), which places nothing on the local seat") {
		t.Errorf("retry_note = %q, want the failed retry and then the pin", pr.RetryNote)
	}
	if st := tally.Snapshot(); st.Reasoned[PinMeasurement] != 1 {
		t.Errorf("tally %+v, want the subtask counted once under measurement", st)
	}
}

// TestAReasonedRemotePinNamesThePinAndNotTheLeaseWhenTheLocalSeatIsReserved: the local seat is not a candidate for a pinned call,
// so a lease that holds it is not why the retry did not run. The pin is checked before the lease questions, and the note
// names the pin and not the reservation (a retry that did consult the lease would say "the local seat is reserved").
func TestAReasonedRemotePinNamesThePinAndNotTheLeaseWhenTheLocalSeatIsReserved(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	scriptFirstJob(t, remotePinFirstAttempts[0].first, "verified by the second node", nodeA)
	cfg := pinCfg(t)
	reserveTheLocalSeat(t, &cfg)
	var localCalls atomic.Int64
	results, _, err := RunWith(t.Context(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if localCalls.Load() != 0 || nodeA.dispatches.Load() != 1 {
		t.Fatalf("local calls %d, node dispatches %d, want the one attempt on the node", localCalls.Load(), nodeA.dispatches.Load())
	}
	if !strings.Contains(pr.RetryNote, "route=remote is pinned (pin_reason operator)") || strings.Contains(pr.RetryNote, "reserved") {
		t.Errorf("retry_note = %q, want the pin named and the reservation left out: the seat was never a candidate", pr.RetryNote)
	}
}

// TestARemoteHintAndTheBareRemoteRouteStillRetryOnTheLocalSeat: the controls the pin's guard must not break. A reasonless
// remote hint is placed as auto and a bare remote route (a caller with no reason channel: fleet-smoke, the review lane,
// offload_ask, agent_run) is a pin without a reason; both keep the retry they always had, remote -> local, which the
// system doc states and TestRunRetryRemoteFailureFallsBackToLocal pins for the bare route.
func TestARemoteHintAndTheBareRemoteRouteStillRetryOnTheLocalSeat(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       func() (*RunOptions, *PinTally)
		wantHints  int
		wantUnreas int
	}{
		{"a reasonless remote hint", func() (*RunOptions, *PinTally) { return countedOpts("") }, 1, 0},
		{"the bare remote route of a caller with no reason channel", func() (*RunOptions, *PinTally) {
			tally := NewPinTally()
			return &RunOptions{PinTally: tally}, tally
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "wrong answer")
			cfg := pinCfg(t)
			var localCalls atomic.Int64
			opts, tally := tc.opts()
			results, sum, err := RunWith(t.Context(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA}, opts)
			if err != nil {
				t.Fatal(err)
			}
			pr := results[0]
			if localCalls.Load() != 1 || !pr.ranLocal || nodeA.dispatches.Load() != 1 || sum.Retried != 1 || sum.RetryRecovered != 1 {
				t.Fatalf("local calls %d, ran local %v, node dispatches %d, summary %+v, want the wrong answer retried on the idle seat and recovered", localCalls.Load(), pr.ranLocal, nodeA.dispatches.Load(), sum)
			}
			if pr.PinReason != "" || strings.Contains(pr.PlacementReason, "is pinned") || strings.Contains(pr.RetryNote, "is pinned") {
				t.Errorf("pin_reason %q, placement %q, note %q: a call with no reason is not a reasoned pin", pr.PinReason, pr.PlacementReason, pr.RetryNote)
			}
			if st := tally.Snapshot(); st.Hints != tc.wantHints || st.Unreasoned != tc.wantUnreas {
				t.Errorf("tally %+v, want %d hint(s) and %d unreasoned", st, tc.wantHints, tc.wantUnreas)
			}
		})
	}
}

// TestRemotePinnedIsTheReasonedRemoteRouteAndNothingElse is the guard's predicate as a table. A reasoned LOCAL pin cannot
// reach the branch the guard sits in (it never runs on a node), so the end-to-end tests above cannot tell the predicate
// that also asks for the remote route from one that asks for the reason alone; this can.
func TestRemotePinnedIsTheReasonedRemoteRouteAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  pinCall
		want bool
	}{
		{"a remote pin under measurement", pinCall{asked: "remote", reason: PinMeasurement}, true},
		{"a remote pin under operator", pinCall{asked: "remote", reason: PinOperator}, true},
		{"a remote hint", pinCall{asked: "remote", hint: true}, false},
		{"the bare remote route", pinCall{asked: "remote"}, false},
		{"a local pin under privacy", pinCall{asked: "local", reason: PinPrivacy}, false},
		{"a local pin under measurement", pinCall{asked: "local", reason: PinMeasurement}, false},
		{"a local hint", pinCall{asked: "local", hint: true}, false},
		{"no pin", pinCall{}, false},
	} {
		if got := (&runner{pin: tc.pin}).remotePinned(); got != tc.want {
			t.Errorf("%s: remotePinned() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
