package gpulease

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// pidAlive reports whether pid is a live process. Same semantics as
// internal/gpulock.pidAlive so the two views of process liveness can never
// disagree: EPERM still means alive (the process exists, we just may not signal
// it), and on Windows a successful OpenProcess is itself the proof.
//
// A package var so tests can substitute a deterministic "dead holder" without
// spawning real processes.
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows: OpenProcess failed => no such process
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		return true // FindProcess only opens live processes on Windows
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
