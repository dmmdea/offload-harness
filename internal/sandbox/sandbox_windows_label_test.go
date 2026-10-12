//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/dmmdea/offload-harness/internal/proctree"
)

// The cage lowers the mandatory integrity label of the run's folders so that a low-integrity
// child can write them, and puts the label back afterwards. Either step can be refused: a folder
// whose owner lacks WRITE_OWNER refuses the lowering, and a restore can find the folder changed
// under it. Run used to carry on without a word. The caller had asked for a writable worktree,
// did not get one, and saw only the child's unexplained "Access is denied".
//
// These tests make the label calls fail on demand through the setLabelSACL / getLabelSD seams, so
// what they prove does not depend on the ACLs of the machine that runs them, and they assert what
// the CALLER is told: Result.Stderr, the error of a run that never started, and the log.

// notePrefix is the text every line the cage adds to a run's stderr starts with. Spelled out here
// rather than read from the package, so a change to the shape callers see fails a test.
const notePrefix = "[sandbox] "

// errRefused is the injected cause. A fixed, unmistakable string: an assertion on it can only be
// met by a note that carries the error the label call really returned. "Some note exists" would be
// met just as well by a machine whose own ACLs refuse the call, which is the dependency these tests
// exist to remove.
var errRefused = errors.New("injected refusal of the label call")

// swapLabelCalls puts test-controlled stand-ins in place of the two Win32 calls the label code
// makes and restores the real ones when the test ends. set sees every attempt to write a folder's
// label (call counts per folder from 1: the first is the lowering, the second the restore) and
// returns an error to refuse it; the real call runs only when set returns nil. get does the same for
// reading a folder's label. Either may be nil.
func swapLabelCalls(t *testing.T, set func(call int, dir string) error, get func(dir string) error) {
	t.Helper()
	realSet, realGet := setLabelSACL, getLabelSD
	t.Cleanup(func() { setLabelSACL, getLabelSD = realSet, realGet })
	calls := map[string]int{}
	setLabelSACL = func(dir string, sacl *windows.ACL) error {
		calls[dir]++
		if set != nil {
			if err := set(calls[dir], dir); err != nil {
				return err
			}
		}
		return realSet(dir, sacl)
	}
	getLabelSD = func(dir string) (*windows.SECURITY_DESCRIPTOR, error) {
		if get != nil {
			if err := get(dir); err != nil {
				return nil, err
			}
		}
		return realGet(dir)
	}
}

// captureLog sends the standard logger's output to the returned buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

// linesWith counts the lines of s that contain sub.
func linesWith(s, sub string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// removeLabel takes any mandatory label off dir, for a test that deliberately leaves one behind.
func removeLabel(dir string) {
	_ = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, nil)
}

// labelFolders returns a labelable worktree and a scratch folder that is NOT inside it, so each
// folder's label is lowered and restored independently of the other's.
func labelFolders(t *testing.T) (wt, scratch string) {
	t.Helper()
	return labelableTempDir(t), filepath.Join(labelableTempDir(t), "scratch")
}

// runCmd runs cmd.exe /c <tail...> in the given folders with the worktree writable.
func runCmd(t *testing.T, wt, scratch string, tail ...string) (Result, error) {
	t.Helper()
	return Run(context.Background(), Spec{
		Argv:               append([]string{cmdExe(t), "/c"}, tail...),
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            scratch,
		AllowedExecutables: []string{"cmd"},
	})
}

// TestWindowsLabelRefusalIsReportedToTheCaller: when the label of the worktree, of the scratch
// folder or of both is refused, the command still runs (that is unchanged: failing closed is a
// separate decision) and the caller is told, for each refused folder, which folder and why, in
// Result.Stderr and once in the log. A folder whose label went through is not named.
func TestWindowsLabelRefusalIsReportedToTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		refuseWorktree, refuseScratch bool
	}{
		{"worktree", true, false},
		{"scratch", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt, scratch := labelFolders(t)
			logged := captureLog(t)
			swapLabelCalls(t, func(call int, dir string) error {
				if call == 1 && ((dir == wt && tc.refuseWorktree) || (dir == scratch && tc.refuseScratch)) {
					return errRefused
				}
				return nil
			}, nil)

			res, err := runCmd(t, wt, scratch, "echo", "ran-anyway")
			if err != nil {
				t.Fatalf("a refused label must not stop the command: %v (stderr=%q)", err, res.Stderr)
			}
			if res.Refused || res.ExitCode != 0 || !strings.Contains(res.Stdout, "ran-anyway") {
				t.Fatalf("the command should have run as before; refused=%v exit=%d stdout=%q stderr=%q",
					res.Refused, res.ExitCode, res.Stdout, res.Stderr)
			}

			refusals := 0
			for _, f := range []struct {
				dir     string
				refused bool
			}{{wt, tc.refuseWorktree}, {scratch, tc.refuseScratch}} {
				inStderr := linesWith(res.Stderr, f.dir)
				inLog := linesWith(logged.String(), f.dir)
				if !f.refused {
					if inStderr != 0 || inLog != 0 {
						t.Errorf("%s was labelled without trouble yet is named (stderr %d lines, log %d lines)\nstderr=%q\nlog=%q",
							f.dir, inStderr, inLog, res.Stderr, logged.String())
					}
					continue
				}
				refusals++
				if inStderr != 1 {
					t.Errorf("the caller must be told once about %s; stderr names it on %d lines: %q", f.dir, inStderr, res.Stderr)
				}
				if inLog != 1 {
					t.Errorf("the log must carry one line about %s; it has %d: %q", f.dir, inLog, logged.String())
				}
			}
			if got := strings.Count(res.Stderr, errRefused.Error()); got != refusals {
				t.Errorf("stderr should carry the cause once per refused folder (%d); it carries it %d times: %q", refusals, got, res.Stderr)
			}
			if !strings.HasPrefix(res.Stderr, notePrefix) {
				t.Errorf("the cage's notes lead stderr and start with %q; got %q", notePrefix, res.Stderr)
			}
			if !strings.Contains(res.Stderr, "could not be made writable") {
				t.Errorf("the note should say what the caller lost, a writable folder; got %q", res.Stderr)
			}
			t.Logf("the caller sees stderr: %q", res.Stderr)
			t.Logf("the log says: %q", logged.String())
		})
	}
}

