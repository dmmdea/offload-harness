package winexec

import (
	"context"
	"os/exec"
)

// Command is exec.Command whose child opens no window on Windows (see HideWindow). On every other
// platform it is exec.Command.
//
// This is the constructor for every child the harness starts and waits for, so one rule holds in one
// place: a console program started by a process that has no console of its own (a detached holder, a
// service, a launcher started hidden) gets a NEW visible console window on Windows unless it is
// started hidden, and with Windows Terminal as the default terminal that window is a Windows Terminal
// window on the operator's desktop. One test run put several there, one titled taskkill.exe. A child
// that must outlive this process is CommandDetached, and a program the operator named is
// CommandIfNoConsole. A new direct os/exec constructor, a process created through the Win32 or
// os.StartProcess calls, or an exec.Cmd literal in a Go file of this module fails
// TestNoDirectExecInProduction until it uses these or is listed there with the reason it must stay as
// it is.
func Command(name string, arg ...string) *exec.Cmd {
	return HideWindow(exec.Command(name, arg...))
}

// CommandContext is exec.CommandContext whose child opens no window on Windows (see Command).
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	return HideWindow(exec.CommandContext(ctx, name, arg...))
}

// CommandDetached is exec.Command whose child always owns a hidden console of its own on Windows (see
// HideWindowDetached), for a child that outlives this process or must not share its console.
func CommandDetached(name string, arg ...string) *exec.Cmd {
	return HideWindowDetached(exec.Command(name, arg...))
}

// CommandIfNoConsole is exec.Command whose child is hidden only when this process has no console (see
// HideWindowIfNoConsole), for a program the operator names and may run at their terminal.
func CommandIfNoConsole(name string, arg ...string) *exec.Cmd {
	return HideWindowIfNoConsole(exec.Command(name, arg...))
}
