package gpugen

import "syscall"

// platformDiskFull: Windows reports a full volume as ERROR_DISK_FULL (112) or, on a handle that
// cannot grow, ERROR_HANDLE_DISK_FULL (39). The syscall package names neither, and its ENOSPC is an
// invented value no real call returns, so the numbers are the check.
func platformDiskFull(e syscall.Errno) bool {
	return e == 112 || e == 39
}
