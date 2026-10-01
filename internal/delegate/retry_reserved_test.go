package delegate

// Register C-81: a retry never runs on a plain text reservation of the local cards, as a first
// placement on route auto or spread does not.
//
// A text lease taken with `gpu reserve --class text`, with no exclusive stamp and no drain, is a
// RESERVATION and not a fence. It keeps a first placement on route auto or spread off the local seat
// (auto waits for the holder, spread deals without the seat) and off replacementNode's last resort, but
// the affinity gate still admits a load onto the cards, so nothing refuses the run once it is
// dialled. alternativeNode asked the lease only whether it FENCES a new run (D-94), which a plain
// reservation does not, so the retry of a subtask whose first attempt had run on a fleet node was
// placed straight onto the cards the holder had reserved for a measurement: the verification retry,
// and the seat-down re-issue of ADR 0066, alike. attempt() takes a forced placement as it is given
// and reads no lease.
//
// These tests hold a PLAIN reservation. Exclusive and draining holds fence, and retry_fence_test.go
// pins what a retry does for them.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// reserveTheLocalSeat holds a plain text reservation and points the config at it. The premise is
// asserted, because a fixture that took a fence by accident would pass on D-94's code and prove
// nothing about this row.
func reserveTheLocalSeat(t *testing.T, cfg *config.Config) gpulease.Info {
	t.Helper()
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg.GPULockPath = dir
	info := LocalLease(cfg.GPULockPath, cfg.StateDir)
	if fenced, why := Fenced(info); !Reserved(info) || fenced || info.Exclusive || info.Draining {
		t.Fatalf("fixture: want a plain text reservation (reserved, not fenced, not exclusive, not draining), got %+v fenced=%v (%s)", info, fenced, why)
	}
	return info
}

// reservedRetryLocal is a local seat whose answer passes verifiedContract's acceptance, so a retry
// that is wrongly placed on it recovers the subtask and the call count says where it ran.
func reservedRetryLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "verified locally", Structured: json.RawMessage(`{"answer":"verified"}`), StopReason: "done"}, nil
	}
}

// retryFirstAttempts are the first attempts that reach alternativeNode with the local seat as a
// candidate for the retry: a node answered and its answer failed the contract's acceptance, and a
// node reported that its seat went down under the run. Both have run on a fleet node, which is the
// one case in which a retry may be placed locally.
var retryFirstAttempts = []struct {
	name  string
	first func(node string) core.AgentWireResult
}{
	{"a failed verification", func(node string) core.AgentWireResult {
		w := remoteWire("wrong answer", `{"answer":"wrong answer"}`)
		w.NodeID = node
		return w
	}},
	{"a seat-down defer", func(node string) core.AgentWireResult { return seatDownWire(node, 6) }},
}

// TestARetryLeavesAPlainlyReservedLocalSeatForAnUntriedRemote: the first attempt ran on a fleet node
// and must be tried again, the local seat is under a plain text reservation, and an untried fleet
// node is eligible and idle. The retry goes to that node. The local runner counts calls instead of
// failing the test, so a retry that lands on the reserved seat is reported with what it ran.
func TestARetryLeavesAPlainlyReservedLocalSeatForAnUntriedRemote(t *testing.T) {
	for _, fa := range retryFirstAttempts {
		for _, route := range []string{"auto", "spread"} {
			t.Run(fa.name+" on route "+route, func(t *testing.T) {
				compressPolls(t, 10*time.Millisecond, 2*time.Second)
				nodeA, urlA := eligibleNode(t, "node-a", "unused")
				nodeB, urlB := eligibleNode(t, "node-b", "unused")
				scriptFirstJob(t, fa.first, "verified by the second node", nodeA, nodeB)
				cfg := testCfg(t)
				reserveTheLocalSeat(t, &cfg)
				var localCalls atomic.Int64

				results, sum, err := Run(context.Background(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, route, []string{urlA, urlB})
				if err != nil {
					t.Fatal(err)
				}
				pr := results[0]
				if localCalls.Load() != 0 {
					t.Fatalf("the retry ran on the reserved local seat (%d local calls) while an untried fleet node was idle; retried_on=%q placement=%q note=%q",
						localCalls.Load(), pr.RetriedOn, pr.PlacementReason, pr.RetryNote)
				}
				if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
					t.Fatalf("dispatches A=%d B=%d, want the first attempt on one fleet node and the retry on the other; note=%q",
						nodeA.dispatches.Load(), nodeB.dispatches.Load(), pr.RetryNote)
				}
				if sum.Retried != 1 || sum.RetryRecovered != 1 || pr.Result.Deferred {
					t.Fatalf("summary = %+v deferred=%v note=%q reason=%q, want the retry to have recovered the subtask", sum, pr.Result.Deferred, pr.RetryNote, pr.Result.Reason)
				}
				if !strings.Contains(pr.PlacementReason, "the local seat is reserved") || !strings.Contains(pr.PlacementReason, "class=text") {
					t.Errorf("the placement reason must say WHY the retry skipped the local seat and name the holder, got %q", pr.PlacementReason)
				}
			})
		}
	}
}

