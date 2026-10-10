package mediaops

import (
	"errors"
	"strings"
	"syscall"
)

// platformTransientRename: Windows reports a file another process has open without delete sharing as
// ERROR_SHARING_VIOLATION (32) or ERROR_LOCK_VIOLATION (33), which an antivirus scanner holding the
// staged file just written produces. Access denied (5) is already read as a permission error.
func platformTransientRename(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 32 || errno == 33)
}

// pathsEqual: NTFS paths compare without regard to case.
func pathsEqual(a, b string) bool { return strings.EqualFold(a, b) }
