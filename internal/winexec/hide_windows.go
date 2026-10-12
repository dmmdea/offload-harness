//go:build windows

package winexec

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process-creation flag.
const createNoWindow = 0x08000000

var getConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// processHasConsole reports whether this process is attached to a console. GetConsoleProcessList
// lists the processes on the console and gives 0 when there is none; a console without a window (the
// hidden one a CREATE_NO_WINDOW parent owns) counts, which GetConsoleWindow would not say. It is a var
// so a test can pose as a parent with or without a console.
var processHasConsole = func() bool {
	var pid uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pid)), 1)
	return n != 0
}

func attrOf(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// HideWindow makes cmd's child window-less and returns cmd, for a child the harness waits for.
//
// The child's own window is hidden (SW_HIDE) always. The child is also started with CREATE_NO_WINDOW
// when this process has no console to hand on: a console program started by a process without a
// console gets a NEW console window, which with Windows Terminal as the default terminal is a Windows
// Terminal window on the operator's desktop. When this process does have a console (the operator's
// terminal, or the hidden one a hidden parent owns) the child inherits it and no window can appear,
// and it stays on that console, so the operator's Ctrl-C and a closed terminal reach it as they reach
// this process. CREATE_NO_WINDOW would move it to a console of its own and an interrupted run would
// leave it running with no window to show it. Other SysProcAttr fields and creation flags the caller
// already set are kept.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	attr := attrOf(cmd)
	attr.HideWindow = true
	if !processHasConsole() {
		attr.CreationFlags |= createNoWindow
	}
	return cmd
}

// HideWindowDetached makes cmd's child window-less on a console of its own whatever this process has,
// and returns cmd. It is for a child that must not share this process's console because it outlives
// this process or must not die with it: a window of the terminal the operator closes ends every
// process on that console, and a detached holder that dies silently leaves a card unreserved.
func HideWindowDetached(cmd *exec.Cmd) *exec.Cmd {
	attr := attrOf(cmd)
	attr.HideWindow = true
	attr.CreationFlags |= createNoWindow
	return cmd
}

// HideWindowIfNoConsole hides cmd's child only when this process has no console, and returns cmd.
// With a console the command is left exactly as exec built it: it is a program the operator named
// and runs at their terminal, where it may be interactive or open a window of its own that they want.
// Without one nobody is there to see a window, so it is hidden like any other child.
func HideWindowIfNoConsole(cmd *exec.Cmd) *exec.Cmd {
	if processHasConsole() {
		return cmd
	}
	return HideWindowDetached(cmd)
}
