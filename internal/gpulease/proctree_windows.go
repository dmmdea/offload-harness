//go:build windows

package gpulease

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTreeImpl enumerates the process table with a Toolhelp snapshot (parent links need no
// access to the processes themselves), selects root's descendants, and reads the command line
// of each from its PEB where the right is granted.
func processTreeImpl(root int) ([]TreeProc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	var table []TreeProc
	for {
		table = append(table, TreeProc{PID: int(pe.ProcessID), PPID: int(pe.ParentProcessID)})
		if err := windows.Process32Next(snap, &pe); err != nil {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	starts := map[int]int64{}
	start := func(pid int) (int64, bool) {
		if v, ok := starts[pid]; ok {
			return v, v != 0
		}
		v, ok := processStart(pid)
		if !ok {
			v = 0
		}
		starts[pid] = v
		return v, ok
	}
	out := descendantsOf(root, table, start)
	for i := range out {
		if v, ok := start(out[i].PID); ok {
			out[i].StartMs = v
		}
		out[i].Cmdline = commandLineOf(uint32(out[i].PID))
	}
	return out, nil
}

// commandLineOf reads a process's command line from its PEB. "" when it cannot be read: a
// process in another security context, an exited one, or a 32-bit process on a 64-bit host
// (whose PEB has another layout, and which is never a ComfyUI interpreter here).
func commandLineOf(pid uint32) string {
	const access = windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ
	h, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var pbi windows.PROCESS_BASIC_INFORMATION
	var retLen uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &retLen); err != nil || pbi.PebBaseAddress == nil {
		return ""
	}
	var n uintptr
	var peb windows.PEB
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress)), (*byte)(unsafe.Pointer(&peb)), unsafe.Sizeof(peb), &n); err != nil || peb.ProcessParameters == nil {
		return ""
	}
	var params windows.RTL_USER_PROCESS_PARAMETERS
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), (*byte)(unsafe.Pointer(&params)), unsafe.Sizeof(params), &n); err != nil {
		return ""
	}
	cl := params.CommandLine
	if cl.Length == 0 || cl.Buffer == nil {
		return ""
	}
	buf := make([]uint16, cl.Length/2)
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(cl.Buffer)), (*byte)(unsafe.Pointer(&buf[0])), uintptr(cl.Length), &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// startUnixMs: on Windows a TreeProc.StartMs is already the process creation time in Unix
// milliseconds (procstart_windows.go).
func startUnixMs(startMs int64) (int64, bool) { return startMs, true }
