package gpuprobe

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Host memory: the one reading and the one rule every GPU lease path shares.
//
// TWO QUANTITIES, NOT ONE. A job takes two different things from the host's memory, and the rule judges each against its
// own line:
//
//   - the working set it TOUCHES: the physical RAM it occupies. The line is the memory that is AVAILABLE now (what can be
//     handed out without paging; standby and cache that can be dropped count). Past it the OS pages.
//   - the commit it is CHARGED: memory the OS has promised it, touched or not. On Windows the line is the commit limit
//     (physical RAM plus the page file): past it OTHER processes' allocations fail while physical RAM may sit free.
//
// They differ by a lot on the hosts this harness runs on. Measured on the reference 3-card Windows box (127.7 GiB RAM,
// 171.7 GiB commit limit), the process tree of one coder-model arm touched at most 27.8 GiB and was charged 58.1 GiB; a
// llama.cpp leg with every layer on the card touched 3.6 GiB and was charged 32.1 GiB (GPU allocations made through the
// display driver model are charged to the process's commit). A rule that compared the commit charge to PHYSICAL RAM
// refused that arm while 75.6 GiB of RAM was free: committed memory is not memory in use. A rule that compared only the
// touched set would let a job that commits twice what it touches push the commit charge past the limit. So an admission
// needs BOTH conditions (HostRAMAdmitsLoad).
//
// WHICH LINE THE COMMIT IS JUDGED AGAINST depends on who said what the commit is. A job that STATES its commit (gpu reserve
// --ram-commit, or a caller that measured one: HostLoad.CommitStated) is judged against the commit limit: the figure is the
// caller's own and the limit is the line the OS enforces. A job that states none declares one figure, and the commit is that
// figure standing in for it: it is judged against the line every such caller was judged against before the two conditions existed,
// the commit charge plus the figure against physical RAM less the headroom. The standing-in figure is the touched set, and a
// job can commit twice what it touches (the coder arm above: 27.8 touched, 58.1 charged; a ComfyUI or Strata arm under the
// display driver model about 2.1x), so against the 44 GiB further out commit limit it would admit a second arm the old line
// refused, into commit exhaustion. The media admission, the seat warm-back and a bare gpu reserve --ram N all declare one figure
// and keep that protection; stating the commit is how a job that measured one is judged on it.
//
// WHERE EACH LINE IS REAL. Available memory is read the same way on every platform (Windows GlobalMemoryStatusEx AvailPhys,
// Linux MemAvailable). The commit limit is a limit only where the OS enforces it: always on Windows, and on Linux only with
// vm.overcommit_memory = 2 (strict). In the default heuristic mode (0) and in mode 1 the kernel grants allocations past
// CommitLimit, Committed_AS counts mappings a process reserved and never touched (CUDA and big mmaps do), and a rule that
// refused a STATED commit on it would turn work away for a limit nothing enforces. HostMemory.CommitLimitSoft says so, and a
// stated commit is then not judged (the touched condition and the physical line of an unstated one still are, as they always
// were on Linux). A reading that cannot say which mode the kernel is in is treated as strict: it fails closed, because the mode is
// the one input that says whether the limit is real, and a limit assumed away on an input nobody read is a brake switched off.
//
// THE LIMIT MOVES. On a Windows host whose page file is system managed the commit limit is the CURRENT one: Windows grows the
// file when the commit charge nears it, and the next reading then carries a larger limit (the incident's own readings show 187.7
// GiB with the file at 60 to 68 GiB, and 171.7 GiB later). The rule reads the current limit, the conservative choice, and does
// not read the configured maximum. So "allocations fail past the limit" is true once the page file has reached its maximum;
// before that, a charge near the limit makes Windows grow the file, which is the growth the incident showed and the rule exists
// to avoid.
//
// The incident that wrote this file (2026-10-09, the reference box): two ComfyUI media lanes streamed bf16 weights that do not
// fit a 16 GiB card, from RAM, at the same time. Free RAM never said "no" at the moment each lease was granted, because
// neither job had loaded yet; committed memory reached 162.9 GiB against a 187.7 GiB limit, the pagefile grew from 60 to 68
// GiB, and free RAM bottomed at 3.2 GiB. The harness has to refuse before the grant, on numbers that include what the granted
// jobs are ABOUT to load, so both conditions add the not-yet-loaded part of the leases already granted (declared less what
// their processes already hold: resident bytes for the touched set, private bytes for the commit).
//
// Paging is never an acceptable state on this harness (the house rule: the cards do the inference, RAM is overflow only,
// bounded, and never makes the box unstable). Nothing here tunes the OS; it reads, and it decides whether a lease may be granted.

