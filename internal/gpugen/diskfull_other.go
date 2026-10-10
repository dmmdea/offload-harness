//go:build !windows

package gpugen

import "syscall"

// platformDiskFull: on a POSIX system ENOSPC, EDQUOT and EROFS are the whole class.
func platformDiskFull(syscall.Errno) bool { return false }
