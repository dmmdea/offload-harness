package gpugen

import (
	"errors"
	"regexp"
	"syscall"
)

// diskFullToken matches the errno a child process reports a full volume with, as a TOKEN and never as
// prose: Node writes the name ("ENOSPC: no space left on device, write"), Python the number
// ("[Errno 28] No space left on device"; 122 is EDQUOT, 30 is EROFS; "[WinError 112]" is Windows' own
// full disk). The words around the token are not matched, because a failure's text can carry text
// that is not the failure: a ComfyUI exec error echoes the failing node's inputs, the prompt among
// them, and a prompt that mentions a full disk must not read as one (the render helpers apply the
// same rule: isDiskFullError in render/batch-jobs.mjs).
var diskFullToken = regexp.MustCompile(`\b(?:ENOSPC|EDQUOT|EROFS)\b|\[(?:Errno (?:28|122|30)|WinError 112)\]`)

// IsDiskFull reports whether err says the volume an output goes to cannot take another byte (full,
// over its quota, or remounted read-only): by the TYPED errno when the error came from a Go file
// call in this process (a *PathError, *LinkError or *SyscallError wrapping the syscall errno), and
// by the errno token in its text when it crossed a process boundary (a render helper's output tail).
// Its wording is never read: the text of an error that carries no token is not a full disk.
func IsDiskFull(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && isDiskFullErrno(errno) {
		return true
	}
	return diskFullToken.MatchString(err.Error())
}

func isDiskFullErrno(e syscall.Errno) bool {
	return e == syscall.ENOSPC || e == syscall.EDQUOT || e == syscall.EROFS || platformDiskFull(e)
}
