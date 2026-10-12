package proctree

import (
	"strings"
	"testing"
	"time"
)

// fakeProcs is a process table the stop path is tested against: who is alive, when each began,
// and what was killed. A kill that is configured to fail leaves the process running.
type fakeProcs struct {
	up      map[int]bool
	began   map[int]int64
	killed  []int
	failFor map[int]bool
}

func (f *fakeProcs) deps() Deps {
	return Deps{
		Alive: func(pid int) bool { return f.up[pid] },
		Start: func(pid int) (int64, bool) { v, ok := f.began[pid]; return v, ok },
		Kill: func(pid int) error {
			f.killed = append(f.killed, pid)
			if !f.failFor[pid] {
				f.up[pid] = false
			}
			return nil
		},
		Sleep: func(time.Duration) {},
		Polls: 3,
	}
}

// Only a process named by the lease's recorded tree or by a harness launch marker may be
// stopped. Whatever else is on the card is reported with its pid and never touched.
func TestForeignProcessOnCardIsReportedNotKilled(t *testing.T) {
	f := &fakeProcs{up: map[int]bool{9001: true, 9100: true, 777: true}, began: map[int]int64{9001: 5, 9100: 6, 777: 7}}
	plan := PlanStop(
		[]Member{{PID: 9001, StartMs: 5}},
		[]Member{{PID: 9100, StartMs: 6}},
		[]int{9001, 9100, 777}, // what the driver says is on the card
	)
	if len(plan.Stop) != 2 {
		t.Fatalf("the plan stops the recorded member and the marker's process, got %+v", plan.Stop)
	}
	if len(plan.NotOurs) != 1 || plan.NotOurs[0] != 777 {
		t.Fatalf("pid 777 is on the card and in neither list: %+v", plan.NotOurs)
	}
	if got := plan.Report(); len(got) != 1 || got[0] != "card still busy by pid 777 (not ours)" {
		t.Fatalf("report = %q", got)
	}
	outcomes := Stop(plan, f.deps())
	for _, k := range f.killed {
		if k == 777 {
			t.Fatal("a process that is not in the recorded tree or a marker was killed")
		}
	}
	if !f.up[777] {
		t.Fatal("the foreign process must still be running")
	}
	if !AllGone(outcomes) {
		t.Fatalf("both of ours are gone: %+v", outcomes)
	}
}

// A pid that was recycled for another process since it was recorded is not the recorded process.
func TestStopSkipsAMemberWhosePidWasRecycled(t *testing.T) {
	f := &fakeProcs{up: map[int]bool{9001: true}, began: map[int]int64{9001: 999}} // began later than recorded
	out := Stop(PlanStop([]Member{{PID: 9001, StartMs: 5}}, nil, nil), f.deps())
	if len(f.killed) != 0 {
		t.Fatalf("killed %v: the pid is another process now", f.killed)
	}
	if len(out) != 1 || out[0].State != StateRecycled || !AllGone(out) {
		t.Fatalf("the recorded process is gone (its pid was reused): %+v", out)
	}
}

// A member that is already gone is a success, and one that will not die is reported, not hidden:
// a takeover that frees the claim over a process still running would double-book the card.
func TestStopVerifiesEachMemberIsDead(t *testing.T) {
	f := &fakeProcs{up: map[int]bool{9001: true, 9002: false, 9003: true}, began: map[int]int64{9001: 5, 9003: 5}, failFor: map[int]bool{9003: true}}
	out := Stop(PlanStop([]Member{{PID: 9001, StartMs: 5}, {PID: 9002, StartMs: 5}, {PID: 9003, StartMs: 5}}, nil, nil), f.deps())
	states := map[int]string{}
	for _, o := range out {
		states[o.PID] = o.State
	}
	if states[9001] != StateStopped || states[9002] != StateGone || states[9003] != StateStillRunning {
		t.Fatalf("states = %v", states)
	}
	if AllGone(out) {
		t.Fatal("a member that will not die means the tree is not verified dead")
	}
	var why string
	for _, o := range out {
		if o.PID == 9003 {
			why = o.Why
		}
	}
	if !strings.Contains(why, "still running") {
		t.Fatalf("the survivor says so: %q", why)
	}
}

// A member recorded with no start identity is held by its pid alone, so it is stopped by pid.
func TestStopTreatsAnUnknownStartAsTheRecordedProcess(t *testing.T) {
	f := &fakeProcs{up: map[int]bool{9001: true}, began: map[int]int64{9001: 12345}}
	out := Stop(PlanStop([]Member{{PID: 9001}}, nil, nil), f.deps())
	if len(f.killed) != 1 || !AllGone(out) {
		t.Fatalf("killed=%v out=%+v", f.killed, out)
	}
}

// A process in both lists is stopped once.
func TestPlanStopDeduplicates(t *testing.T) {
	plan := PlanStop([]Member{{PID: 9001, StartMs: 5}}, []Member{{PID: 9001, StartMs: 5}, {PID: 9100}}, []int{9001})
	if len(plan.Stop) != 2 || len(plan.NotOurs) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

// The member selection is a pure function of a process table: the group's processes and every
// descendant of the command through live parent links, nothing else.
func TestMembersByTableSelectsTheGroupAndTheDescendants(t *testing.T) {
	ppid := map[int]int{
		100: 1,   // the command, leader of group 100
		101: 100, // its child, same group
		102: 101, // a grandchild that left the group (setsid) but whose parent chain is live
		103: 1,   // a reparented member of the group (its parent died)
		200: 1,   // unrelated
		201: 200,
	}
	group := map[int]int{100: 100, 101: 100, 102: 102, 103: 100, 200: 200, 201: 200}
	got := map[int]bool{}
	for _, p := range membersByTable(100, 100, ppid, group) {
		got[p] = true
	}
	for _, want := range []int{100, 101, 102, 103} {
		if !got[want] {
			t.Errorf("member %d missing: %v", want, got)
		}
	}
	for _, not := range []int{1, 200, 201} {
		if got[not] {
			t.Errorf("process %d is not in the tree: %v", not, got)
		}
	}
	// A corrupt table with a cycle terminates.
	cyc := map[int]int{100: 102, 101: 100, 102: 101}
	done := make(chan struct{})
	go func() { membersByTable(100, 100, cyc, map[int]int{}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cycle in the table looped forever")
	}
}
