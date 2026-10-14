//go:build !windows

package main

import "os/exec"

// setShellCmdLine is a no-op outside Windows: sh -c takes the line as one argument already.
func setShellCmdLine(cmd *exec.Cmd, line string) {}
