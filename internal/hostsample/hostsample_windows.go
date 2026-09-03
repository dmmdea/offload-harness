//go:build windows

package hostsample

import (
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes    = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type filetime struct{ lo, hi uint32 }

func (f filetime) f64() float64 { return float64(uint64(f.hi)<<32 | uint64(f.lo)) }

func readCPU() (cpuTimes, bool) {
	var idle, kernel, user filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return cpuTimes{}, false
	}
	// kernel time INCLUDES idle on Windows.
	return cpuTimes{idle: idle.f64(), total: kernel.f64() + user.f64()}, true
}

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

func readRAM() (usedGiB, totalGiB float64, ok bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.TotalPhys == 0 {
		return 0, 0, false
	}
	g := float64(1 << 30)
	return float64(m.TotalPhys-m.AvailPhys) / g, float64(m.TotalPhys) / g, true
}
