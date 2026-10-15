//go:build windows

package main

import (
	"time"

	"golang.org/x/sys/windows"
)

var procPowerProfileTickCount64 = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64")

// powerProfileBootTime is when the operating system session began: now less GetTickCount64, the milliseconds
// since it started. A Fast Startup shutdown keeps the kernel session, so a power cycle through it does not
// move this time; the row counts "this boot" from the last kernel start. The zero time means the call failed.
func powerProfileBootTime(now time.Time) time.Time {
	ms, _, _ := procPowerProfileTickCount64.Call()
	if ms == 0 {
		return time.Time{}
	}
	return now.Add(-time.Duration(ms) * time.Millisecond)
}
