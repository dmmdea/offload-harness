//go:build windows

package winexec

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process-creation flag.
const createNoWindow = 0x08000000

// HideWindow makes cmd's child window-less and returns cmd. CREATE_NO_WINDOW stops a console program
// from allocating a console window (the case that opens a Windows Terminal window when the parent has
// no console to hand on), and HideWindow sets the STARTUPINFO to SW_HIDE for a program that opens a
// window of its own. Both, as gpu_hide_windows.go does for the detached GPU holder. Other
// SysProcAttr fields and creation flags the caller already set are kept.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	return cmd
}
