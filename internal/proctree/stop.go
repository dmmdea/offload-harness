package proctree

// The stop primitives a takeover is built from (plan P10, used by P11).
//
// WHAT MAY BE STOPPED. Only a process named by the lease's recorded tree (pid and start identity)
// or by a harness launch marker. Anything else the driver shows on the card is somebody else's:
// it is REPORTED with its pid ("card still busy by pid N (not ours)") and never touched. The
// plan is a pure function of three lists so that rule is testable without killing anything.
//
// HOW. Stop re-checks each member right before it signals it, by pid AND start identity, so a pid
// recycled for another process since it was recorded is left alone; it signals, then polls until
// the process is gone; and it reports one outcome per member, so a caller that frees a claim
// can require that the tree is verified dead (AllGone) and never frees it over a survivor.
// Nothing here runs on a timer or on its own: it is the action of a command an operator ran.

import (
	"strconv"
	"time"
)

// Member identifies one process: its pid and its start identity (the platform's own opaque
// value, compared only for equality; 0 = unknown, held by pid alone).
type Member struct {
	PID     int
	StartMs int64
}

// Plan is what may be stopped and what must be left alone.
type Plan struct {
	// Stop are the processes of the recorded tree and of the launch markers, each once.
	Stop []Member
	// NotOurs are pids the driver shows on the card that are in neither list.
	NotOurs []int
}

// PlanStop decides what a stop may touch. recorded is the lease's recorded tree, markers the
// processes harness launch markers name, onCard the pids the driver lists on the lease's cards.
func PlanStop(recorded, markers []Member, onCard []int) Plan {
	var p Plan
	ours := map[int]bool{}
	for _, list := range [][]Member{recorded, markers} {
		for _, m := range list {
			if m.PID <= 0 || ours[m.PID] {
				continue
			}
			ours[m.PID] = true
			p.Stop = append(p.Stop, m)
		}
	}
	seen := map[int]bool{}
	for _, pid := range onCard {
		if pid <= 0 || ours[pid] || seen[pid] {
			continue
		}
		seen[pid] = true
		p.NotOurs = append(p.NotOurs, pid)
	}
	return p
}

// Report is one line per process on the card that is not ours.
func (p Plan) Report() []string {
	out := make([]string, 0, len(p.NotOurs))
	for _, pid := range p.NotOurs {
		out = append(out, "card still busy by pid "+strconv.Itoa(pid)+" (not ours)")
	}
	return out
}

// The states of one member after Stop.
const (
	// StateStopped: the process was running as recorded and is gone now.
	StateStopped = "stopped"
	// StateGone: the process had already exited.
	StateGone = "already gone"
	// StateRecycled: the recorded process is gone and its pid names another one, left alone.
	StateRecycled = "pid recycled"
	// StateStillRunning: it was signalled and is still running.
	StateStillRunning = "still running"
	// StateFailed: it could not be signalled.
	StateFailed = "could not be stopped"
)

// Outcome is what happened to one member of a plan.
type Outcome struct {
	PID   int
	State string
	Why   string
}

// Deps are the process seams. Alive, Start and Kill are required; the lease package's
// PIDAlive and ProcessStart are the identity the records were written with, and Kill is this
// package's.
type Deps struct {
	Alive func(pid int) bool
	// Start reports a process's start identity; false when it cannot say.
	Start func(pid int) (int64, bool)
	Kill  func(pid int) error
	// Sleep pauses between the checks that a signalled process is gone.
	Sleep func(time.Duration)
	// Polls is how many times a signalled process is re-checked (spaced 500 ms) before it is
	// reported as still running.
	Polls int
}

// Kill ends one process by pid; a process that is already gone is not an error.
func Kill(pid int) error { return killPID(pid) }

// Stop stops every member of the plan and reports one Outcome per member. Members of NotOurs are
// never signalled.
func Stop(p Plan, d Deps) []Outcome {
	out := make([]Outcome, 0, len(p.Stop))
	for _, m := range p.Stop {
		out = append(out, stopOne(m, d))
	}
	return out
}

func stopOne(m Member, d Deps) Outcome {
	o := Outcome{PID: m.PID}
	if !d.Alive(m.PID) {
		o.State = StateGone
		return o
	}
	if m.StartMs != 0 {
		if st, ok := d.Start(m.PID); ok && st != m.StartMs {
			o.State, o.Why = StateRecycled, "the pid began at another time than the recorded process, so it is another process now; not touched"
			return o
		}
	}
	if err := d.Kill(m.PID); err != nil {
		o.State, o.Why = StateFailed, err.Error()
		return o
	}
	polls := d.Polls
	if polls < 1 {
		polls = 1
	}
	for i := 0; i < polls; i++ {
		if !d.Alive(m.PID) {
			o.State = StateStopped
			return o
		}
		if d.Sleep != nil {
			d.Sleep(500 * time.Millisecond)
		}
	}
	if !d.Alive(m.PID) {
		o.State = StateStopped
		return o
	}
	o.State, o.Why = StateStillRunning, "the stop was sent to the process and it is still running"
	return o
}

// AllGone reports whether every member of a plan is verified gone (stopped, already gone, or its
// recorded process gone with the pid reused). A caller that frees a claim requires it.
func AllGone(outs []Outcome) bool {
	for _, o := range outs {
		switch o.State {
		case StateStopped, StateGone, StateRecycled:
		default:
			return false
		}
	}
	return true
}