// TestWindowsRevertFailureIsReportedAndTrue: a run that cannot put a folder's label back leaves
// that folder writable by every low-integrity process, which is a worse outcome than the lowering
// failing, and the caller must hear of it. The note has to be TRUE, so the test also checks the
// folder really is still low afterwards.
func TestWindowsRevertFailureIsReportedAndTrue(t *testing.T) {
	wt, scratch := labelFolders(t)
	logged := captureLog(t)
	swapLabelCalls(t, func(call int, dir string) error {
		if call == 2 && dir == wt {
			return errRefused
		}
		return nil
	}, nil)
	t.Cleanup(func() { removeLabel(wt) }) // the refused restore leaves the folder low on purpose; leave nothing behind

	res, err := runCmd(t, wt, scratch, "echo", "ran-anyway")
	if err != nil || res.Refused || !strings.Contains(res.Stdout, "ran-anyway") {
		t.Fatalf("the run itself is unchanged by a refused restore: err=%v refused=%v stdout=%q", err, res.Refused, res.Stdout)
	}

	after := labelSDDL(t, wt)
	if !strings.Contains(after, "(ML") || !strings.Contains(after, "LW") {
		t.Fatalf("precondition: the refused restore should have left the worktree low; its label is %q", after)
	}
	if linesWith(res.Stderr, wt) != 1 || !strings.Contains(res.Stderr, errRefused.Error()) || !strings.Contains(res.Stderr, "not restored") {
		t.Errorf("the caller must be told the worktree's label was not restored, and why; stderr=%q", res.Stderr)
	}
	if linesWith(logged.String(), wt) != 1 {
		t.Errorf("the log must carry one line about the worktree; got %q", logged.String())
	}
	if linesWith(res.Stderr, scratch) != 0 {
		t.Errorf("the scratch folder's label was restored and must not be named; stderr=%q", res.Stderr)
	}
}

// TestWindowsUnreadablePriorLabelIsReported: the cage restores a folder to the label it read before
// lowering it. When that label cannot be read, the old code took the failure for "no label" and the
// restore then removed whatever label the folder had. The run goes on as before (the folder is still
// lowered and cleared), and the caller is told that the folder is not restored to what it was.
func TestWindowsUnreadablePriorLabelIsReported(t *testing.T) {
	wt, scratch := labelFolders(t)
	logged := captureLog(t)
	swapLabelCalls(t, nil, func(dir string) error {
		if dir == wt {
			return errRefused
		}
		return nil
	})

	res, err := runCmd(t, wt, scratch, "echo", "ran-anyway")
	if err != nil || res.Refused || !strings.Contains(res.Stdout, "ran-anyway") {
		t.Fatalf("the run itself is unchanged by an unreadable prior label: err=%v refused=%v stdout=%q", err, res.Refused, res.Stdout)
	}
	if linesWith(res.Stderr, wt) != 1 || !strings.Contains(res.Stderr, errRefused.Error()) || !strings.Contains(res.Stderr, "could not be read") {
		t.Errorf("the caller must be told the worktree's prior label could not be read, and why; stderr=%q", res.Stderr)
	}
	if linesWith(logged.String(), wt) != 1 {
		t.Errorf("the log must carry one line about the worktree; got %q", logged.String())
	}
	// The note says the label is removed instead of restored; it must be true.
	if after := labelSDDL(t, wt); strings.Contains(after, "(ML") && strings.Contains(after, "LW") {
		t.Errorf("the note promises the run's low label is removed after the run, yet the worktree is still low: %q", after)
	}
}

