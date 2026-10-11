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

// The same rule for the proof that the cage SAYS when it cannot label or unlabel a folder
// (sandbox_windows_label_test.go): the label code exists only on Windows.

func TestWindowsLabelRefusalIsReportedToTheCaller(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsRevertFailureIsReportedAndTrue(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsUnreadablePriorLabelIsReported(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsCleanRunCarriesNoCageNotes(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsLabelNotesSurviveACommandThatCannotStart(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsLabelNotesPrecedeTheChildsOwnStderr(t *testing.T) {
	t.Skip("windows-only: the integrity-label proof needs the Win32 security calls; run `go test ./internal/sandbox/` on Windows")
}

func TestWindowsKillFailureAtTheDeadlineIsReported(t *testing.T) {
	t.Skip("windows-only: the deadline proof needs a Windows Job Object; run `go test ./internal/sandbox/` on Windows")
}
