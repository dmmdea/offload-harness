//go:build windows

package gpulease

import (
	"os"

	"golang.org/x/sys/windows"
)

// OpenShared opens a file for reading in a way that does not stand in the way of a rename or a delete of that file by
// someone else. Use it wherever a harness image (the installed executable, a backup beside it) is read while a rename-swap
// deploy may replace it.
//
// WHY THIS EXISTS: on Windows a rename of a file, or a delete of it, needs FILE_SHARE_DELETE on every handle already open
// against it, and Go's os.Open grants only FILE_SHARE_READ|FILE_SHARE_WRITE (syscall.Open on windows). A stale MCP server
// hashes the installed executable on the first call after a deploy and reads its build marker, so a rollback or a second swap
// that landed in that window failed with a sharing violation, and node-swap's holder diagnosis (it looks up processes by the
// executable path it is about to replace) cannot see a stale server whose image was already renamed away.
//
// The handle is a read handle for a regular file; reads, Stat and Close behave as on any *os.File. A file that does not
// exist is an error os.IsNotExist reports, as with os.Open.
func OpenShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