// TestARetryIsSkippedNotRunLocallyWhenOnlyAReservedLocalSeatIsLeft: with no untried fleet node the
// retry is not placed at all. It does not run on the reserved cards, it does not wait out the
// holder, and the published result stays the first attempt with a retry_note that names the holder.
// This is what replacementNode answers for a reserved local seat (no placement, and a sentence that
// names the holder); a retry has no capacity wait to hand that answer to, so it is the end of the
// retry, exactly as it is for a fenced seat.
func TestARetryIsSkippedNotRunLocallyWhenOnlyAReservedLocalSeatIsLeft(t *testing.T) {
	for _, fa := range retryFirstAttempts {
		t.Run(fa.name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "unused")
			scriptFirstJob(t, fa.first, "verified by the second node", nodeA)
			cfg := testCfg(t)
			reserveTheLocalSeat(t, &cfg)
			var localCalls atomic.Int64

			start := time.Now()
			results, sum, err := Run(context.Background(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "spread", []string{urlA})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			pr := results[0]
			if localCalls.Load() != 0 {
				t.Fatalf("the retry ran on the reserved local seat (%d local calls); retried_on=%q placement=%q note=%q",
					localCalls.Load(), pr.RetriedOn, pr.PlacementReason, pr.RetryNote)
			}
			if sum.Retried != 0 || pr.RetriedOn != "" || nodeA.dispatches.Load() != 1 {
				t.Fatalf("nothing was left to retry on: retried=%d retried_on=%q node-a dispatches=%d note=%q", sum.Retried, pr.RetriedOn, nodeA.dispatches.Load(), pr.RetryNote)
			}
			// The first attempt stands as it was: the failed verification, or the seat-down defer
			// that was promised a second placement and is told why it did not get one.
			switch {
			case fa.name == "a seat-down defer" && !SeatDownDefer(pr.Result):
				t.Fatalf("published reason = %q, want the seat-down defer the first attempt produced", pr.Result.Reason)
			case fa.name == "a failed verification" && len(pr.AcceptanceFailures) == 0:
				t.Fatalf("published result carries no acceptance failure: %+v", pr.Result)
			}
			lease := LocalLease(cfg.GPULockPath, cfg.StateDir)
			want := "retry skipped: the local seat is reserved (" + HolderLine(lease) +
				") and no other node is eligible; a retry placed there would run on the cards the holder reserved"
			if pr.RetryNote != want {
				t.Fatalf("retry_note = %q, want %q", pr.RetryNote, want)
			}
			if strings.Contains(pr.RetryNote, "affinity cordon") {
				t.Fatalf("retry_note = %q: a plain reservation is admitted at the affinity gate, so the cordon wait is not why the retry was refused", pr.RetryNote)
			}
			// The reservation is read from the lease record, never discovered by waiting for the holder:
			// a measurement can hold the cards for hours.
			if elapsed > 30*time.Second {
				t.Fatalf("the retry took %s: it waited for the holder instead of reading the lease", elapsed.Round(time.Second))
			}
		})
	}
}

