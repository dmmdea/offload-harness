//go:build windows

package sandbox

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The cage reports how a command ended from GetExitCodeProcess. When that call fails the command has
// run, its output is in hand, and what is unknown is how it ended. Run used to leave ExitCode at its
// zero value then, and every caller reads 0 as "it worked": a command that failed (or was killed) read
// as a success, with nothing to say otherwise.
//
// These tests make the call fail on demand through the readExitCode seam, like the label tests do for
// the Win32 label calls, so what they prove does not depend on the host, and assert what the CALLER is
// told: Result.ExitCode, Result.Stderr and the log.

// errExitRefused is the injected cause: a fixed, unmistakable string, so a note can only match it by
// carrying the error the read really returned.
var errExitRefused = errors.New("injected refusal of the exit-status read")

// swapExitCode puts a test-controlled stand-in in place of the exit-status read and restores the real
// call when the test ends.
func swapExitCode(t *testing.T, read func(process windows.Handle, code *uint32) error) {
	t.Helper()
	real := readExitCode
	t.Cleanup(func() { readExitCode = real })
	readExitCode = read
}

// TestWindowsUnreadableExitStatusIsNotTakenForSuccess: the run itself is unchanged (the command ran,
// its output is kept, no error, no refusal), but its exit code is not 0, and the caller is told, in
// Result.Stderr and once in the log, that the status could not be read and why.
func TestWindowsUnreadableExitStatusIsNotTakenForSuccess(t *testing.T) {
	wt, scratch := labelFolders(t)
	logged := captureLog(t)
	swapExitCode(t, func(windows.Handle, *uint32) error { return errExitRefused })

	res, err := runCmd(t, wt, scratch, "echo", "ran-anyway")
	if err != nil || res.Refused || !strings.Contains(res.Stdout, "ran-anyway") {
		t.Fatalf("the command ran, so its output is kept and the run is neither an error nor a refusal: err=%v refused=%v stdout=%q stderr=%q",
			err, res.Refused, res.Stdout, res.Stderr)
	}
	if res.ExitCode == 0 {
		t.Errorf("a run whose exit status could not be read reports exit 0, which every caller reads as success; stderr=%q", res.Stderr)
	}
	if res.ExitCode != -1 {
		t.Errorf("the exit code of a run with no readable status is -1 (the value os.ProcessState.ExitCode gives a process it has no code for); got %d", res.ExitCode)
	}
	if !strings.HasPrefix(res.Stderr, notePrefix) || !strings.Contains(res.Stderr, errExitRefused.Error()) || !strings.Contains(res.Stderr, "exit status") {
		t.Errorf("the caller must be told the exit status could not be read, and why, in a note that leads stderr; stderr=%q", res.Stderr)
	}
	if got := strings.Count(logged.String(), errExitRefused.Error()); got != 1 {
		t.Errorf("the log must carry the failure once; it carries it %d times: %q", got, logged.String())
	}
}

// TestWindowsExitCodeReachesTheCaller is the control for the test above: with the real call, the
// command's own exit code is what Run reports, 0 included, and there is nothing to say about it.
func TestWindowsExitCodeReachesTheCaller(t *testing.T) {
	for _, code := range []int{0, 1, 3, 42} {
		t.Run("exit "+strconv.Itoa(code), func(t *testing.T) {
			wt, scratch := labelFolders(t)
			logged := captureLog(t)

			res, err := runCmd(t, wt, scratch, "exit", strconv.Itoa(code))
			if err != nil || res.Refused {
				t.Fatalf("run failed: err=%v refused=%v stderr=%q", err, res.Refused, res.Stderr)
			}
			if res.ExitCode != code {
				t.Errorf("the command exited %d; Run reported %d", code, res.ExitCode)
			}
			if res.Stderr != "" || logged.Len() != 0 {
				t.Errorf("a status that was read has nothing to report; stderr=%q log=%q", res.Stderr, logged.String())
			}
		})
	}
}
