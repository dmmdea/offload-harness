package gpulease

import (
	"strconv"
	"strings"
)

// processMemory is one process's memory in bytes: private (what it has committed that no other process shares;
// the currency of the system commit charge: PrivateUsage on Windows, the private writable mappings, VmData, on Linux,
// which is what the kernel's Committed_AS charges when a mapping is made, touched or not) and resident (the working
// set: what is in physical RAM now). ok is
// false when the process cannot be read, which is unreadable, never zero. Per-platform; the readers are in
// procmem_windows.go, procmem_linux.go and procmem_other.go.
//
// The two are different quantities and the difference is the point of recording both (G3 of the P0 plan): on
// Windows a process's GPU allocations may be charged to its private bytes (commit) and not appear in its working
// set, so private can read high by the VRAM it holds while resident is the RAM it actually occupies. Which
// quantity a number is says which question it answers.
func processMemory(pid int) (private, resident uint64, ok bool) { return readProcessMemory(pid) }

// privateBytes is the private half of processMemory.
func privateBytes(pid int) (uint64, bool) {
	p, _, ok := processMemory(pid)
	return p, ok
}

// parseStatusResident reads VmRSS (kB) out of /proc/<pid>/status text and returns it in bytes: the Linux
// working set. Platform-neutral so the parser is tested on every host.
func parseStatusResident(text string) (uint64, bool) {
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != "VmRSS" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// parseStatusPrivate reads VmData (kB) out of /proc/<pid>/status text and returns it in bytes: the
// process's private writable mappings, touched or not. That is what the kernel's commit accounting
// charges (Committed_AS grows by a private writable mapping when it is made), so it is the Linux
// counterpart of the Windows PrivateUsage. RssAnon + VmSwap would be only the part of those mappings
// that has been touched: a lease whose process committed memory it never touched (a CUDA host
// reservation, a large allocation) would be counted in Committed_AS and again as the part of its
// declared commit still to be charged. VmData leaves out the main thread's stack and shared memory
// (tmpfs, SysV), which the kernel charges and VmData does not, so the figure can read a little low,
// which leaves more of a declaration pending: the over-refusing direction, never the over-admitting
// one. A status without a VmData line is unreadable rather than guessed from VmRSS (which counts
// shared file pages that are not this process's own commit). Platform-neutral so the parser is
// tested on every host.
func parseStatusPrivate(text string) (uint64, bool) {
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != "VmData" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
