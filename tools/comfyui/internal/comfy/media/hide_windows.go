// Copyright 2026 dmmdea and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build windows

package media

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW creation flag.
const createNoWindow = 0x08000000

// hideWindow makes cmd's child window-less. A console program started by a process that has no
// console of its own (an MCP server started hidden, a service) gets a NEW visible console window on
// Windows unless it is started hidden, and with Windows Terminal as the default terminal that window
// is a Windows Terminal window on the operator's desktop. CREATE_NO_WINDOW stops the console
// allocation; HideWindow hides a window the program opens itself.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
