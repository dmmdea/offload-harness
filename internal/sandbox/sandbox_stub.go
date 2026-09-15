//go:build !linux && !windows

package sandbox

import (
	"context"
	"errors"
)

// ErrUnsupported is returned when the OS cage is requested on a non-Linux build.
// The Landlock / seccomp / user-namespace primitives the cage depends on are
// Linux-only, so on Windows (and any other OS) the cage cannot be enforced and
// MUST fail closed — a future shell capability stays ungranted there.
var ErrUnsupported = errors.New("sandbox: OS-level cage is Linux-only (Landlock/seccomp/user namespaces); unavailable on this platform")

// The cage's environment contract (sandbox.go) needs its four lists on every
// platform. They are all empty here: this build refuses to run a caged command
// at all, so there is no child to hand an environment to.
var (
	envFixed       []string
	envPassthrough []string
	envHomeNames   []string
	envTempNames   []string
)

// Available reports whether the OS cage can be enforced on this host. Always
// false off Linux.
func Available() (bool, string) {
	return false, "OS-level cage requires Linux (Landlock + seccomp + user namespaces); this is a non-Linux build"
}

// Run refuses on non-Linux: fail-closed, never run an un-caged command.
func Run(_ context.Context, _ Spec) (Result, error) {
	return Result{}, ErrUnsupported
}

// IsWorker is always false off Linux (there is no re-exec worker path).
func IsWorker() bool { return false }

// RunWorkerFromEnv is a no-op off Linux.
func RunWorkerFromEnv() {}
