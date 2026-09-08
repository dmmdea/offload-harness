//go:build !windows

package accelclient

import "os/exec"

// hideWindow is a no-op off Windows; there is no console window to hide.
func hideWindow(*exec.Cmd) {}
