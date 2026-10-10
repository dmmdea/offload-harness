package gpuprobe

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Host memory: the one reading and the one rule every GPU lease path shares.
//
// WHY COMMITTED MEMORY AND NOT FREE RAM. The incident that wrote this file (2026-10-09, the
// reference 3-card Windows box, 127.7 GiB physical): two ComfyUI media lanes streamed bf16 weights
// that do not fit a 16 GiB card, from RAM, at the same time. Free RAM never said "no" at the moment
// each lease was granted, because neither job had loaded yet; committed memory reached 162.9 GiB
// against a 187.7 GiB limit, the pagefile grew from 60 to 68 GiB, and free RAM bottomed at 3.2 GiB.
// A card has a hard edge (the driver refuses the allocation); host RAM does not (the OS pages), so
// the harness has to refuse before the grant, on a number that includes what the granted jobs are
// ABOUT to load. Commit charge is the nearest reading on Windows: memory the OS has promised, touched or not
// (what a job has yet to request is added from its declaration, the not-yet-loaded part).
// On Linux Committed_AS is the same figure by the kernel's accounting, and counts mappings a process
// reserved and never touched (CUDA and big mmaps do), so it reads high there: the safe direction for
// a guard whose failure is paging.
//
// Paging is never an acceptable state on this harness (the house rule: the cards do the inference,
// RAM is overflow only, bounded, and never makes the box unstable). Nothing here tunes the OS; it
// reads, and it decides whether a lease may be granted.

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

// HostMemory is one reading of the host's memory, in GiB.
type HostMemory struct {
	// PhysicalGiB is the installed RAM the OS can use.
	PhysicalGiB float64
	// AvailableGiB is what can be handed out without paging (standby and cache that can be dropped
	// count): the figure the old free-RAM guards read.
	AvailableGiB float64
	// CommitUsedGiB is memory the OS has committed: promised to processes, resident or not. Windows
	// total minus available page file (GlobalMemoryStatusEx); Linux Committed_AS.
	CommitUsedGiB float64
	// CommitLimitGiB is what the OS will commit before refusing: physical plus page file on Windows,
	// CommitLimit on Linux.
	CommitLimitGiB float64
}

// HostVerdict is the one word for how the host's memory stands against its physical RAM.
type HostVerdict string

const (
	// HostOK: commit sits more than the headroom below physical RAM.
	HostOK HostVerdict = "OK"
	// HostNear: commit is within the headroom of physical RAM, so the next job to load pushes it over.
	HostNear HostVerdict = "NEAR"
	// HostOver: committed memory exceeds physical RAM. The OS can keep the difference in the page file,
	// so memory touched later may page; whether the box is paging NOW is not something this reading can
	// say (that takes page-file growth or the pages-out rate, and resident memory can sit well below
	// commit), so nothing built on the verdict claims it.
	HostOver HostVerdict = "OVER"
	// HostUnknown: the reading could not be taken.
	HostUnknown HostVerdict = "unknown"
)

