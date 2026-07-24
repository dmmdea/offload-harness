//go:build windows

package fleetnode

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

var (
	pdh                       = syscall.NewLazyDLL("pdh.dll")
	pdhOpenQuery              = pdh.NewProc("PdhOpenQueryW")
	pdhAddEnglishCounter      = pdh.NewProc("PdhAddEnglishCounterW")
	pdhCollectQueryData       = pdh.NewProc("PdhCollectQueryData")
	pdhGetFormattedCounterArr = pdh.NewProc("PdhGetFormattedCounterArrayW")
	pdhCloseQuery             = pdh.NewProc("PdhCloseQuery")
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32Snap  = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW       = kernel32.NewProc("Process32FirstW")
	procProcess32NextW        = kernel32.NewProc("Process32NextW")
	procCloseHandle           = kernel32.NewProc("CloseHandle")
)

const (
	pdhFmtDouble      = 0x00000200
	th32csSnapProcess = 0x00000002
)

// pdhFmtCountervalueItemDouble mirrors PDH_FMT_COUNTERVALUE_ITEM_W on amd64:
// szName (LPWSTR, offset 0) is already 8-byte aligned, so FmtValue follows at
// offset 8 with NO padding between them — sizeof is 24, and that stride is
// what PdhGetFormattedCounterArrayW packs the buffer with. (Live-fix
// 2026-07-17: the plan's draft carried a spurious uint32 pad here, inflating
// the struct to 32 bytes; item[1]'s Name then read as garbage and crashed the
// live smoke. The only real padding is inside PDH_FMT_COUNTERVALUE, after
// CStatus, modeled below.)
type pdhFmtCountervalueItemDouble struct {
	Name *uint16
	Val  pdhFmtCountervalueDouble
}
type pdhFmtCountervalueDouble struct {
	CStatus uint32
	_       uint32
	Double  float64
}

type processEntry32 struct {
	Size            uint32
	_               uint32
	ProcessID       uint32
	_               uintptr
	_               uint32
	_               uint32
	ParentProcessID uint32
	_               int32
	_               uint32
	ExeFile         [260]uint16
}

// descendants returns rootPid plus every transitive child pid (toolhelp snapshot).
func descendants(rootPid int) map[uint32]bool {
	set := map[uint32]bool{uint32(rootPid): true}
	snap, _, _ := procCreateToolhelp32Snap.Call(th32csSnapProcess, 0)
	if syscall.Handle(snap) == syscall.InvalidHandle {
		return set
	}
	defer procCloseHandle.Call(snap)
	// Collect (pid, ppid) pairs, then close over the tree (children may precede parents).
	type pp struct{ pid, ppid uint32 }
	var all []pp
	var e processEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e))); r != 0 {
		for {
			all = append(all, pp{e.ProcessID, e.ParentProcessID})
			if r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e))); r == 0 {
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, x := range all {
			if set[x.ppid] && !set[x.pid] {
				set[x.pid] = true
				changed = true
			}
		}
	}
	return set
}

// TreeDedicatedGiB sums \GPU Process Memory(pid_*)\Dedicated Usage over rootPid's tree.
// Counter instances are named like "pid_12345_luid_...". Returns an error when the
// counter set is unavailable (old Windows, broken counters) — callers fall back to
// the global-delta method.
func TreeDedicatedGiB(rootPid int) (float64, error) {
	return treeCounterGiB(rootPid, `\GPU Process Memory(*)\Dedicated Usage`)
}

// TreeDedicatedPlusSharedGiB is the UMA sampler mode (fleet_sampler:
// "pdh-shared", J3): on an iGPU, allocations land in SHARED system memory and
// per-process Dedicated Usage reads ~0 — so footprints silently never recorded
// on the amd-rdna3 tier (the audit's "invisible" break). Dedicated + Shared is
// the honest per-process cost there. Two PDH queries; same tree semantics.
func TreeDedicatedPlusSharedGiB(rootPid int) (float64, error) {
	ded, err := treeCounterGiB(rootPid, `\GPU Process Memory(*)\Dedicated Usage`)
	if err != nil {
		return 0, err
	}
	sh, err := treeCounterGiB(rootPid, `\GPU Process Memory(*)\Shared Usage`)
	if err != nil {
		return 0, err
	}
	return ded + sh, nil
}

