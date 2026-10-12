//go:build !windows

package seatrate

// platformTransientRename: off Windows a rename is never blocked by a reader's open file, so the portable
// permission check is all there is and a refusal that is not one is final.
func platformTransientRename(error) bool { return false }
