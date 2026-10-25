//go:build !windows && !linux

package forecastserve

import (
	"os/exec"
	"syscall"
)

// startBound starts cmd on a platform with no parent-death signal (the nodes that run this seat
// are Windows and Linux); stop asks the child to terminate.
func startBound(cmd *exec.Cmd) (release, stop func(), err error) {
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return func() {}, func() { _ = cmd.Process.Signal(syscall.SIGTERM) }, nil
}
