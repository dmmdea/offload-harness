package gpugen

import (
	"io/fs"
	"syscall"
	"testing"
)

// Windows reports a full volume as ERROR_DISK_FULL (112) or ERROR_HANDLE_DISK_FULL (39); the syscall
// package's ENOSPC is an invented value no real call returns, so the numbers are what must be read.
func TestIsDiskFullReadsTheWindowsErrnos(t *testing.T) {
	for _, n := range []syscall.Errno{112, 39} {
		err := &fs.PathError{Op: "write", Path: "a.png", Err: n}
		if !IsDiskFull(err) || ClassifyErr(err) != "disk_full" {
			t.Errorf("errno %d must be a full disk, got IsDiskFull=%v class=%q", n, IsDiskFull(err), ClassifyErr(err))
		}
	}
	if IsDiskFull(&fs.PathError{Op: "open", Path: "a.png", Err: syscall.Errno(5)}) {
		t.Error("ERROR_ACCESS_DENIED (5) is not a full disk")
	}
}