// Verdict judges a reading against the configured headroom. OVER is commit used above physical RAM and
// NEAR is within the headroom of it. Both are the guard's own conservative lines for the house rule
// (RAM is overflow only and never makes the box unstable), chosen 2026-10-09 and not measured: a
// definition of "too much promised", not a reading of paging.
func (m HostMemory) Verdict(headroomGiB float64) HostVerdict {
	if m.PhysicalGiB <= 0 {
		return HostUnknown
	}
	if headroomGiB < 0 {
		headroomGiB = 0
	}
	switch {
	case m.CommitUsedGiB > m.PhysicalGiB:
		return HostOver
	case m.CommitUsedGiB > m.PhysicalGiB-headroomGiB:
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
	// Impossible: no state of this box admits the need (it exceeds physical RAM less the headroom),
	// so waiting cannot help; the caller refuses instead of queueing.
	Impossible bool
	// Unreadable: the reading could not be taken on a platform that has a reader.
	Unreadable bool
	// The numbers the decision rested on, all GiB. ProjectedGiB is commit now plus the need plus the
	// part still to load of leases already granted; LimitGiB is physical RAM less the headroom;
	// AvailableGiB is what could be handed out without paging when the decision was taken.
	NeedGiB, CommittedGiB, PendingGiB, PhysicalGiB, HeadroomGiB, LimitGiB, ProjectedGiB, AvailableGiB float64
	// Why is the sentence a refusal carries ("" when OK).
	Why string
}

// HostRAMAdmits is THE rule, read by the lease grant (authoritative, inside the grant's critical
// section) and by the card allocator (advisory, before it picks cards), so the two cannot disagree.
// An admission needs BOTH terms:
//
//	commit:    commit used now + need + pending      <= physical RAM - headroom
//	physical:  available now - need - pending        >= headroom
//
// need is what the lease declares it will load; pending is the part of the leases ALREADY GRANTED
// that has not loaded yet (declared need less what their processes hold), which neither counter yet
// includes. A lease that declares no need (<= 0) adds nothing, so it is admitted whatever the box
// looks like: refusing it would idle a card without making the memory any safer. A need that exceeds
// physical RAM less the headroom is Impossible: it cannot be admitted however long it waits.
//
// WHY TWO TERMS (G3 of the P0 plan). Commit is the primary term because it counts memory the OS has
// PROMISED, touched or not, which is what stops a second lane before it has loaded. On Windows, GPU
// allocations made through WDDM may be charged to the process's commit too (a llama-server with every layer
// on the card was reported at 29.2 GiB private bytes beside 25.7 GiB of VRAM; the evidence and what is still
// unverified are in docs/systems/gpu-lease.md). That cuts two ways, and only one of them is safe. A box whose
// commit already carries the VRAM in use reads high, so a rule that read only commit would over-refuse by
// that much. But a DECLARATION is the model files' size (hostneed) or, once three runs exist, the largest
// RESIDENT set a render's tree reached, so a lane whose commit carries card memory on top of that commits MORE
// than it declared, and the commit term only learns it after the lane has started. The physical term reads
// what can be handed out without paging, so it catches resident memory that commit does not show; it does not
// catch a lane that commits more than it declared, and neither term sees a process that holds no lease.
//
// So this rule is a brake on DECLARED loads, not a bound on what the box commits, and an estimate is all a
// declaration is until three measured runs raise it. Measured counterexample (docs/systems/gpu-lease.md,
// "Known limits"): at a commit reading of 81.6 GiB on the 127.7 GiB reference box with 83 GiB available, a
// 32.8 GiB declaration (the Krea 2 bf16 files) is admitted here (projected 114.4 GiB against a limit of
// 119.7, 50.2 GiB left available), while a ComfyUI lane holding 57.7 GiB private bytes (its family is not
// recorded) read 129.3 GiB committed on the same box some hours later (a separate reading, so the 47.7 GiB
// between the two is the lane and whatever else changed), above its physical RAM. Those readings were measured on
// the reference box on 2026-10-10 from session readings that are not recorded in this repository.
func HostRAMAdmits(mem HostMemory, readable bool, needGiB, pendingGiB, headroomGiB float64) HostRAMCheck {
	return hostRAMAdmits(mem, readable, HostMemorySupported, needGiB, pendingGiB, headroomGiB)
}

func hostRAMAdmits(mem HostMemory, readable, supported bool, needGiB, pendingGiB, headroomGiB float64) HostRAMCheck {
	if needGiB < 0 {
		needGiB = 0
	}
	if pendingGiB < 0 {
		pendingGiB = 0
	}
	if headroomGiB < 0 {
		headroomGiB = 0
	}
	c := HostRAMCheck{NeedGiB: needGiB, PendingGiB: pendingGiB, HeadroomGiB: headroomGiB}
	if needGiB == 0 {
		c.OK = true
		return c
	}
	if !readable || mem.PhysicalGiB <= 0 {
		if !supported {
			c.OK = true // no reader on this platform: the rule cannot judge, and a refusal here would be permanent
			return c
		}
		c.Unreadable = true
		c.Why = fmt.Sprintf("waiting for host RAM: the host's memory cannot be read, so a lease that needs %.1f GiB cannot be shown to fit", needGiB)
		return c
	}
	c.CommittedGiB, c.PhysicalGiB, c.AvailableGiB = mem.CommitUsedGiB, mem.PhysicalGiB, mem.AvailableGiB
	c.LimitGiB = mem.PhysicalGiB - headroomGiB
	c.ProjectedGiB = mem.CommitUsedGiB + needGiB + pendingGiB
	switch {
	case needGiB > c.LimitGiB:
		c.Impossible = true
		c.Why = fmt.Sprintf("host RAM can never admit this: it needs %.1f GiB, but %.1f GiB of physical RAM less %.1f GiB of headroom leaves %.1f GiB; "+
			"state the real need (gpu reserve --ram <GiB>, 0 = none) or lower gpu_host_ram_headroom_gib", needGiB, mem.PhysicalGiB, headroomGiB, max(c.LimitGiB, 0))
	case c.ProjectedGiB > c.LimitGiB:
		pend := ""
		if pendingGiB > 0 {
			pend = fmt.Sprintf(" (+%.1f GiB still to load by leases already running)", pendingGiB)
		}
		c.Why = fmt.Sprintf("waiting for host RAM: needs %.1f GiB, committed %.1f of %.1f GiB physical%s, %.1f GiB headroom",
			needGiB, mem.CommitUsedGiB, mem.PhysicalGiB, pend, headroomGiB)
	case mem.AvailableGiB-needGiB-pendingGiB < headroomGiB:
		pend := ""
		if pendingGiB > 0 {
			pend = fmt.Sprintf(" (+%.1f GiB still to load by leases already running)", pendingGiB)
		}
		c.Why = fmt.Sprintf("waiting for host RAM: needs %.1f GiB, only %.1f GiB of %.1f GiB physical is available%s, %.1f GiB headroom",
			needGiB, mem.AvailableGiB, mem.PhysicalGiB, pend, headroomGiB)
	default:
		c.OK = true
	}
	return c
}

// parseMeminfo reads the counters the Linux reader needs from /proc/meminfo text, all in kB:
// MemTotal and MemAvailable are required (without them there is no reading); Committed_AS and
// CommitLimit are the kernel's commit accounting and are present on every kernel this harness runs on,
// but a sandbox that virtualises /proc/meminfo (a container's lxcfs, gVisor) can omit them. Then the
// reading falls back to what the box visibly uses: commit used = MemTotal - MemAvailable, limit =
// MemTotal. That under-counts memory a process reserved and never touched, but it keeps the rule
// working: the alternative, no reading on a platform that has a reader, makes every lease that
// declares host RAM wait forever.
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
	if !seen["Committed_AS"] || !seen["CommitLimit"] {
		committed, limit = total-avail, total
	}
	return HostMemory{PhysicalGiB: total, AvailableGiB: avail, CommitUsedGiB: committed, CommitLimitGiB: limit}, true
}
