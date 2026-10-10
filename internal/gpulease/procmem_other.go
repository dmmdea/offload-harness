//go:build !windows && !linux

package gpulease

// privateBytes has no reader on this platform: a process's memory is unreadable, so a lease's whole
// declared need counts as still to load (the conservative reading).
func privateBytes(pid int) (uint64, bool) { return 0, false }
