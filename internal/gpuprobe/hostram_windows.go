//go:build windows

package gpuprobe

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostRAMSupported tells the test whether a !ok from HostFreeRAMGiB is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = true

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX for GlobalMemoryStatusEx.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// hostFreeRAMGiB reads AvailPhys — the memory Windows can hand out without
// paging, which is what a multi-GB expert load actually needs.
func hostFreeRAMGiB() (float64, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.TotalPhys == 0 {
		return 0, false
	}
	return float64(m.AvailPhys) / (1 << 30), true
}
