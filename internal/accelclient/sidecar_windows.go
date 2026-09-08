//go:build windows

package accelclient

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the spawned launcher invisible (house rule: no visible
// console windows ever — they interrupt the operator and get closed, killing
// the service).
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
