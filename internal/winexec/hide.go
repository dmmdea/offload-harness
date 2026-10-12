package winexec

import (
	"context"
	"os/exec"
)

// Command is exec.Command whose child opens no window on Windows (see HideWindow). On every other
// platform it is exec.Command.
//
// This is the constructor for every child the harness starts, so one rule holds in one place: a
// console program started by a process that has no console of its own (a detached holder, a service,
// a launcher started hidden) gets a NEW visible console window on Windows unless it is started hidden,
// and with Windows Terminal as the default terminal that window is a Windows Terminal window on the
// operator's desktop. One test run put several there, one titled taskkill.exe. A new direct
// os/exec constructor in production code fails windows_hide_lint_test.go until it uses these or is
// listed there with the reason it must stay visible.
func Command(name string, arg ...string) *exec.Cmd {
	return HideWindow(exec.Command(name, arg...))
}

// CommandContext is exec.CommandContext whose child opens no window on Windows (see Command).
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	return HideWindow(exec.CommandContext(ctx, name, arg...))
}
