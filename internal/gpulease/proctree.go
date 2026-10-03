package gpulease

// The wrapper's process tree (plan P4, evidence rule (a) and (c) of infer.go).
//
// A legacy lease records the pid of the wrapper that holds it and nothing about what it
// runs. The tree below that pid is where ComfyUI lives, started by a script started by the
// wrapper, so its command lines are the only place a `--cuda-device` can be read. Reading is
// all this does: nothing here signals, stops or kills a process (plan invariant I5).

// ProcessTree lists root and every descendant of it, each with its start time and, where the
// operating system lets this process read it, its command line. A process whose command line
// cannot be read (another security context) is listed with an empty one: it is reported, not
// skipped. The error is non-nil only when the process table itself cannot be read.
func ProcessTree(root int) ([]TreeProc, error) { return processTreeImpl(root) }

// descendantsOf selects root and its descendants from a flat process table by parent links.
// It is pure so the rule is testable without an operating system.
//
// A parent link is only believed when the child began no earlier than its parent: process ids
// are recycled, and a child whose recorded parent id now names a younger, unrelated process is
// not that process's child. start reads a process's start time (0, false when unknown, which
// accepts the link: an unreadable start must not hide a real descendant). Each process is
// visited once, so a cycle in a corrupt table cannot loop.
func descendantsOf(root int, table []TreeProc, start func(pid int) (int64, bool)) []TreeProc {
	byParent := map[int][]TreeProc{}
	var rootProc *TreeProc
	for i := range table {
		p := table[i]
		if p.PID == root {
			rootProc = &table[i]
		}
		byParent[p.PPID] = append(byParent[p.PPID], p)
	}
	var out []TreeProc
	seen := map[int]bool{root: true}
	if rootProc != nil {
		out = append(out, *rootProc)
	} else {
		out = append(out, TreeProc{PID: root})
	}
	queue := []int{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		parentStart, parentKnown := start(parent)
		for _, c := range byParent[parent] {
			if seen[c.PID] {
				continue
			}
			if childStart, ok := start(c.PID); ok && parentKnown && childStart < parentStart {
				continue // a recycled parent id: this process is older than the parent it names
			}
			seen[c.PID] = true
			out = append(out, c)
			queue = append(queue, c.PID)
		}
	}
	return out
}

// ticksToUnixMs converts a Linux process start (clock ticks since boot at USER_HZ, 100 on every
// Linux this harness runs on) to Unix milliseconds, given the boot time in Unix seconds.
func ticksToUnixMs(bootSec, ticks int64) int64 {
	const userHZ = 100
	return bootSec*1000 + ticks*1000/userHZ
}
