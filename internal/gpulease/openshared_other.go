//go:build !windows

package gpulease

import "os"

// sharedOpenMeetsDeletePending is false: no open here is refused for an instant that then passes. POSIX has no delete-pending state
// (rename(2) is atomic, and an open descriptor never holds a rename or an unlink up), so a permission error on an open is a lasting
// refusal, such as a lease record written with a mode the caller cannot read, and a wait can only add to the cost of reporting it.
// readSharedFile therefore returns it at once. openshared_windows.go has the state this is not.
const sharedOpenMeetsDeletePending = false

// OpenShared opens a file for reading in a way that does not stand in the way of a rename or a delete of that file by
// someone else. Off Windows that is os.Open: an open file descriptor never blocks a rename(2) or an unlink, so there is
// nothing to grant (openshared_windows.go says why Windows needs more).
func OpenShared(path string) (*os.File, error) { return os.Open(path) }
