package delegate

// Patience is the time the call has left (ADR 0073, the diagnosis' F07).
//
// How long a job may wait to START on a node was the contract's poll budget (ADR 0063 decision 5):
// 660-1,260 s for a timeout_auto contract. A call has 1,500 s, so late in a call a node whose
// backlog outlasted the call still passed the gate. The job was dealt, the call ended first, the
// delegator cancelled, and a job the node had already started kept running there with nobody
// waiting for it (17 call-deadline cuts in one day, 11 of them with the job still on a node).
// The patience is now clamped to what the call has left less the reserve, and the reason prints
// the arithmetic: the number it shows is the call's, and it says so.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// callIn is a runner whose call has this long to run.
func callIn(t *testing.T, d time.Duration) *runner {
	t.Helper()
	call := newCallDeadline(context.Background(), &RunOptions{Deadline: time.Now().Add(d)}, 1)
	return &runner{route: "remote", cfg: testCfg(t), call: call}
}

// backloggedNode is an eligible node that has room (it will take a dispatch, and the deal sees
// headroom on it) but publishes that a new job waits eta seconds to start: inside a 300 s
// contract's patience (360 s with the grace), far outside a call that has 40 s left. The headroom
// is nominal on purpose: a deal only reaches the start-wait gate for a node it could otherwise
// deal to, and the node's own published estimate is preferred outright (ADR 0050 decision 3).
func backloggedNode(eta float64) NodeView {
	v := eligibleRemote()
	v.NodeID = "backlogged-node"
	v.AgentCtxTokens = 32768
	v.MaxConcurrentJobs, v.JobsRunning, v.JobsQueued = 4, 2, 0
	v.QueueDepth, v.MaxQueueDepth = 2, 8
	v.QueueWaitEstimateSec = &eta
	return v
}

// TestPatienceIsClampedToWhatTheCallHasLeft is the rule itself, one row per way it can come out.
func TestPatienceIsClampedToWhatTheCallHasLeft(t *testing.T) {
	withCallReserve(t, 10*time.Second)
	st := oneStepSchemaContract(300, false)
	v := backloggedNode(200)
	full := patienceFor(st.Contract, v)

	if p, note := (&runner{}).patience(st.Contract, v); p != full || note != "" {
		t.Fatalf("no call deadline: patience %s note %q, want the poll budget %s untouched", p, note, full)
	}
	if p, note := callIn(t, time.Hour).patience(st.Contract, v); p != full || note != "" {
		t.Fatalf("a call an hour from its end: patience %s note %q, want the poll budget %s untouched", p, note, full)
	}

	p, note := callIn(t, 40*time.Second).patience(st.Contract, v)
	if p > 30*time.Second || p < 29*time.Second {
		t.Fatalf("a call with 40 s left under a 10 s reserve: patience %s, want about 30 s", p)
	}
	for _, want := range []string{"the call's deadline is", "reserved for a placed job to run in", "poll budget is " + full.Round(time.Second).String()} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to contain %q", note, want)
		}
	}

	// A call with no more than the reserve left has nothing to start a job in. The clamp is
	// never zero, because startsWithinPatience reads zero as "no bound at all".
	for _, left := range []time.Duration{5 * time.Second, -time.Second} {
		p, note := callIn(t, left).patience(st.Contract, v)
		if p <= 0 || p > time.Millisecond || note == "" {
			t.Fatalf("a call with %s left: patience %s note %q, want the tightest positive bound with its note", left, p, note)
		}
		if ok, _ := startsWithinPatience(v, p); ok {
			t.Fatalf("a call with %s left let a node with a 200 s backlog through", left)
		}
		if ok, _ := startsWithinPatience(eligibleRemote(), p); !ok {
			t.Fatalf("a call with %s left held out a node that publishes no estimate: unknown is no opinion", left)
		}
	}
}

