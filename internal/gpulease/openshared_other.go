//go:build !windows

package gpulease

import "os"

// OpenShared opens a file for reading in a way that does not stand in the way of a rename or a delete of that file by
// someone else. Off Windows that is os.Open: an open file descriptor never blocks a rename(2) or an unlink, so there is
// nothing to grant (openshared_windows.go says why Windows needs more).
func OpenShared(path string) (*os.File, error) { return os.Open(path) }
