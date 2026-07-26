//go:build windows

package gpulease

import "syscall"

// processStart returns an opaque process-start identity for pid, used ONLY for
// equality comparison against a recorded value. On Windows the creation FILETIME
// is exact and monotonic enough for this purpose.
//
// Why this exists: a lease records its holder's pid, and Windows recycles pids.
// Without a start-time component, a recycled pid makes a dead holder read as alive
// forever and the card is never reclaimed. Comparing creation time makes "same pid,
// different process" detectable.
//
// PROCESS_QUERY_LIMITED_INFORMATION is deliberate: it succeeds against processes in
// other security contexts (an elevated or SYSTEM holder) where the broader
// PROCESS_QUERY_INFORMATION would fail with access-denied. This package must work
// across exactly those boundaries — a per-user-only view is the class of bug it
// exists to fix.
func processStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)

	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	// Nanoseconds() converts the FILETIME to Unix nanoseconds; milliseconds are
	// ample resolution for identity and keep the JSON record small.
	return creation.Nanoseconds() / 1e6, true
}
