//go:build windows

package gpuprobe

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostRAMSupported tells the test whether a !ok from ReadHostMemory is a
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

// readHostMemory reads GlobalMemoryStatusEx. AvailPhys is the memory Windows can hand out
// without paging, which is what a multi-GB expert load actually needs. On this call ullTotalPageFile
// is the system COMMIT LIMIT (physical plus page file, not the page file alone) and ullAvailPageFile
// is the commit still available, so commit used is their difference: the figure Task Manager shows
// as "Committed". (The API documents both as "for the system or the current process, whichever
// is smaller"; a harness process has no job-object commit quota, so they are the system's.)
func readHostMemory() (HostMemory, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.TotalPhys == 0 {
		return HostMemory{}, false
	}
	const gib = 1 << 30
	used := uint64(0)
	if m.TotalPageFile > m.AvailPageFile {
		used = m.TotalPageFile - m.AvailPageFile
	}
	return HostMemory{
		PhysicalGiB:    float64(m.TotalPhys) / gib,
		AvailableGiB:   float64(m.AvailPhys) / gib,
		CommitUsedGiB:  float64(used) / gib,
		CommitLimitGiB: float64(m.TotalPageFile) / gib,
	}, true
}