// DefaultHostRAMHeadroomGiB is the host RAM the admission rule keeps uncommitted beyond what a
// lease declares (config gpu_host_ram_headroom_gib, 0 = this). It is the room the desktop, the
// agents and the kernel's own growth need while a lease loads. The incident box's non-media baseline
// was 56 GiB of desktop apps, agent CLIs, browsers, WSL and kernel pools, and that baseline moves by
// several GiB inside a minute, so 8 is the floor, not a measurement of any one job.
//
// CHOSEN 2026-10-09 by the harness session, NOT MEASURED, and not a number the operator typed. What it
// stands in for is each node's interactive working-set swing, which has not been measured yet: the
// measurement procedure, the data points so far and the table that takes `measured <date> <node> n=<k>` rows
// are in docs/systems/gpu-lease.md ("How the numbers get measured"). The config key is the override and
// the thing to change when a node's own measurement says so.
const DefaultHostRAMHeadroomGiB = 8.0

// HostMemory is one reading of the host's memory, in GiB. A reading whose commit limit is enforced (CommitLimitSoft false: Windows
// always, Linux in strict mode) must carry that limit: one that leaves CommitLimitGiB at 0 is not a reading, the rule waits on it as
// unreadable and its verdict is unknown, because a counter that came back zero must not turn the commit brake off. A SOFT reading
// carries a limit nobody enforces, and may leave it 0.
type HostMemory struct {
	// PhysicalGiB is the installed RAM the OS can use.
	PhysicalGiB float64
	// AvailableGiB is what can be handed out without paging (standby and cache that can be dropped
	// count): the figure the old free-RAM guards read.
	AvailableGiB float64
	// CommitUsedGiB is memory the OS has committed: promised to processes, resident or not. Windows
	// total minus available page file (GlobalMemoryStatusEx); Linux Committed_AS (a limit only in strict mode).
	CommitUsedGiB float64
	// CommitLimitGiB is what the OS will commit before refusing: physical plus page file on Windows,
	// CommitLimit on Linux.
	CommitLimitGiB float64
	// CommitLimitSoft says CommitLimitGiB is not a limit this OS enforces (Linux with vm.overcommit_memory 0 or 1,
	// or a sandbox that hides the accounting). The zero value is a hard limit: Windows always, Linux in strict mode.
	CommitLimitSoft bool
}

// commitLimitMissing says the reading is of a platform whose commit limit is enforced and carries none: a reading the rule cannot
// trust, failed closed (see HostMemory).
func (m HostMemory) commitLimitMissing() bool { return !m.CommitLimitSoft && m.CommitLimitGiB <= 0 }

// HostLoad is what a job takes of the host's memory, in the two quantities the admission rule keeps apart.
type HostLoad struct {
	// TouchGiB is the working set the job will touch: the physical RAM it occupies.
	TouchGiB float64
	// CommitGiB is the commit it will be charged: memory the OS promises it, touched or not.
	CommitGiB float64
	// CommitStated says CommitGiB is a figure the caller stated or measured (gpu reserve --ram-commit): it is judged against the
	// OS commit limit. Not set, CommitGiB is the touched figure standing in for a commit nobody stated, and it is judged against
	// physical RAM less the headroom, the line a caller that declares one figure has always been held to. It is read on a need;
	// on a pending or held figure it means nothing.
	CommitStated bool
}

