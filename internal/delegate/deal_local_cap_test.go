// deal_local_cap_test.go: the spread deal counts the LOCAL seat's run cap the way it
// counts a remote's headroom (ADR 0063, decision 6), and says what it did.
//
// The remotes were counted and the local seat was not: with every remote dealt to its
// headroom the deal's fallback handed ALL the overflow to an idle local seat - nine of
// twelve subtasks against a run cap of four - into a line (the seat's own FIFO) that
// the subtasks could not leave, which is the pile-up the counting exists to end. The
// overflow goes to the capacity wait instead, which places it on the first node that
// frees, the local seat included.

package delegate

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// TestIdleLocalSeatIsNotDealtBeyondItsRunCap: 12 subtasks, remotes with room for 3,
// an idle local seat whose run cap is 4. The remotes take their 3 and the local seat
// its 4; the other 5 wait in line for whichever frees first. The deal used to hand the
// local seat 9.
func TestIdleLocalSeatIsNotDealtBeyondItsRunCap(t *testing.T) {
	r := fitRunner(headroomRemote(fitBigRemote, "big-remote", 2, 0), headroomRemote(fitMidRemote, "mid-remote", 1, 0))
	r.cfg.FleetMaxConcurrentJobs = 4
	_, slots := deal(r, repeatGoal(fitMechGoal, 12)...)
	dealt, waiting := dealCounts(slots)
	if local := dealt[fitLocal().NodeID]; local != 4 {
		t.Fatalf("local dealt %d, want its run cap of 4 (all dealt %v, capacity wait %d)", local, dealt, waiting)
	}
	if dealt["big-remote"] != 2 || dealt["mid-remote"] != 1 || waiting != 5 {
		t.Fatalf("dealt %v, capacity wait %d, want big 2, mid 1 (their headroom) and 5 subtasks waiting in line", dealt, waiting)
	}
	for i, sl := range slots {
		if !sl.capacityWait {
			continue
		}
		for _, want := range []string{"already dealt to its headroom", "big-remote: cap (0/2 running, headroom 2, dealt 2)", "local seat's run cap"} {
			if !strings.Contains(sl.reason, want) {
				t.Errorf("subtask %d reason = %q, want it to contain %q", i, sl.reason, want)
			}
		}
		if strings.Contains(sl.reason, "no eligible remote") {
			t.Errorf("subtask %d reason = %q says no remote was eligible: two were, and both were full", i, sl.reason)
		}
	}
}

// TestSpreadDealCapsTheLocalRotationShareAtTheRunCap: the same count binds the local
// seat's ordinary rotation slots. One remote with room for everything, an idle seat
// with a run cap of 2, 8 subtasks: the seat takes 2 and the remote the other 6, where
// the rotation used to hand the seat 4 and let 2 of them queue behind the cap.
func TestSpreadDealCapsTheLocalRotationShareAtTheRunCap(t *testing.T) {
	r := fitRunner(headroomRemote(fitBigRemote, "big-remote", 64, 0))
	r.cfg.FleetMaxConcurrentJobs = 2
	_, slots := deal(r, repeatGoal(fitMechGoal, 8)...)
	dealt, waiting := dealCounts(slots)
	if dealt[fitLocal().NodeID] != 2 || dealt["big-remote"] != 6 || waiting != 0 {
		t.Fatalf("dealt %v, capacity wait %d, want the local seat held to its run cap of 2 and the remote given the other 6", dealt, waiting)
	}
}

