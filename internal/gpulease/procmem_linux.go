//go:build linux

package gpulease

import (
	"os"
	"strconv"
)

// readProcessMemory is the process's private writable mappings (VmData in /proc/<pid>/status), touched or
// not: memory that belongs to this process alone and that the kernel's commit accounting charges, the
// counterpart of the Windows private commit; and its resident set (VmRSS), the working set. A process
// that has exited, or whose status this user cannot read, is unreadable.
func readProcessMemory(pid int) (private, resident uint64, ok bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0, false
	}
	p, pok := parseStatusPrivate(string(b))
	if !pok {
		return 0, 0, false
	}
	r, _ := parseStatusResident(string(b))
	return p, r, true
}
