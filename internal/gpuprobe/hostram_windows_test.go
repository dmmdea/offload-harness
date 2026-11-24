//go:build windows

package gpuprobe

import "testing"

// useGlobalMemoryStatus stands fn in for GlobalMemoryStatusEx until the returned function is called.
func useGlobalMemoryStatus(fn func(*memoryStatusEx) bool) (restore func()) {
	prev := globalMemoryStatus
	globalMemoryStatus = fn
	return func() { globalMemoryStatus = prev }
}

// The Windows reader hands four positional counters to hostMemoryFromStatus, and the conversion is pinned through its own seam
// (TestTheWindowsCountersBecomeAReadingWithAHardLimit). This pins the call itself: a MEMORYSTATUSEX whose fields all differ, so
// swapping AvailPhys with AvailPageFile, or TotalPhys with TotalPageFile, or any other pair, changes the reading. The live
// reading cannot tell them apart (its bounds hold for both orders), which is why a swap shipped green in a mutant run.
func TestTheWindowsReaderWiresEachCounterToItsField(t *testing.T) {
	const gib = 1 << 30
	var sawLength uint32
	restore := useGlobalMemoryStatus(func(m *memoryStatusEx) bool {
		sawLength = m.Length
		m.TotalPhys, m.AvailPhys = 128*gib, 72*gib
		m.TotalPageFile, m.AvailPageFile = 172*gib, 100*gib
		m.TotalVirtual, m.AvailVirtual, m.AvailExtendedVirtual = 9000*gib, 8000*gib, 7000*gib
		return true
	})
	defer restore()
	// The page files' room to grow is the kernel's own list (pagefile_windows.go); stood in, so the reading does not depend on
	// the machine running the test. It lands on top of the commit limit, as the ceiling.
	prevGrowth := pageFileGrowthGiB
	pageFileGrowthGiB = func() float64 { return 60 }
	defer func() { pageFileGrowthGiB = prevGrowth }()

	got, ok := readHostMemory()
	if !ok {
		t.Fatal("a call that succeeds is a reading")
	}
	want := HostMemory{PhysicalGiB: 128, AvailableGiB: 72, CommitUsedGiB: 72, CommitLimitGiB: 172, CommitCeilingGiB: 232}
	if got != want {
		t.Fatalf("reading %+v, want %+v: physical is TotalPhys, available AvailPhys, the commit limit TotalPageFile, the charge TotalPageFile less AvailPageFile, and the ceiling the limit plus what the page files can still grow", got, want)
	}
	if sawLength != 64 {
		t.Errorf("GlobalMemoryStatusEx refuses a structure whose Length is not set to its size (64), the reader passed %d", sawLength)
	}

	restore()
	defer useGlobalMemoryStatus(func(*memoryStatusEx) bool { return false })()
	if m, ok := readHostMemory(); ok {
		t.Fatalf("a failed call is no reading, got %+v", m)
	}
}