// Declares says the job declares any host memory at all: the one test every "does this lease ask the host for something"
// decision goes through, so a job that declares only a commit is not mistaken for one that declares nothing.
func (l HostLoad) Declares() bool { return l.TouchGiB > 0 || l.CommitGiB > 0 }

// HostVerdict is the one word for how the host's memory stands against the two lines the admission rule keeps.
type HostVerdict string

const (
	// HostOK: the available memory, the room under physical RAM for the commit charge and (where the OS enforces one) the room
	// under the commit limit all exceed the headroom, so a lease that declares host RAM can be admitted if it is small enough.
	HostOK HostVerdict = "OK"
	// HostNear: one of them is inside the headroom, so a lease that declares any host RAM waits.
	HostNear HostVerdict = "NEAR"
	// HostOver: one of them is gone: no memory is available, or the commit charge is above physical RAM (the line a commit nobody
	// stated is held to), or it has reached a commit limit the OS enforces (allocations by other processes can fail). Whether the
	// box is paging NOW is not something this reading can say (that takes page-file growth or the pages-out rate), so nothing
	// built on the verdict claims it.
	HostOver HostVerdict = "OVER"
	// HostUnknown: the reading could not be taken.
	HostUnknown HostVerdict = "unknown"
)

// Verdict judges a reading against the configured headroom, by the lines HostRAMAdmitsLoad applies: the available physical
// memory, the commit charge against physical RAM (the line a commit nobody stated is held to, on every reading), and the commit
// limit where the OS enforces one. OVER is the commit charge above physical RAM, or nothing available, or the limit reached; NEAR
// is within the headroom of any of them. They are the guard's own conservative lines for the house rule (RAM is overflow only and
// never makes the box unstable), chosen 2026-10-09 and not measured: a definition of "too little room", not a reading of paging.
// A reading from a platform whose limit is enforced but carries none is unknown (fail closed).
func (m HostMemory) Verdict(headroomGiB float64) HostVerdict {
	if m.PhysicalGiB <= 0 || m.commitLimitMissing() {
		return HostUnknown
	}
	if headroomGiB < 0 {
		headroomGiB = 0
	}
	hard := !m.CommitLimitSoft && m.CommitLimitGiB > 0
	switch {
	case m.AvailableGiB <= 0, m.CommitUsedGiB > m.PhysicalGiB, hard && m.CommitUsedGiB >= m.CommitLimitGiB:
		return HostOver
	case m.AvailableGiB < headroomGiB, m.CommitUsedGiB > m.PhysicalGiB-headroomGiB, hard && m.CommitUsedGiB > m.CommitLimitGiB-headroomGiB:
		return HostNear
	}
	return HostOK
}

// hostMemoryFn is the reader ReadHostMemory calls. A variable so UseHostMemoryReader can stand a
// host in for a test; production never replaces it.
var hostMemoryFn = readHostMemory

// ReadHostMemory takes one reading. ok=false means this platform has no reader or the read failed:
// see HostMemorySupported for which of the two.
func ReadHostMemory() (HostMemory, bool) { return hostMemoryFn() }

// HostMemorySupported says whether this platform has a reader at all (Windows and Linux do). On a
// platform without one the admission rule cannot judge and admits, rather than refusing every lease
// for a number nobody can take; on a platform with one, an unreadable reading refuses.
const HostMemorySupported = hostRAMSupported

// UseHostMemoryReader makes ReadHostMemory call fn until restore is called. It exists for tests of
// the packages that take a lease (a stand-in host with a known commit charge, so a test result does
// not depend on what the machine running it is doing); production code never calls it.
func UseHostMemoryReader(fn func() (HostMemory, bool)) (restore func()) {
	prev := hostMemoryFn
	hostMemoryFn = fn
	return func() { hostMemoryFn = prev }
}