// TestARetryStillRunsOnAReservedLocalSeatForTheHoldersOwnChild: the reservation keeps the others
// off the cards, never the holder's own measured work. `gpu reserve --class text -- <delegate>` runs
// the delegator as the holder's child with GPU_LEASE_EPOCH set to the held epoch, and Reserved
// exempts it for a first placement; a retry reads the same predicate, so it keeps the seat too. The
// control the check must not break, and the one that fails if the check is written against the
// lease's class and not against Reserved.
func TestARetryStillRunsOnAReservedLocalSeatForTheHoldersOwnChild(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "wrong answer") // fails acceptance -> retryable
	cfg := testCfg(t)
	info := reserveTheLocalSeat(t, &cfg)
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch, 10))
	var localCalls atomic.Int64

	// route=auto, not remote: the held lease reads the local seat as busy, so the deal sends the first
	// attempt to the fleet node, and the inherited lease exempts the seat for the retry. Route remote
	// would also land the retry here, but only because alternativeNode has no route=remote guard in its
	// "first ran on a fleet node" branch (replacementNode refuses to fall local on route=remote), so a
	// control on it would turn red the day that is fixed, for a reason that has nothing to do with C-81.
	results, sum, err := Run(context.Background(), cfg, reservedRetryLocal(&localCalls), []core.AgentContract{verifiedContract()}, "auto", []string{urlA})
	if err != nil {
		t.Fatal(err)
	}
	if nodeA.dispatches.Load() != 1 {
		t.Fatalf("the first attempt must have run on the fleet node: node-a dispatches=%d, local calls=%d, summary=%+v", nodeA.dispatches.Load(), localCalls.Load(), sum)
	}
	if localCalls.Load() != 1 || sum.Retried != 1 || sum.RetryRecovered != 1 {
		t.Fatalf("local calls=%d summary=%+v note=%q, want the holder's own child to retry on the seat it reserved", localCalls.Load(), sum, results[0].RetryNote)
	}
}

// TestAReservedLocalSeatDoesNotOutrankTheCallDeadlineInTheRetryNote: a reservation is told the way a
// fence is when the call's deadline (ADR 0065) has ended the read of the fleet. The first attempt
// answers with a seat-down defer in time, so a re-placement on another node is owed, and the fleet's
// health turns slow once it has answered, so the deadline ends the read that would have named a node.
// The note leads with the deadline, keeps the reservation behind it (read from the lease record, so it
// stays true) with the reservation's own reason, and says nothing of a fleet it never read. The
// retry still does not run on the reserved seat: the lease was read before the fleet was.
func TestAReservedLocalSeatDoesNotOutrankTheCallDeadlineInTheRetryNote(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	script := &firstJobScript{}
	var slow atomic.Bool
	tune := func(id string) func(*fakeNode) {
		return func(f *fakeNode) {
			inner := script.poll(t, id, seatDownWire(id, 6), runningForever)
			f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				slow.Store(true) // the fleet's health turns slow once the first attempt has answered
				return inner(jobID, n)
			}
			f.healthDelayFn = func() time.Duration {
				if slow.Load() {
					return 30 * time.Second
				}
				return 0
			}
		}
	}
	_, urlA := acceptingNode(t, "node-a", "unused", tune("node-a"))
	_, urlB := acceptingNode(t, "node-b", "unused", tune("node-b"))
	cfg := testCfg(t)
	reserveTheLocalSeat(t, &cfg)
	var localCalls atomic.Int64

	results, sum, _ := runWithin(t, 15*time.Second, cfg, reservedRetryLocal(&localCalls),
		[]core.AgentContract{plainContract()}, "remote", []string{urlA, urlB}, deadlineIn(1500*time.Millisecond), nil)
	pr := results[0]

	if localCalls.Load() != 0 {
		t.Fatalf("the retry ran on the reserved local seat (%d local calls); note=%q placement=%q", localCalls.Load(), pr.RetryNote, pr.PlacementReason)
	}
	if !SeatDownDefer(pr.Result) || pr.deadlineCut || pr.retried || sum.Deferred != 1 {
		t.Fatalf("summary %+v cut %v retried %v reason %q, want the seat-down defer published as it was, with no retry (premise)", sum, pr.deadlineCut, pr.retried, pr.Result.Reason)
	}
	lease := LocalLease(cfg.GPULockPath, cfg.StateDir)
	want := "retry skipped: " + callDeadlinePrefix + " before a retry node was chosen; the local seat is reserved (" + HolderLine(lease) +
		"), and a retry placed there would run on the cards the holder reserved"
	if pr.RetryNote != want {
		t.Fatalf("retry_note = %q, want %q", pr.RetryNote, want)
	}
	if strings.Contains(pr.RetryNote, "no other node is eligible") {
		t.Fatalf("retry_note = %q accuses nodes the deadline kept the delegator from asking", pr.RetryNote)
	}
}

