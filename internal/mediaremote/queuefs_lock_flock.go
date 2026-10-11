//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package mediaremote

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive flock on f without waiting. A flock belongs to the open file description, so a second open
// of the file (another goroutine, another process) is refused while the first is open, and the kernel drops the lock when
// the descriptor closes or its process ends, however it ends.
func tryLockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errLockHeld
		}
		return err
	}
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
