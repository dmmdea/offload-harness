//go:build !windows

package hanger

import "os/exec"

// hideWindow has nothing to do where there are no console windows.
func hideWindow(cmd *exec.Cmd) {}
