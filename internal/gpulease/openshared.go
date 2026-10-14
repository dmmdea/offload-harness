package gpulease

import "os"

// OpenShared opens a file for reading in a way that does not stand in the way of a rename or a delete of that file by
// someone else. Use it wherever a harness image (the installed executable, a backup beside it) is read while a rename-swap
// deploy may replace it.
//
// Seam: it is os.Open until the platform files take over (openshared_windows.go grants FILE_SHARE_DELETE).
func OpenShared(path string) (*os.File, error) { return os.Open(path) }
