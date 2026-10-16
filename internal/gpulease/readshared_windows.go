//go:build windows

package gpulease

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openRecordShared opens a lease record for reading with the share mode of a POSIX reader: read, write AND delete.
//
// os.Open grants only FILE_SHARE_READ|FILE_SHARE_WRITE (see renameReplacing). A reader holding such a handle at the instant a writer
// renames a fresh record over the name fails the writer's rename, and a reader that opens the name while that rename is in flight
// fails with ERROR_SHARING_VIOLATION; measured on the card-scoped record, 1 read in 30 to 90 failed under a concurrent rewrite, and
// none of 491,000 did through this open. The handle keeps the record it opened (the old one, whole) until it is closed.
func openRecordShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	const share = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, share, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// longPath is the extended-length form of an absolute path too long for the legacy limit, which os.Open applies itself and a direct
// CreateFile does not. Shorter and relative paths are returned as they are.
func longPath(path string) string {
	const limit = 248 // the legacy limit for a directory name; a file path is held to 260, a margin short of both is safe
	if len(path) < limit || strings.HasPrefix(path, `\\?\`) || !filepath.IsAbs(path) {
		return path
	}
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