// treeCounterGiB sums the given per-process GPU counter over rootPid's tree.
func treeCounterGiB(rootPid int, counterPath string) (float64, error) {
	var q uintptr
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return 0, fmt.Errorf("PdhOpenQuery: 0x%x", r)
	}
	defer pdhCloseQuery.Call(q)
	path, _ := syscall.UTF16PtrFromString(counterPath)
	var c uintptr
	if r, _, _ := pdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		return 0, fmt.Errorf("PdhAddEnglishCounter: 0x%x", r)
	}
	if r, _, _ := pdhCollectQueryData.Call(q); r != 0 {
		return 0, fmt.Errorf("PdhCollectQueryData: 0x%x", r)
	}
	var bufLen, itemCount uint32
	pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), 0)
	if bufLen == 0 {
		return 0, fmt.Errorf("no GPU Process Memory instances")
	}
	buf := make([]byte, bufLen)
	if r, _, _ := pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return 0, fmt.Errorf("PdhGetFormattedCounterArray: 0x%x", r)
	}
	// Bound-check itemCount against the buffer PDH actually filled before the
	// slice cast: a garbage count would index past buf and PANIC — and this
	// runs in the always-on sampler goroutine, where a panic kills the whole
	// harness mid-render. Implausible counts (0, > the cast's 1<<16 array
	// bound, or more items than fit in bufLen) are an error, not a crash.
	if itemCount == 0 || itemCount > 1<<16 || uintptr(itemCount)*unsafe.Sizeof(pdhFmtCountervalueItemDouble{}) > uintptr(bufLen) {
		return 0, fmt.Errorf("pdh: implausible item count %d (buf %d)", itemCount, bufLen)
	}
	tree := descendants(rootPid)
	items := (*[1 << 16]pdhFmtCountervalueItemDouble)(unsafe.Pointer(&buf[0]))[:itemCount:itemCount]
	var totalBytes float64
	for _, it := range items {
		name := syscall.UTF16ToString((*[256]uint16)(unsafe.Pointer(it.Name))[:])
		pid := pidFromInstance(name)
		if pid != 0 && tree[pid] && it.Val.CStatus == 0 {
			totalBytes += it.Val.Double
		}
	}
	return totalBytes / (1 << 30), nil
}

// sumAdapterCounterGiB sums a \GPU Adapter Memory(*) counter across ALL
// adapter instances (instances are "luid_0x..._phys_N"; a single-GPU box has
// one). Same PDH plumbing as the per-process tree, no pid filtering.
func sumAdapterCounterGiB(counterPath string) (float64, error) {
	var q uintptr
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return 0, fmt.Errorf("PdhOpenQuery: 0x%x", r)
	}
	defer pdhCloseQuery.Call(q)
	path, _ := syscall.UTF16PtrFromString(counterPath)
	var c uintptr
	if r, _, _ := pdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		return 0, fmt.Errorf("PdhAddEnglishCounter: 0x%x", r)
	}
	if r, _, _ := pdhCollectQueryData.Call(q); r != 0 {
		return 0, fmt.Errorf("PdhCollectQueryData: 0x%x", r)
	}
	var bufLen, itemCount uint32
	pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), 0)
	if bufLen == 0 {
		return 0, fmt.Errorf("no GPU Adapter Memory instances")
	}
	buf := make([]byte, bufLen)
	if r, _, _ := pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return 0, fmt.Errorf("PdhGetFormattedCounterArray: 0x%x", r)
	}
	if itemCount == 0 || itemCount > 1<<16 || uintptr(itemCount)*unsafe.Sizeof(pdhFmtCountervalueItemDouble{}) > uintptr(bufLen) {
		return 0, fmt.Errorf("pdh: implausible item count %d (buf %d)", itemCount, bufLen)
	}
	items := (*[1 << 16]pdhFmtCountervalueItemDouble)(unsafe.Pointer(&buf[0]))[:itemCount:itemCount]
	var totalBytes float64
	for _, it := range items {
		if it.Val.CStatus == 0 {
			totalBytes += it.Val.Double
		}
	}
	return totalBytes / (1 << 30), nil
}

