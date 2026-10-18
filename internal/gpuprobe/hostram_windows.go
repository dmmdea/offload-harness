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

// globalMemoryStatus fills m from GlobalMemoryStatusEx (m.Length already set) and says whether the call succeeded. A variable so a
// test can stand a MEMORYSTATUSEX in and pin which counter reaches which field of the reading; production never replaces it.
var globalMemoryStatus = func(m *memoryStatusEx) bool {
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(m)))
	return r != 0
}

// readHostMemory reads GlobalMemoryStatusEx. AvailPhys is the memory Windows can hand out
// without paging, which is what a multi-GB expert load actually needs. On this call ullTotalPageFile
// is the system COMMIT LIMIT (physical plus page file, not the page file alone) and ullAvailPageFile
// is the commit still available, so commit used is their difference: the figure Task Manager shows
// as "Committed". The limit is the CURRENT one: with a system-managed page file Windows grows the file as the charge nears
// it and the next call returns a larger limit (see the header of hostmemory.go); the configured maximum is not read. (The API
// documents both counters as "for the system or the current process, whichever is smaller": they are the system's unless the
// process runs under a job object with a commit limit, which the harness does not set up and this reader does not check.)
func readHostMemory() (HostMemory, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if !globalMemoryStatus(&m) {
		return HostMemory{}, false
	}
	return hostMemoryFromStatus(m.TotalPhys, m.AvailPhys, m.TotalPageFile, m.AvailPageFile)
}
