//go:build windows

package gpulease

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isEphemeralRenameError reports whether err is the transient failure
// produced when os.Rename's underlying MoveFileEx races a concurrent
// reader's open handle on the destination — see renameReplacing for why this
// happens and why it is safe to retry rather than a real fault.
//
// Both errnos are observed in the field for the same race, depending on
// exactly when the reader's handle is open relative to MoveFileEx's internal
// delete-then-link: ERROR_ACCESS_DENIED (measured: `gpulease: restamp:
// rename … : Access is denied.` under go test -race with a concurrent
// Inspect() poller) and ERROR_SHARING_VIOLATION (the same race, the error
// Windows returns when the handle collision is detected slightly earlier in
// the call).
func isEphemeralRenameError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION:
		return true
	}
	return false
}