// HostRAMCheck is the answer of the admission rule.
type HostRAMCheck struct {
	// OK: the lease may be granted as far as host RAM goes.
	OK bool
	// Impossible: no state of this box admits the declaration (a touched set above physical RAM less the headroom, or a commit
	// above the commit limit less the headroom), so waiting cannot help; the caller refuses instead of queueing.
	Impossible bool
	// Unreadable: the reading could not be taken on a platform that has a reader.
	Unreadable bool
	// The numbers the touched-set condition rested on, all GiB. NeedGiB is the working set the job declared and PendingGiB the
	// part still to be touched of leases already granted; AvailableGiB is what could be handed out without paging when the
	// decision was taken and AvailableAfterGiB what would be left of it (admitted iff at least HeadroomGiB); LimitGiB is
	// physical RAM less the headroom, the most a touched set can ever be admitted at.
	NeedGiB, PendingGiB, PhysicalGiB, HeadroomGiB, LimitGiB, AvailableGiB, AvailableAfterGiB float64
	// The numbers the commit condition rested on, all GiB. CommittedGiB is the commit charge now, CommitNeedGiB the commit the
	// job declared and CommitPendingGiB the part still to be charged of leases already granted (for a stated commit their
	// declared commit less what their processes are charged; for an unstated one their declared working set less what their
	// processes occupy, the figure the physical line has always counted); CommitLimitGiB is the OS commit limit, CommitBoundGiB
	// the line the commit is judged against less the headroom (the commit limit for a stated commit, physical RAM for an
	// unstated one), and CommitProjectedGiB the charge the grant would reach (admitted iff at most the bound). The last two are
	// set when the commit condition applies.
	CommittedGiB, CommitNeedGiB, CommitPendingGiB, CommitLimitGiB, CommitBoundGiB, CommitProjectedGiB float64
	// CommitApplies says the commit condition was put to this reading: the job declared a commit and either did not state it (the
	// physical line, on every reading) or stated it and the OS enforces a commit limit. CommitStated says the job stated it.
	// TouchShort and CommitShort say which condition refused; both can.
	CommitApplies, CommitStated, TouchShort, CommitShort bool
	// Why is the sentence a refusal carries ("" when OK).
	Why string
}

// HostRAMAdmitsLoad is THE rule, read by the lease grant (authoritative, inside the grant's critical section) and by the card
// allocator (advisory, before it picks cards), so the two cannot disagree. An admission needs BOTH conditions:
//
//	touched:  available now - need.TouchGiB - pending.TouchGiB             >= headroom
//	commit:   commit charge now + need.CommitGiB + pending.CommitGiB       <= commit limit - headroom      (a STATED commit)
//	          commit charge now + need.CommitGiB + pending.TouchGiB        <= physical RAM - headroom      (a commit nobody stated)
//
// the stated form only where the OS enforces a commit limit (HostMemory.CommitLimitSoft is false), the unstated form on every
// reading, and either only when the job declared a commit. need is what the lease declares it will load; pending is the part of the
// leases ALREADY GRANTED that has not loaded yet (declared less what their processes hold), which neither counter yet includes: for
// the touched condition and the unstated commit line the working set their processes occupy is subtracted, for the stated commit
// line the private bytes they are charged. A quantity a job declares as 0 is not judged: it adds nothing to that counter, so
// refusing it would idle a card without making the memory any safer; a job that declares neither is admitted whatever the box
// looks like. A touched set above physical RAM less the headroom, or a commit above the line it is judged against less the
// headroom, is Impossible: it cannot be admitted however long it waits.
//
// WHY THE LINES ARE THESE (see the file header). A STATED commit is compared to the commit limit, not to physical RAM: committed
// memory is not memory in use, and only the limit is what the OS refuses allocations at. A commit nobody stated is the touched
// figure standing in for one, and keeps the line it always had: its stand-in can understate a job that commits more than it touches.
// What a job TOUCHES is compared to the memory that is available, because that is what pages when it runs out. A declaration is
// an estimate (the model files' size, or the largest working set a render's tree reached): this rule is a brake on DECLARED loads,
// not a bound on what the box commits, and neither term sees a process that holds no lease.
func HostRAMAdmitsLoad(mem HostMemory, readable bool, need, pending HostLoad, headroomGiB float64) HostRAMCheck {
	return hostRAMAdmitsLoad(mem, readable, HostMemorySupported, need, pending, headroomGiB)
}

