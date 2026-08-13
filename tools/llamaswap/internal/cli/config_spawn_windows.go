// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW creation flag.
const createNoWindow = 0x08000000

// hideSpawnedWindow makes a child process completely invisible on Windows.
//
// Both mechanisms are set on purpose and they are not redundant:
//   - HideWindow drives the STARTUPINFO wShowWindow field (SW_HIDE), which
//     governs a GUI subsystem process.
//   - CREATE_NO_WINDOW stops a CONSOLE subsystem process — which llama-swap.exe
//     is — from allocating a console at all.
//
// Without the second one a throwaway validation instance flashes a console
// window over whatever the operator is doing, and a stray window on this box
// gets closed by reflex, killing the process under test.
func hideSpawnedWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
