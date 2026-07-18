//go:build windows

package fleetnode

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
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
	var q uintptr
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return 0, fmt.Errorf("PdhOpenQuery: 0x%x", r)
	}
	defer pdhCloseQuery.Call(q)
	path, _ := syscall.UTF16PtrFromString(`\GPU Process Memory(*)\Dedicated Usage`)
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
