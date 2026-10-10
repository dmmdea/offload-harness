//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package mediaremote

import (
	"errors"
	"os"
)

// A platform with no file lock this package knows cannot make the machine-wide claim, so the queue refuses to start rather
// than claim a node without it: a call that finds no ticket can be made runs on the lane it has, as it did before tickets.
func tryLockFile(*os.File) error {
	return errors.New("the ticket queue needs an operating-system file lock, which this platform does not offer here")
}

func unlockFile(*os.File) {}
