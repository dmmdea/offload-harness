//go:build windows

package seatrate

import (
	"errors"
	"syscall"
)

// platformTransientRename: Windows reports a file another process has open without delete sharing as
// ERROR_SHARING_VIOLATION (32) or ERROR_LOCK_VIOLATION (33) when the handle collision is seen slightly
// earlier in the call than the access denial (5) a plain reader produces. Go maps only the denial to a
// permission error (syscall.Errno.Is), so these two are named here.
func platformTransientRename(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 32 || errno == 33)
}
