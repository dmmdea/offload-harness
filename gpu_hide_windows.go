//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow makes a spawned child window-less on Windows.
//
// This is not cosmetic. A visible console window gets closed by hand, and closing it
// kills the process behind it — a detached GPU holder that dies silently leaves the
// card unreserved while its operator believes it is protected, which is worse than
// never having reserved it. CREATE_NO_WINDOW plus HideWindow covers both the console
// allocation and the window itself.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
