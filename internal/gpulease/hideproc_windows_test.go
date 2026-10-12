//go:build windows

package gpulease

import (
	"os/exec"
	"syscall"
)

// hideProc makes a helper process this package's tests start window-less: none of them may flash a console window.
func hideProc(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
