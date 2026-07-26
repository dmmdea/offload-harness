//go:build windows

package gpulease

import (
	"errors"
	"syscall"
)

// pidAliveImpl reports process liveness on Windows.
//
// Two deliberate choices, both because this package must work ACROSS security
// contexts (a SYSTEM-registered service holding the card while a user session asks
// for it):
//
//  1. PROCESS_QUERY_LIMITED_INFORMATION, not PROCESS_QUERY_INFORMATION. The limited
//     right is granted across integrity levels; the broader one is not, and using it
//     made every elevated/SYSTEM holder look dead. (procstart_windows.go already used
//     the limited right for the same reason — the two were inconsistent.)
//
//  2. ACCESS_DENIED means ALIVE, not gone. An access failure proves the process
//     EXISTS and that we may not inspect it; only a lookup that says "no such
//     process" is evidence of death. This also matches the Node side, which maps
//     EPERM to alive — if the two disagree, one of them reclaims a lease the other
//     believes it holds.
func pidAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Present but not inspectable => alive. Anything else (notably
		// ERROR_INVALID_PARAMETER for an unknown pid) => gone.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		// The handle opened, so the process object exists. Fail SAFE: treat an
		// unreadable exit code as alive rather than reclaiming someone's GPU.
		return true
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
