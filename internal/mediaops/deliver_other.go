//go:build !windows

package mediaops

// platformTransientRename: on a POSIX system the transient cases (EBUSY, EACCES, EPERM) are read by
// the portable check.
func platformTransientRename(error) bool { return false }

// pathsEqual: POSIX paths compare exactly.
func pathsEqual(a, b string) bool { return a == b }