// HostRAMAdmits is HostRAMAdmitsLoad for a caller that declares ONE figure: it is taken for both quantities and states no commit,
// so such a caller is judged on its commit against physical RAM less the headroom, as it was before the two conditions existed.
// The pending figure is likewise taken for both.
func HostRAMAdmits(mem HostMemory, readable bool, needGiB, pendingGiB, headroomGiB float64) HostRAMCheck {
	return hostRAMAdmits(mem, readable, HostMemorySupported, needGiB, pendingGiB, headroomGiB)
}

func hostRAMAdmits(mem HostMemory, readable, supported bool, needGiB, pendingGiB, headroomGiB float64) HostRAMCheck {
	return hostRAMAdmitsLoad(mem, readable, supported, HostLoad{TouchGiB: needGiB, CommitGiB: needGiB}, HostLoad{TouchGiB: pendingGiB, CommitGiB: pendingGiB}, headroomGiB)
}

func hostRAMAdmitsLoad(mem HostMemory, readable, supported bool, need, pending HostLoad, headroomGiB float64) HostRAMCheck {
	need = HostLoad{TouchGiB: max(need.TouchGiB, 0), CommitGiB: max(need.CommitGiB, 0), CommitStated: need.CommitStated}
	pending = HostLoad{TouchGiB: max(pending.TouchGiB, 0), CommitGiB: max(pending.CommitGiB, 0)}
	headroomGiB = max(headroomGiB, 0)
	c := HostRAMCheck{NeedGiB: need.TouchGiB, PendingGiB: pending.TouchGiB, CommitNeedGiB: need.CommitGiB, CommitPendingGiB: pending.CommitGiB, CommitStated: need.CommitStated, HeadroomGiB: headroomGiB}
	if need.TouchGiB == 0 && need.CommitGiB == 0 {
		c.OK = true
		return c
	}
	if !readable || mem.PhysicalGiB <= 0 || mem.commitLimitMissing() {
		if !supported {
			c.OK = true // no reader on this platform: the rule cannot judge, and a refusal here would be permanent
			return c
		}
		c.Unreadable = true
		c.Why = fmt.Sprintf("waiting for host RAM: the host's memory cannot be read, so a lease that needs %.1f GiB cannot be shown to fit", max(need.TouchGiB, need.CommitGiB))
		return c
	}
	c.CommittedGiB, c.PhysicalGiB, c.AvailableGiB = mem.CommitUsedGiB, mem.PhysicalGiB, mem.AvailableGiB
	c.LimitGiB = mem.PhysicalGiB - headroomGiB
	c.AvailableAfterGiB = mem.AvailableGiB - need.TouchGiB - pending.TouchGiB
	c.CommitLimitGiB = mem.CommitLimitGiB
	hardLimit := !mem.CommitLimitSoft && mem.CommitLimitGiB > 0
	// The line the commit is judged against, and the not-yet-charged part that goes with it.
	if need.CommitGiB > 0 {
		switch {
		case need.CommitStated && hardLimit:
			c.CommitApplies = true
			c.CommitBoundGiB = mem.CommitLimitGiB - headroomGiB
			c.CommitProjectedGiB = mem.CommitUsedGiB + need.CommitGiB + pending.CommitGiB
		case !need.CommitStated:
			// The retired comparison, kept for every caller that stated no commit: commit charge + the declared figure + what the
			// running leases have yet to touch, against physical RAM less the headroom. On every reading, hard limit or not.
			c.CommitApplies = true
			c.CommitBoundGiB = c.LimitGiB
			c.CommitPendingGiB = pending.TouchGiB
			c.CommitProjectedGiB = mem.CommitUsedGiB + need.CommitGiB + pending.TouchGiB
		}
	}

	// A declaration no state of the box can admit ends the request at once.
	var never []string
	if need.TouchGiB > c.LimitGiB {
		c.TouchShort = true
		never = append(never, fmt.Sprintf("it needs %.1f GiB of working set, but %.1f GiB of physical RAM less %.1f GiB of headroom leaves %.1f GiB; "+
			"state the real need (gpu reserve --ram <GiB>, 0 = none) or lower gpu_host_ram_headroom_gib", need.TouchGiB, mem.PhysicalGiB, headroomGiB, max(c.LimitGiB, 0)))
	}
	// An unstated commit equal to the working set is the same fact as the sentence above; a stated one is its own.
	if c.CommitApplies && need.CommitGiB > c.CommitBoundGiB && (need.CommitStated || !c.TouchShort) {
		c.CommitShort = true
		never = append(never, fmt.Sprintf("it declares %.1f GiB of commit, but the %.1f GiB %s less %.1f GiB of headroom leaves %.1f GiB; "+
			"state the real commit (gpu reserve --ram-commit <GiB>) or lower gpu_host_ram_headroom_gib", need.CommitGiB, commitLineGiB(mem, need), commitLineName(need), headroomGiB, max(c.CommitBoundGiB, 0)))
	}
	if len(never) > 0 {
		c.Impossible = true
		c.Why = "host RAM can never admit this: " + strings.Join(never, "; and ")
		return c
	}

	// The two conditions, each refusing on its own.
	pend := func(g float64) string {
		if g > 0 {
			return fmt.Sprintf(" (+%.1f GiB still to load by leases already running)", g)
		}
		return ""
	}
	var why []string
	if need.TouchGiB > 0 && c.AvailableAfterGiB < headroomGiB {
		c.TouchShort = true
		why = append(why, fmt.Sprintf("needs %.1f GiB, only %.1f GiB of %.1f GiB physical is available%s, %.1f GiB headroom",
			need.TouchGiB, mem.AvailableGiB, mem.PhysicalGiB, pend(pending.TouchGiB), headroomGiB))
	}
	if c.CommitApplies && c.CommitProjectedGiB > c.CommitBoundGiB {
		c.CommitShort = true
		if need.CommitStated {
			why = append(why, fmt.Sprintf("needs %.1f GiB of commit, committed %.1f of %.1f GiB commit limit%s, %.1f GiB headroom",
				need.CommitGiB, mem.CommitUsedGiB, mem.CommitLimitGiB, pend(pending.CommitGiB), headroomGiB))
		} else {
			hint := ""
			if hardLimit && mem.CommitLimitGiB > mem.PhysicalGiB {
				hint = fmt.Sprintf(" (no commit stated, so it is held to physical RAM; a stated commit is held to the %.1f GiB commit limit)", mem.CommitLimitGiB)
			}
			why = append(why, fmt.Sprintf("needs %.1f GiB, committed %.1f of %.1f GiB physical%s, %.1f GiB headroom%s",
				need.CommitGiB, mem.CommitUsedGiB, mem.PhysicalGiB, pend(pending.TouchGiB), headroomGiB, hint))
		}
	}
	if len(why) == 0 {
		c.OK = true
		return c
	}
	c.Why = "waiting for host RAM: " + strings.Join(why, "; and ")
	return c
}

