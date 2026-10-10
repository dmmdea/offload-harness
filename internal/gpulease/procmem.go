package gpulease

import (
	"strconv"
	"strings"
)

// processMemory is one process's memory in bytes: private (what it has committed that no other process shares;
// the currency of the system commit charge) and resident (the working set: what is in physical RAM now). ok is
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

// parseStatusPrivate reads RssAnon and VmSwap (kB) out of /proc/<pid>/status text and returns
// their sum in bytes. RssAnon is missing before Linux 4.5, which leaves the process unreadable
// rather than guessing from VmRSS (which counts shared file pages that are not this process's
// own commit). Platform-neutral so the parser is tested on every host.
func parseStatusPrivate(text string) (uint64, bool) {
	var anon, swap uint64
	haveAnon := false
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "RssAnon":
			anon, haveAnon = kb, true
		case "VmSwap":
			swap = kb
		}
	}
	if !haveAnon {
		return 0, false
	}
	return (anon + swap) * 1024, true
}