// TestSpreadDealIsUnchangedWhileTheLocalSeatHasRoom: the control arm. With room to
// spare the deal is exactly the rotation it was (local, A, B, local, ...).
func TestSpreadDealIsUnchangedWhileTheLocalSeatHasRoom(t *testing.T) {
	a := NodeView{NodeID: "node-a", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	b := NodeView{NodeID: "node-b", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	r := fitRunner(a, b)
	r.cfg.FleetMaxConcurrentJobs = 4
	where, _ := deal(r, repeatGoal(fitMechGoal, 8)...)
	want := []string{"local-box", "node-a", "node-b", "local-box", "node-a", "node-b", "local-box", "node-a"}
	if !equalStrings(where, want) {
		t.Fatalf("deal = %v, want %v", where, want)
	}
}

// TestSpreadDealDoesNotCapTheLocalSeatWhenNoRemoteCouldTakeTheContract: the cap
// exists to hand the overflow to a node that will free. With no remote able to run the
// contract at all there is nothing to wait for, and the seat's own line IS the queue,
// exactly as it was: queued-local beats ineligible-remote.
func TestSpreadDealDoesNotCapTheLocalSeatWhenNoRemoteCouldTakeTheContract(t *testing.T) {
	off := fitBigRemote
	off.AgentEnabled = false
	r := fitRunner(off)
	r.cfg.FleetMaxConcurrentJobs = 2
	_, slots := deal(r, repeatGoal(fitMechGoal, 8)...)
	dealt, waiting := dealCounts(slots)
	if dealt[fitLocal().NodeID] != 8 || waiting != 0 {
		t.Fatalf("dealt %v, capacity wait %d, want all 8 on the local seat: no remote could take any of them", dealt, waiting)
	}
	for i, sl := range slots {
		if !strings.Contains(sl.reason, "no eligible remote") {
			t.Errorf("subtask %d reason = %q, want it to keep saying no remote was eligible", i, sl.reason)
		}
	}
}

// TestSpreadDealCountsWhatIsAlreadyRegisteredOnTheLocalSeat: the seat's room is what
// its run cap leaves after the runs already registered on it (another Run's, a fleet
// job's), not the whole cap.
func TestSpreadDealCountsWhatIsAlreadyRegisteredOnTheLocalSeat(t *testing.T) {
	r := fitRunner(headroomRemote(fitBigRemote, "big-remote", 64, 0), headroomRemote(fitMidRemote, "mid-remote", 64, 0))
	r.cfg = testCfg(t)
	r.cfg.FleetMaxConcurrentJobs = 4
	for i := 0; i < 3; i++ {
		run := gpuactivity.Start(r.cfg.GPULockPath, r.cfg.StateDir, gpuactivity.Run{Seat: r.cfg.AgentPlannerModel(""), Kind: "contract", Goal: "another run's", Phase: gpuactivity.PhaseRunning})
		if run == nil {
			t.Fatal("fixture: could not register a run on the local seat")
		}
		t.Cleanup(run.End)
	}
	_, slots := deal(r, repeatGoal(fitMechGoal, 6)...)
	dealt, waiting := dealCounts(slots)
	if dealt[fitLocal().NodeID] != 1 || waiting != 0 {
		t.Fatalf("dealt %v, capacity wait %d, want the local seat given 1 (three of its four run-cap slots are taken) and the rest dealt to the remotes", dealt, waiting)
	}
}

// TestSpreadDealFallbackReasonSaysNoRemoteHasRoom: a subtask dealt to the local seat
// because every remote is at its headroom says so - "no eligible remote" would send
// an operator to add a node when both nodes it has are merely full.
func TestSpreadDealFallbackReasonSaysNoRemoteHasRoom(t *testing.T) {
	r := fitRunner(headroomRemote(fitBigRemote, "big-remote", 1, 0))
	r.cfg.FleetMaxConcurrentJobs = 4
	_, slots := deal(r, repeatGoal(fitMechGoal, 3)...)
	third := slots[2]
	if !third.view.Local || third.capacityWait {
		t.Fatalf("subtask 2 = %+v, want the local seat (it has room) - the remote's one slot went to subtask 1", third.placement)
	}
	if !strings.Contains(third.reason, "no remote with room") || strings.Contains(third.reason, "no eligible remote") {
		t.Fatalf("reason = %q, want it to say no remote had room, not that none was eligible", third.reason)
	}
}

// TestSpreadDealNamesAProbeFailureBesideACappedRemote: noEligibleRemote's rule is
// DEFAULT TO LOUD - a remote that failed its health probe is a broken node, not a busy
// one, and a busy one beside it must not paper over it. The capacity branch used to
// replace the diagnosis wholesale, so the failed node vanished from the reason and the
// run read healthy (no remotes_unreachable, exit 0) while half the fleet was down.
func TestSpreadDealNamesAProbeFailureBesideACappedRemote(t *testing.T) {
	full := headroomRemote(fitBigRemote, "big-remote", 1, 1) // headroom 0
	r := fitRunner(full)
	r.spreadProbeErrs = []string{"http://192.0.2.9:18811: dial tcp 192.0.2.9:18811: connect: connection refused"}
	r.spreadLease.Held, r.spreadLease.Class = true, "text" // the seat is reserved: nothing else can take it
	_, slots := deal(r, fitMechGoal)
	sl := slots[0]
	if !sl.deadFleet {
		t.Fatalf("slot = %+v, want the fleet flagged (a remote failed its health probe)", sl.placement)
	}
	for _, want := range []string{"connection refused", "failed the health probe", "big-remote: cap"} {
		if !strings.Contains(sl.reason, want) {
			t.Errorf("reason = %q, want it to name %q: both the busy node and the broken one", sl.reason, want)
		}
	}

	// The control arm: the same busy remote with every probe answering is a plain
	// capacity outcome, not a broken fleet.
	r2 := fitRunner(full)
	r2.spreadLease.Held, r2.spreadLease.Class = true, "text"
	_, clean := deal(r2, fitMechGoal)
	if clean[0].deadFleet || strings.Contains(clean[0].reason, "health probe") {
		t.Fatalf("slot = %+v, want no fleet flag when every remote answered its probe", clean[0].placement)
	}
}

// TestSpreadDealNamesTheBacklogItHeldBackInEveryDealtSlot: a node held out by the
// backlog gate is named, with its arithmetic, on every slot dealt beside it - not only
// when nothing else was left - so an operator can see why it was dealt nothing.
func TestSpreadDealNamesTheBacklogItHeldBackInEveryDealtSlot(t *testing.T) {
	slow := slowNodeWithHeadroom()
	slow.AgentCtxTokens = 131072
	fast := fitMidRemote
	fast.NodeID = "fast-node"
	r := fitRunner(slow, fast)
	r.spreadLease.Held, r.spreadLease.Class = true, "text" // the seat is reserved: the remotes take everything
	c := fitSubtask(fitMechGoal, 100).Contract
	c.TimeoutSec = 300
	for i, sl := range r.dealSpread([]core.AgentContract{c, c, c}, fitLocal()) {
		if sl.view.NodeID != "fast-node" {
			t.Fatalf("subtask %d was dealt to %q (%s), want fast-node", i, sl.view.NodeID, sl.reason)
		}
		for _, want := range []string{"slow-node: backlog (", "444 s"} {
			if !strings.Contains(sl.reason, want) {
				t.Errorf("subtask %d reason = %q, want it to name the held-back node with its arithmetic (%q)", i, sl.reason, want)
			}
		}
	}
}

// slowNodeWithHeadroom is slowNodeShaped with three free execution slots and a start
// ETA of 444 s: only the backlog gate - never the headroom count - can keep it out.
func slowNodeWithHeadroom() NodeView {
	v := slowNodeShaped()
	v.NodeID = "slow-node"
	v.MaxConcurrentJobs, v.JobsRunning, v.JobsQueued, v.QueueDepth, v.MaxQueueDepth = 4, 1, 0, 1, 8
	est := 444.0
	v.QueueWaitEstimateSec = &est
	return v
}

// The shipped deal tests hold the slow node out with slowNodeShaped: one worker, one
// running, headroom 0 - so the headroom count drops it whether or not the backlog gate
// exists. These use a node with headroom, so only the gate can.

func TestAutoDealBacklogGateHoldsBackANodeThatHasHeadroom(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	r := &runner{route: "remote"}
	only := r.placeAutoRemote("seed", st, localNode(), []NodeView{slowNodeWithHeadroom()}, []string{"http://slow"}, true, map[string]int{}, nil)
	if !only.capacityWait {
		t.Fatalf("slot = %+v, want the capacity wait: the only node has headroom but cannot start the job inside the caller's patience", only.placement)
	}
}

func TestSpreadDealBacklogGateHoldsBackANodeThatHasHeadroom(t *testing.T) {
	slow := slowNodeWithHeadroom()
	slow.AgentCtxTokens = 131072
	r := fitRunner(slow)
	r.spreadLease.Held, r.spreadLease.Class = true, "text" // a text lease takes the local seat out of the rotation
	c := fitSubtask(fitMechGoal, 100).Contract
	c.TimeoutSec = 300
	for i, sl := range r.dealSpread([]core.AgentContract{c, c}, fitLocal()) {
		if sl.view.NodeID == "slow-node" {
			t.Fatalf("subtask %d was dealt to a node that cannot start it inside the caller's patience (%s)", i, sl.reason)
		}
	}
}