// AdapterUsageGiB reads the WDDM adapter-level memory usage: Dedicated and
// Shared, summed across adapters. This is the windows-generic health source's
// "used" half — vendor-agnostic (WDDM counters exist for AMD/Intel/NVIDIA).
func AdapterUsageGiB() (dedicatedGiB, sharedGiB float64, err error) {
	d, err := sumAdapterCounterGiB(`\GPU Adapter Memory(*)\Dedicated Usage`)
	if err != nil {
		return 0, 0, err
	}
	s, err := sumAdapterCounterGiB(`\GPU Adapter Memory(*)\Shared Usage`)
	if err != nil {
		return 0, 0, err
	}
	return d, s, nil
}

// DedicatedVramTotalGiB reads the LARGEST HardwareInformation.qwMemorySize
// across the display-class registry keys — the same capacity source
// setup/detect.ps1 uses (AdapterRAM saturates at 4GB; qwMemorySize is the
// 64-bit truth). On an iGPU this is the BIOS carve-out, not real capacity —
// the UMA composition in GenericWindowsProbe accounts for that.
func DedicatedVramTotalGiB() (float64, error) {
	base := `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return 0, fmt.Errorf("display class key: %w", err)
	}
	defer k.Close()
	subs, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return 0, fmt.Errorf("display class subkeys: %w", err)
	}
	var maxBytes uint64
	for _, sub := range subs {
		if !strings.HasPrefix(sub, "0") {
			continue // only the 0000/0001/... adapter keys
		}
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, base+`\`+sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, verr := sk.GetIntegerValue("HardwareInformation.qwMemorySize")
		sk.Close()
		if verr == nil && v > maxBytes {
			maxBytes = v
		}
	}
	if maxBytes == 0 {
		return 0, fmt.Errorf("no adapter reports HardwareInformation.qwMemorySize")
	}
	return float64(maxBytes) / (1 << 30), nil
}

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

var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// SystemRAMGiB returns total physical RAM — the WDDM shared-budget input
// (Windows grants the GPU up to ~half of system RAM as shared memory).
func SystemRAMGiB() (float64, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %v", callErr)
	}
	return float64(m.TotalPhys) / (1 << 30), nil
}

// GenericWindowsProbe is the windows-generic MemProbe (J3): capacity from the
// registry, usage from the WDDM adapter counters. Composition depends on the
// memory model:
//   - discrete (uma=false): total = dedicated VRAM; used = adapter Dedicated.
//   - UMA iGPU (uma=true): the carve-out is NOT the capacity — the GPU can
//     address carve-out + the WDDM shared budget (~half of system RAM), and
//     real allocations land in Shared. total = carve-out + RAM/2;
//     used = adapter Dedicated + Shared.
//
// Every value is a live read; any failing source fails the probe (the sampler
// keeps its last good snapshot, same contract as the nvidia-smi source).
func GenericWindowsProbe(uma bool) MemProbe {
	return func() (float64, float64, error) {
		dedTotal, err := DedicatedVramTotalGiB()
		if err != nil {
			return 0, 0, err
		}
		ded, sh, err := AdapterUsageGiB()
		if err != nil {
			return 0, 0, err
		}
		if !uma {
			return dedTotal, ded, nil
		}
		ram, err := SystemRAMGiB()
		if err != nil {
			return 0, 0, err
		}
		return dedTotal + ram/2, ded + sh, nil
	}
}

// pidFromInstance extracts 12345 from "pid_12345_luid_0x..._phys_0". Pure; unit-tested.
func pidFromInstance(name string) uint32 {
	if !strings.HasPrefix(name, "pid_") {
		return 0
	}
	rest := name[4:]
	end := strings.IndexByte(rest, '_')
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.ParseUint(rest[:end], 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}
