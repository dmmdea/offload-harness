//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// powerProfileBootTime is when the kernel started, from the btime line of /proc/stat. The zero time means the
// platform has no such file (or it names no boot time), which the row treats as an unknown boot time.
func powerProfileBootTime(now time.Time) time.Time {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || sec <= 0 {
			return time.Time{}
		}
		return time.Unix(sec, 0)
	}
	return time.Time{}
}
