//go:build windows

package proctree

import (
	"os/exec"
	"syscall"
)

// hideChild makes a helper process this package's tests start window-less: the tests start the test binary itself as
// the command, its child and its grandchild, and none of them may flash a console window.
func hideChild(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
