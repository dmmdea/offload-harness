// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build windows

package measure

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. A console window flashing up on every
// nvidia-smi poll (or, worse, a scratch llama-server that owns a visible
// console the operator can close) is a house-rule violation: visible windows
// interrupt the desktop and get closed, killing the child.
const createNoWindow = 0x08000000

// HideWindow configures cmd so the child process never creates a console
// window. Exported so command code that spawns servers uses the same
// treatment as the measurement helpers.
func HideWindow(cmd *exec.Cmd) { hideWindow(cmd) }

func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