// commitLineGiB and commitLineName say which line a declared commit is judged against: the OS commit limit for a stated one,
// physical RAM for one nobody stated.
func commitLineGiB(mem HostMemory, need HostLoad) float64 {
	if need.CommitStated {
		return mem.CommitLimitGiB
	}
	return mem.PhysicalGiB
}

func commitLineName(need HostLoad) string {
	if need.CommitStated {
		return "commit limit"
	}
	return "physical RAM"
}

// parseMeminfo reads the counters the Linux reader needs from /proc/meminfo text, all in kB:
// MemTotal and MemAvailable are required (without them there is no reading); Committed_AS and
// CommitLimit are the kernel's commit accounting and are present on every kernel this harness runs on,
// but a sandbox that virtualises /proc/meminfo (a container's lxcfs, gVisor) can omit them. Then the
// reading falls back to what the box visibly uses: commit used = MemTotal - MemAvailable, limit =
// MemTotal. That under-counts memory a process reserved and never touched, but it keeps the rule
// working: the alternative, no reading on a platform that has a reader, makes every lease that
// declares host RAM wait forever. That reading's commit limit is marked soft (the commit condition is not applied to it).
func parseMeminfo(text string) (HostMemory, bool) {
	want := map[string]*float64{}
	var total, avail, committed, limit float64
	want["MemTotal"], want["MemAvailable"], want["Committed_AS"], want["CommitLimit"] = &total, &avail, &committed, &limit
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		dst, ok := want[strings.TrimSuffix(fields[0], ":")]
		if !ok {
			continue
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || kb < 0 {
			return HostMemory{}, false
		}
		*dst = kb / (1 << 20)
		seen[strings.TrimSuffix(fields[0], ":")] = true
	}
	if !seen["MemTotal"] || !seen["MemAvailable"] || total <= 0 {
		return HostMemory{}, false
	}
	soft := false
	if !seen["Committed_AS"] || !seen["CommitLimit"] {
		// What the box visibly uses stands in for a charge the kernel did not report, so the "limit" is no limit anyone enforces.
		committed, limit, soft = total-avail, total, true
	}
	return HostMemory{PhysicalGiB: total, AvailableGiB: avail, CommitUsedGiB: committed, CommitLimitGiB: limit, CommitLimitSoft: soft}, true
}

