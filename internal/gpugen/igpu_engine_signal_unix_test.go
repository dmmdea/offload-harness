//go:build !windows

package gpugen

import (
	"os"
	"strings"
	"syscall"
)

// selfSignal ends this (engine) process by the named signal, the way a crashing engine dies. The Go
// runtime owns SIGSEGV / SIGABRT / SIGBUS (it would exit 2 with a trace instead of dying of the
// signal), so the process image is replaced by a shell that kills itself with it: same pid, and the
// parent sees the real signal.
func selfSignal(name string) {
	_ = syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -s " + strings.TrimPrefix(name, "SIG") + " $$; sleep 30"}, os.Environ())
	os.Exit(98)
}
