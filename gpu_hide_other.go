//go:build !windows

package main

import "os/exec"

// hideWindow is a no-op off Windows: a spawned child inherits no console window
// there, so there is nothing to hide.
func hideWindow(cmd *exec.Cmd) {}
