//go:build !windows

package gpulease

import (
	"errors"
	"os"
	"syscall"
)

// pidAliveImpl reports process liveness on POSIX via signal 0.
//
// EPERM means the process EXISTS and belongs to another user — alive, not gone. That
// matches the Node side (`process.kill(pid, 0)` with the same EPERM handling); a
// disagreement between the two would let one reclaim a lease the other holds.
func pidAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
