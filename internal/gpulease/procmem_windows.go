//go:build windows

package gpulease

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procK32GetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCountersEx mirrors PROCESS_MEMORY_COUNTERS_EX. The size fields are SIZE_T, so
// uintptr keeps the layout right on both 32- and 64-bit builds.
type processMemoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// privateBytes is the process's PrivateUsage: the memory it has committed that no other process
// shares. It is the same currency as the system commit charge the grant compares against, which
// is why it is the number subtracted from a lease's declared need. The limited-query right is
// tried first, because it is granted across integrity levels; a process in another security
// context that refuses both is unreadable, not zero (the caller decides what unreadable means).
func privateBytes(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	const (
		queryLimited = 0x1000
		queryAndRead = 0x0400 | 0x0010 // PROCESS_QUERY_INFORMATION | PROCESS_VM_READ
	)
	for _, access := range []uint32{queryLimited, queryAndRead} {
		h, err := syscall.OpenProcess(access, false, uint32(pid))
		if err != nil {
			continue
		}
		var c processMemoryCountersEx
		c.Cb = uint32(unsafe.Sizeof(c))
		r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb))
		_ = syscall.CloseHandle(h)
		if r != 0 {
			return uint64(c.PrivateUsage), true
		}
	}
	return 0, false
}