// TestTheAutoDealHoldsBackANodeThatCannotStartBeforeTheCallEnds: the node would start the job in
// 200 s, which is inside the contract's patience and far outside a call that has 40 s left. With
// no call deadline it is dealt (the control); under one it is held out, the subtask goes to the
// capacity wait, and the reason prints the call's arithmetic.
func TestTheAutoDealHoldsBackANodeThatCannotStartBeforeTheCallEnds(t *testing.T) {
	withCallReserve(t, 10*time.Second)
	st := oneStepSchemaContract(300, false)
	slow := backloggedNode(200)
	fast := eligibleRemote()
	fast.NodeID = "fast-node"
	views, bases := []NodeView{slow, fast}, []string{"http://slow", "http://fast"}

	control := &runner{route: "remote", cfg: testCfg(t)}
	if slot := control.placeAutoRemote("seed", st, localNode(), views[:1], bases[:1], true, map[string]int{}, nil); slot.capacityWait || slot.base != "http://slow" {
		t.Fatalf("control (no call deadline): slot %+v, want the backlogged node dealt, as before", slot.placement)
	}

	r := callIn(t, 40*time.Second)
	slot := r.placeAutoRemote("seed", st, localNode(), views, bases, true, map[string]int{}, nil)
	if slot.capacityWait || slot.base != "http://fast" {
		t.Fatalf("slot %+v, want the deal on fast-node: the backlogged node cannot start the job before the call ends", slot.placement)
	}
	for _, want := range []string{"backlogged-node: backlog (", "200 s", "the call's deadline is", "less the 10s reserved"} {
		if !strings.Contains(slot.reason, want) {
			t.Errorf("reason = %q, want the held-out node narrated with %q", slot.reason, want)
		}
	}
	only := r.placeAutoRemote("seed", st, localNode(), views[:1], bases[:1], true, map[string]int{}, nil)
	if !only.capacityWait {
		t.Fatalf("slot %+v, want the capacity wait: the only node cannot start the job before the call ends", only.placement)
	}
}

// TestTheSpreadDealHoldsBackANodeThatCannotStartBeforeTheCallEnds is the same rule for
// route=spread's rotation.
func TestTheSpreadDealHoldsBackANodeThatCannotStartBeforeTheCallEnds(t *testing.T) {
	withCallReserve(t, 10*time.Second)
	slow := backloggedNode(200)
	fast := fitMidRemote
	fast.NodeID = "fast-node"
	contracts := make([]core.AgentContract, 4)
	for i := range contracts {
		contracts[i] = fitSubtask(fitMechGoal, 100).Contract
		contracts[i].TimeoutSec = 300
	}
	dealt := func(r *runner) (toSlow int, reasons string) {
		r.spreadLease.Held = true // a text lease takes the local seat out of the rotation
		r.spreadLease.Class = "text"
		for _, sl := range r.dealSpread(contracts, fitLocal()) {
			if sl.view.NodeID == "backlogged-node" {
				toSlow++
			}
			reasons += sl.reason + "\n"
		}
		return toSlow, reasons
	}

	if n, _ := dealt(fitRunner(slow, fast)); n == 0 {
		t.Fatal("control (no call deadline): the backlogged node was dealt nothing, so this test would pass without the clamp")
	}
	r := fitRunner(slow, fast)
	r.call = newCallDeadline(context.Background(), &RunOptions{Deadline: time.Now().Add(40 * time.Second)}, 1)
	n, reasons := dealt(r)
	if n != 0 {
		t.Fatalf("the spread deal gave the backlogged node %d subtask(s): it cannot start one before the call ends", n)
	}
	if !strings.Contains(reasons, "the call's deadline is") {
		t.Errorf("reasons = %q, want the held-out node's reason to print the call's arithmetic", reasons)
	}
}

// TestReplacementHoldsBackANodeThatCannotStartBeforeTheCallEnds: a refused subtask is re-placed only
// on a node that can start it before the call ends, and the busy line says why.
func TestReplacementHoldsBackANodeThatCannotStartBeforeTheCallEnds(t *testing.T) {
	withCallReserve(t, 10*time.Second)
	st := oneStepSchemaContract(300, false)
	views, bases := []NodeView{backloggedNode(200)}, []string{"http://slow"}

	if v, _, busy := (&runner{cfg: testCfg(t)}).withRoom(st, views, bases); len(v) != 1 || len(busy) != 0 {
		t.Fatalf("control (no call deadline): candidates %d busy %v, want the node a candidate", len(v), busy)
	}
	v, _, busy := callIn(t, 40*time.Second).withRoom(st, views, bases)
	if len(v) != 0 || len(busy) != 1 {
		t.Fatalf("candidates %d busy %v, want the node held out", len(v), busy)
	}
	for _, want := range []string{"backlogged-node: backlog (", "200 s", "the call's deadline is"} {
		if !strings.Contains(busy[0], want) {
			t.Errorf("busy line = %q, want it to contain %q", busy[0], want)
		}
	}
}

