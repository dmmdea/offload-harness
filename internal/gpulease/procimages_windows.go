//go:build windows

package gpulease

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// RunningImages lists the image of every running process for which want returns a
// non-empty reason. want sees the pid and the executable's base name (from the process
// snapshot, which needs no access to the process itself), so only the processes worth
// auditing are opened. A process that cannot be opened is returned with an empty Path:
// the audit reports it rather than skipping it, because "could not see it" is not "it
// is fine".
func RunningImages(want func(pid int, exeName string) string) ([]ProcImage, error) {
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
	var out []ProcImage
	for {
		pid := int(pe.ProcessID)
		if why := want(pid, windows.UTF16ToString(pe.ExeFile[:])); why != "" {
			out = append(out, ProcImage{PID: pid, Path: imagePathOf(uint32(pe.ProcessID)), Why: why})
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	return out, nil
}

// imagePathOf asks for a process's full image path with the limited right, which is
// granted across integrity levels. "" when it cannot be read.
func imagePathOf(pid uint32) string {
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}
