//go:build !windows

package gpulease

import "os/exec"

// hideProc is a no-op off Windows: a spawned child opens no console window there.
func hideProc(*exec.Cmd) {}
