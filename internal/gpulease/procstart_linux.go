//go:build linux

package gpulease

import (
	"os"
	"strconv"
	"strings"
)

// processStart returns an opaque process-start identity for pid (Linux): field 22
// of /proc/<pid>/stat, the process start time in clock ticks since boot. Used ONLY
// for equality comparison against a recorded value, so the unit and epoch do not
// matter — only that the same process yields the same number and a recycled pid
// yields a different one.
//
// Parsing note: field 2 (comm) is parenthesized and MAY CONTAIN SPACES and even
// ')' , so a naive Fields() split is wrong. We cut at the LAST ')' and index the
// remaining fields, which is the documented-safe way to parse this file.
func processStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	close := strings.LastIndex(s, ")")
	if close < 0 || close+2 >= len(s) {
		return 0, false
	}
	// After "pid (comm) ", fields resume at state (field 3). starttime is field 22,
	// i.e. index 19 in this remainder (22 - 3 = 19).
	rest := strings.Fields(s[close+2:])
	const startTimeIndexAfterComm = 19
	if len(rest) <= startTimeIndexAfterComm {
		return 0, false
	}
	v, err := strconv.ParseInt(rest[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