// TestTheCapacityWaitHoldsANodeOutThatCannotStartBeforeTheCallEnds is the whole path: the one node
// has room but publishes a 200 s start wait, the call has 2 s. The wait re-reads the node every
// tick, never dispatches to it (a job queued there would outlive the call and run on with nobody
// waiting), and ends before the deadline as a capacity defer that prints the call's arithmetic.
// The control arm has no call deadline: the same node takes the job.
func TestTheCapacityWaitHoldsANodeOutThatCannotStartBeforeTheCallEnds(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 300*time.Millisecond)
	eta := 200.0
	tune := func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning, f.queueDepth, f.maxQueueDepth = 2, 2, 2, 8
		f.queueWaitEstimate = &eta
	}
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0
	contract := plainContract()
	contract.TimeoutSec = 300

	control, controlURL := acceptingNode(t, "backlogged-node", "answer", tune)
	_, csum, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{controlURL}, nil, nil)
	if csum.Succeeded != 1 || control.dispatches.Load() != 1 {
		t.Fatalf("control (no call deadline): summary %+v dispatches %d, want the job dealt to the node, whose 200 s start wait is inside the contract's patience", csum, control.dispatches.Load())
	}

	node, url := acceptingNode(t, "backlogged-node", "answer", tune)
	start := time.Now()
	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url}, deadlineIn(2*time.Second), nil)
	if node.dispatches.Load() != 0 {
		t.Fatalf("the node was dispatched %d time(s): a job queued behind its 200 s backlog outlives a 2 s call", node.dispatches.Load())
	}
	pr := results[0]
	if sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity || pr.deadlineCut {
		t.Fatalf("summary %+v result %+v cut %v, want a capacity defer before the deadline", sum, pr.Result, pr.deadlineCut)
	}
	// Only the lower bound is timed: that the call ended before its deadline is the capacity class and
	// !deadlineCut above, and an upper bound 0.3 s from the horizon would fail on a loaded runner.
	if time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("the call took %s, want it to wait out the call to the horizon (1.7 s)", elapsed)
	}
	for _, want := range []string{"backlogged-node", "backlog", "200 s", "the call's deadline is"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
	if healths := node.healths.Load(); healths < 3 {
		t.Fatalf("the node's health was read %d time(s): a held-out node is re-read every tick", healths)
	}
}

// TestTheVerdictLineReadsTheSamePatienceAsTheGate: the narration and the gate are read-only twins
// (placement_reason.go). Fed the gate's own patience they agree; fed the contract's poll budget
// alone they do not, which is what a narration left behind by a clamp would print.
func TestTheVerdictLineReadsTheSamePatienceAsTheGate(t *testing.T) {
	withCallReserve(t, 10*time.Second)
	st := oneStepSchemaContract(300, false)
	views, bases := []NodeView{backloggedNode(200)}, []string{"http://slow"}
	r := callIn(t, 40*time.Second)

	withClamp := placementVerdictLine(st, views, bases, "", map[string]int{}, nil, r.patience)
	if !strings.Contains(withClamp, "backlog (") || !strings.Contains(withClamp, "the call's deadline is") {
		t.Fatalf("verdict line = %q, want the node narrated as the backlog the gate saw, with the call's arithmetic", withClamp)
	}
	own := placementVerdictLine(st, views, bases, "", map[string]int{}, nil, nil)
	if strings.Contains(own, "backlog (") {
		t.Fatalf("verdict line = %q: with the contract's own poll budget (a nil patienceFn) the node is not a backlog", own)
	}
}