// TestWindowsCleanRunCarriesNoCageNotes is the control for the three above: when every label call
// goes through, the caller's stderr is exactly the command's own (none, for echo) and the log is
// silent, so a note means something.
func TestWindowsCleanRunCarriesNoCageNotes(t *testing.T) {
	wt, scratch := labelFolders(t)
	logged := captureLog(t)

	res, err := runCmd(t, wt, scratch, "echo", "all-quiet")
	if err != nil || res.Refused || !strings.Contains(res.Stdout, "all-quiet") {
		t.Fatalf("clean run failed: err=%v refused=%v stdout=%q stderr=%q", err, res.Refused, res.Stdout, res.Stderr)
	}
	if res.Stderr != "" {
		t.Errorf("a run whose labels all went through must add nothing to stderr; got %q", res.Stderr)
	}
	if logged.Len() != 0 {
		t.Errorf("a run whose labels all went through must log nothing; got %q", logged.String())
	}
}

// TestWindowsLabelNotesSurviveACommandThatCannotStart: when Run fails before the command runs there
// is no Result worth reading, and the caller looks at the error. A note recorded before that
// failure must reach the error too, or the failure it explains is lost.
func TestWindowsLabelNotesSurviveACommandThatCannotStart(t *testing.T) {
	wt, scratch := labelFolders(t)
	captureLog(t)
	swapLabelCalls(t, func(call int, dir string) error {
		if call == 1 && dir == wt {
			return errRefused
		}
		return nil
	}, nil)

	// Allowlisted by base name, absent on disk: the allowlist passes, the labels are lowered, and
	// CreateProcessAsUser then fails.
	missing := filepath.Join(t.TempDir(), "cmd.exe")
	res, err := Run(context.Background(), Spec{
		Argv:               []string{missing, "/c", "echo", "x"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            scratch,
		AllowedExecutables: []string{"cmd"},
	})
	if err == nil {
		t.Fatalf("a command that does not exist must be an error; got result %+v", res)
	}
	if res.Refused {
		t.Fatalf("a missing executable is not an allowlist refusal: %+v", res)
	}
	if !strings.Contains(err.Error(), errRefused.Error()) || linesWith(err.Error(), wt) == 0 {
		t.Errorf("the error of a run that never started must carry the label note; got %q", err.Error())
	}
	if !strings.Contains(res.Stderr, errRefused.Error()) {
		t.Errorf("Result.Stderr should carry the note as well; got %q", res.Stderr)
	}
}

// TestWindowsLabelNotesPrecedeTheChildsOwnStderr: the child's stderr is not replaced, reordered or
// cut by the notes; the notes come first, the child's text follows untouched.
func TestWindowsLabelNotesPrecedeTheChildsOwnStderr(t *testing.T) {
	wt, scratch := labelFolders(t)
	captureLog(t)
	swapLabelCalls(t, func(call int, dir string) error {
		if call == 1 && dir == wt {
			return errRefused
		}
		return nil
	}, nil)

	res, err := runCmd(t, wt, scratch, "echo", "child-complaint", "1>&2")
	if err != nil || res.Refused {
		t.Fatalf("run failed: err=%v refused=%v stderr=%q", err, res.Refused, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("the probe writes to stderr only; stdout=%q", res.Stdout)
	}
	note, child := strings.Index(res.Stderr, errRefused.Error()), strings.Index(res.Stderr, "child-complaint")
	if note < 0 || child < 0 || note > child {
		t.Errorf("want the cage's note first and the child's own stderr after it; note at %d, child at %d in %q", note, child, res.Stderr)
	}
}

// TestWindowsKillFailureAtTheDeadlineIsReported: the caller's deadline is enforced by ending the
// command's job. When that call fails the cage can only wait for the command to finish by itself,
// so the deadline is not in force, and the old code said nothing (and, if the command did finish,
// claimed it had killed it). The test ends the job for real behind the refused call, so it does not
// wait out the command, and asserts what the caller is told: the time limit was not enforced, and
// no claim of a kill the cage could not confirm.
func TestWindowsKillFailureAtTheDeadlineIsReported(t *testing.T) {
	wt, scratch := labelFolders(t)
	logged := captureLog(t)
	realKill := terminateJob
	t.Cleanup(func() { terminateJob = realKill })
	terminateJob = func(job *proctree.Job, exitCode uint32) error {
		_ = realKill(job, exitCode)
		return errRefused
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := Run(ctx, Spec{
		Argv:               []string{cmdExe(t), "/c", "ping", "-n", "30", "127.0.0.1"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            scratch,
		AllowedExecutables: []string{"cmd"},
	})
	if err != nil || res.Refused {
		t.Fatalf("a failed kill does not turn the run into an error or a refusal: err=%v refused=%v stderr=%q", err, res.Refused, res.Stderr)
	}
	if !strings.Contains(res.Stderr, errRefused.Error()) || !strings.Contains(res.Stderr, "time limit") {
		t.Errorf("the caller must be told the time limit was not enforced, and why; stderr=%q", res.Stderr)
	}
	if strings.Contains(res.Stderr, "killed after context timeout") {
		t.Errorf("the cage must not claim a kill it could not confirm; stderr=%q", res.Stderr)
	}
	if got := strings.Count(logged.String(), errRefused.Error()); got != 1 {
		t.Errorf("the log must carry the failure once; it carries it %d times: %q", got, logged.String())
	}
}
