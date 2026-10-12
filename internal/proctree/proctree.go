// Package proctree contains and stops the process tree a command starts (plan P10).
//
// WHY. A lease wrapper used to start its command with a bare exec and, when the lease was lost,
// kill the direct child only: a grandchild (a launcher's render server, a script's worker) kept
// running on a card another job was about to be handed. Nothing could enumerate what a wrapper
// had started either, so a wrapper that died took with it the only record of what held the card.
//
// WHAT. Start runs a command inside a containment the OS can enumerate and stop as a whole:
//
//   - Windows: a job object. The child is created suspended, assigned to the job and resumed, so
//     it cannot spawn a process that escapes before it is inside; every descendant joins the job,
//     the job lists them (PIDs) and TerminateJobObject stops them all. The job object code
//     (Job) is the one the agent shell's cage (internal/sandbox) builds on.
//   - Unix: the child leads its own session and process group, and the group is signalled as a
//     whole (kill(-pgid)). A process that leaves the group is still found through its parent
//     chain while that chain is alive. PR_SET_PDEATHSIG is not used: it is delivered per thread
//     and covers the direct child only.
//
// Letting go of the containment (Close, or the wrapper dying) leaves the tree running unless the
// caller opted in with DieWithWrapper: a wrapper that is a single point of failure for a
// twenty-hour render is worse than the orphan the lease then keeps as a tree-orphan.
//
// This package never decides what to stop. Terminate stops everything the containment holds;
// PlanStop and Stop (stop.go) are the only path that stops a process the containment does not
// hold, and they stop nothing that is not named by a recorded tree or a launch marker.
package proctree

import (
	"errors"
	"os/exec"
)

// Options configures Start.
type Options struct {
	// DieWithWrapper ends the tree when the wrapper's last handle on the containment closes
	// (Windows: KILL_ON_JOB_CLOSE; the wrapper exiting or dying closes it). Off by default.
	// Platforms that cannot honour it refuse it with ErrDieWithWrapperUnsupported.
	DieWithWrapper bool
}

// ErrDieWithWrapperUnsupported is returned by Start where the platform cannot end a tree when
// its parent dies: a refusal, never a silent no-op, because the caller asked for a guarantee.
var ErrDieWithWrapperUnsupported = errors.New("proctree: die-with-wrapper is not supported on this platform (there is no handle whose closing ends a whole process tree); the tree is left to the lease's tree-orphan rule")

// SupportsDieWithWrapper reports whether this platform can end a tree when its parent dies, so a
// caller can refuse the option before it does anything else (a wrapper that queued for hours for a
// card and then found it could not give the guarantee it was asked for has wasted the wait).
func SupportsDieWithWrapper() bool { return supportsDieWithWrapper }

// ErrNotContained is returned by PIDs and Terminate when the command runs without a
// containment (its Warning says why). The caller falls back to what it can enumerate itself.
var ErrNotContained = errors.New("proctree: the command is not contained")

// Start starts cmd inside a containment and returns its handle. It fails when the command
// cannot be started, or when the options cannot be honoured. When the OS refuses the
// containment itself (a job that cannot be nested, a handle that cannot be opened) the command
// still runs, uncontained, and Warning says why: a wrapper must not fail the job it wraps over
// a missing safety net, and it must not hide that the net is missing.
func Start(cmd *exec.Cmd, opts Options) (*Tree, error) { return start(cmd, opts) }

// Tree is a started command and the containment around it.
type Tree struct {
	cmd *exec.Cmd
	p   platform
	// warning is why the command runs uncontained ("" when it is contained).
	warning string
}

// Contained reports whether the command runs inside a containment the OS can enumerate and stop.
func (t *Tree) Contained() bool { return t.p.contained() }

// Warning says why the command runs uncontained ("" when it is contained).
func (t *Tree) Warning() string { return t.warning }

// Mechanism names what contains the tree: "job object", "process group" or "none".
func (t *Tree) Mechanism() string { return t.p.mechanism() }

// PID is the command's own pid.
func (t *Tree) PID() int { return t.cmd.Process.Pid }

// PIDs lists the live members of the tree the containment knows of: the command and every
// descendant (Windows: the job's processes; Unix: the group's processes and the descendants
// reachable through live parent links). Order is not meaningful.
func (t *Tree) PIDs() ([]int, error) { return t.p.pids(t) }

// Terminate stops every member of the tree at once. Members that already exited are not an
// error. It is the wrapper stopping its OWN tree (the lease was lost, the operator interrupted
// it); stopping anything the containment does not hold is PlanStop's.
func (t *Tree) Terminate() error { return t.p.terminate(t) }

// TerminateExcept stops every member of the tree that is not in protect and returns the pids it
// left running. A process another lease took over (a kept ComfyUI instance it re-stamped to
// itself) must survive the wrapper that lost its lease; protect names those, descendants
// included. With nothing to protect it is Terminate.
func (t *Tree) TerminateExcept(protect map[int]bool) ([]int, error) {
	if len(protect) == 0 {
		return nil, t.Terminate()
	}
	return t.p.terminateExcept(t, protect)
}

// Close releases the wrapper's handle on the containment. The tree keeps running unless the
// tree was started with DieWithWrapper.
func (t *Tree) Close() error { return t.p.close() }

// membersByTable selects, from a flat process table, the members of a tree: every process whose
// group is pgid, plus every descendant of root through parent links. Pure, so the rule is
// testable without an operating system. ppid maps a pid to its parent, group maps a pid to its
// process group.
func membersByTable(pgid, root int, ppid, group map[int]int) []int {
	seen := map[int]bool{}
	var out []int
	add := func(pid int) {
		if !seen[pid] {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	for pid, g := range group {
		if pgid > 0 && g == pgid {
			add(pid)
		}
	}
	children := map[int][]int{}
	for pid, parent := range ppid {
		children[parent] = append(children[parent], pid)
	}
	if _, ok := ppid[root]; ok {
		add(root)
	}
	queue := []int{root}
	visited := map[int]bool{root: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if visited[c] {
				continue
			}
			visited[c] = true
			add(c)
			queue = append(queue, c)
		}
	}
	return out
}
