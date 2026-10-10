//go:build linux

package gpulease

import (
	"os"
	"strconv"
)

// privateBytes is the process's anonymous resident memory plus what of it is swapped out
// (RssAnon + VmSwap in /proc/<pid>/status): memory that belongs to this process alone, the
// nearest thing /proc offers to the Windows private commit. A process that has exited, or whose
// status this user cannot read, is unreadable.
func privateBytes(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	return parseStatusPrivate(string(b))
}