// parseOvercommit reads the text of /proc/sys/vm/overcommit_memory: the mode, 0 (heuristic), 1 (always) or 2 (strict). Anything
// else is no reading.
func parseOvercommit(text string) (mode int, ok bool) {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || n < 0 || n > 2 {
		return 0, false
	}
	return n, true
}

// readHostMemoryLinux is the Linux reader over a file-reading seam (os.ReadFile in production). The meminfo figures come from
// parseMeminfo; the commit limit is soft only when the kernel is known to be in mode 0 or 1 (vm.overcommit_memory), because there
// it grants allocations past CommitLimit. A mode that cannot be read, or parsed, is assumed to be strict (fail closed).
func readHostMemoryLinux(read func(path string) ([]byte, error)) (HostMemory, bool) {
	b, err := read("/proc/meminfo")
	if err != nil {
		return HostMemory{}, false
	}
	m, ok := parseMeminfo(string(b))
	if !ok {
		return HostMemory{}, false
	}
	if !m.CommitLimitSoft {
		mode, known := 0, false
		if ob, oerr := read("/proc/sys/vm/overcommit_memory"); oerr == nil {
			mode, known = parseOvercommit(string(ob))
		}
		m.CommitLimitSoft = known && mode != 2
	}
	return m, true
}

// hostMemoryFromStatus converts the Windows GlobalMemoryStatusEx counters (bytes) into a reading. On that call ullTotalPageFile
// is the system COMMIT LIMIT (physical plus page file, not the page file alone) and ullAvailPageFile is the commit still
// available, so the commit charge is their difference: the figure Task Manager shows as "Committed". The Windows limit is
// always enforced, so the reading's is hard.
func hostMemoryFromStatus(totalPhys, availPhys, totalPageFile, availPageFile uint64) (HostMemory, bool) {
	if totalPhys == 0 || totalPageFile == 0 {
		return HostMemory{}, false // a total page file of 0 is the commit limit missing, and Windows always enforces one
	}
	const gib = 1 << 30
	used := uint64(0)
	if totalPageFile > availPageFile {
		used = totalPageFile - availPageFile
	}
	return HostMemory{
		PhysicalGiB:    float64(totalPhys) / gib,
		AvailableGiB:   float64(availPhys) / gib,
		CommitUsedGiB:  float64(used) / gib,
		CommitLimitGiB: float64(totalPageFile) / gib,
	}, true
}
