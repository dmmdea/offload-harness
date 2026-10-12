//go:build !windows

package proctree

import "os/exec"

// hideChild is a no-op off Windows: a child there has no console window to flash.
func hideChild(cmd *exec.Cmd) {}