// TestRetryHeldWhyTellsAReservationFromAFence pins the one coupling in the retry note. alternativeNode
// returns the lease clause as text, and retryHeldWhy picks the closing reason of the note by the
// clause's opening (localReservedPrefix): a reservation is told with its own reason and a fence with
// the fence's. If reservedClause were edited to open some other way, the note would fall back to the
// fence's explanation, a wait at the affinity cordon, which is false for a reservation (the affinity
// gate admits the load). The two exact-note tests would catch that as a mismatch of a long string;
// this one names the coupling.
func TestRetryHeldWhyTellsAReservationFromAFence(t *testing.T) {
	holder := gpulease.Info{Held: true, Class: gpulease.ClassText, Epoch: 7, PID: 4242, Reason: "weights A/B"}

	clause := reservedClause(holder)
	if !strings.HasPrefix(clause, localReservedPrefix) {
		t.Fatalf("reservedClause = %q does not open with localReservedPrefix %q: retryHeldWhy tells a reservation from a fence by that opening, so the retry_note would end on the fence's reason (a wait at the affinity cordon), which a plain reservation never meets", clause, localReservedPrefix)
	}
	if got := retryHeldWhy(clause); got != retryReservedWhy {
		t.Errorf("retryHeldWhy(%q) = %q, want the reservation's reason %q", clause, got, retryReservedWhy)
	}

	fenceInfo := gpulease.Info{Held: true, Class: gpulease.ClassText, Epoch: 7, PID: 4242, Draining: true}
	fenced, fence := Fenced(fenceInfo)
	if !fenced {
		t.Fatalf("fixture: a draining text lease must fence, got fenced=%v (%s)", fenced, fence)
	}
	// The fence's clause as alternativeNode words it: it must never be mistaken for a reservation.
	fenceClause := "the local seat is fenced (" + fence + " — " + HolderLine(fenceInfo) + ")"
	if got := retryHeldWhy(fenceClause); got != retryFenceWhy {
		t.Errorf("retryHeldWhy(%q) = %q, want the fence's reason %q", fenceClause, got, retryFenceWhy)
	}
}

// TestAReservedSeatReadsTheSameInARefusedReplacementAndASkippedRetry: replacementNode ends a refused
// re-placement with the sentence a skipped retry names for the same seat (reservedClause), and each
// of them writes it itself, so nothing but this test keeps the two copies from drifting apart. The
// first placement's own reasons ("local seat reserved (...); no eligible remote - ...") are a
// different shape and are not held to it.
func TestAReservedSeatReadsTheSameInARefusedReplacementAndASkippedRetry(t *testing.T) {
	cfg := testCfg(t)
	info := reserveTheLocalSeat(t, &cfg)
	r := &runner{cfg: cfg, route: "auto"}

	_, why, ok := r.replacementNode(t.Context(), plainContract(), newPlacements(), 1)
	if ok {
		t.Fatalf("replacementNode placed the subtask on a reserved local seat: %q", why)
	}
	if want := ", and " + reservedClause(info); !strings.HasSuffix(why, want) {
		t.Fatalf("replacementNode ended its refusal with %q, want it to end with %q: the clause a skipped retry names for the same seat", why, want)
	}
}
