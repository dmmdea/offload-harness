//go:build !windows

package gpulease

// isEphemeralRenameError is unconditionally false off Windows: POSIX
// rename(2) atomically replaces the destination and is never blocked by a
// concurrent reader's open file descriptor, so renameReplacing degrades to
// exactly one os.Rename and behaviour is unchanged from before this fix.
func isEphemeralRenameError(err error) bool { return false }
