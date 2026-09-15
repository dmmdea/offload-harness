//go:build !windows

package sandbox

import "testing"

// The Windows env-allowlist proof cannot run here, and it must SAY so rather
// than be absent.
//
// A guarantee whose test exists only behind a build tag reads as "passing" on
// every Linux CI run that never compiled it — the same silence that let a nil
// lpEnvironment ship. These stubs keep the names in `go test -v` output on
// every platform, as explicit skips.

func TestWindowsChildDoesNotInheritParentSecrets(t *testing.T) {
	t.Skip("windows-only: the env-block proof needs CreateProcessAsUser; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsChildGetsTheAllowlistedEnvironment(t *testing.T) {
	t.Skip("windows-only: the env-block proof needs CreateProcessAsUser; run `go test ./internal/sandbox/` on Windows")
}
