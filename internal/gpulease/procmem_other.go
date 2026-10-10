//go:build !windows && !linux

package gpulease

// readProcessMemory has no reader on this platform: a process's memory is unreadable, so a lease's whole
// declared need counts as still to load (the conservative reading).
func readProcessMemory(pid int) (private, resident uint64, ok bool) { return 0, 0, false }
