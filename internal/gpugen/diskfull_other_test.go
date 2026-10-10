//go:build !windows

package gpugen

import (
	"io/fs"
	"syscall"
	"testing"
)

// 112 and 39 mean a full disk only on Windows: on a POSIX system 112 is EHOSTDOWN and 39 is ENOTEMPTY
// (Linux), and neither may be recorded as a full disk.
func TestIsDiskFullDoesNotReadWindowsNumbersOnPOSIX(t *testing.T) {
	for _, n := range []syscall.Errno{112, 39} {
		if IsDiskFull(&fs.PathError{Op: "write", Path: "a.png", Err: n}) {
			t.Errorf("errno %d is not a full disk on this platform", n)
		}
	}
}
