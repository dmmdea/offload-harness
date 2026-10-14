//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// setShellCmdLine hands cmd.exe the whole command line as the OS will parse it. Go quotes each
// argument of exec.Command for the C runtime, and a line that contains quotes of its own (a quoted
// path) comes out mangled; cmd.exe /d /s /c "..." strips exactly the outer pair and keeps the inside
// as written, the form node's shell option uses.
func setShellCmdLine(cmd *exec.Cmd, line string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = `cmd.exe /d /s /c "` + line + `"`
}
