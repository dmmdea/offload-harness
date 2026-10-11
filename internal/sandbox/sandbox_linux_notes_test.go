//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The cage masks <worktree>/.git with a read-only tmpfs so a shell with a writable worktree cannot
// plant a git hook that fires on the host later. The mount can be refused (an unprivileged user
// namespace that may not mount: some containers and CI sandboxes), and the old code dropped the
// error: the shell ran with a writable .git and the caller was never told. The command still runs
// now (failing closed is a separate decision), and the caller is told.

// errMountRefused is the injected cause, spelled so that only a note carrying the mount's real
// error can match it.
var errMountRefused = errors.New("injected refusal of the mount")

func dotGitWorktree(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestMaskDotGitSaysWhenTheKernelRefusesTheMount: the note names the directory and the cause, and
// says what the caller lost.
func TestMaskDotGitSaysWhenTheKernelRefusesTheMount(t *testing.T) {
	wt := dotGitWorktree(t)
	var target, fstype string
	var flags uintptr
	calls := 0
	refuse := func(_, tgt, fst string, fl uintptr, _ string) error {
		calls++
		target, fstype, flags = tgt, fst, fl
		return errMountRefused
	}

	note := maskDotGit(Spec{Worktree: wt, WorktreeWritable: true}, refuse)

	gp := filepath.Join(wt, ".git")
	if calls != 1 || target != gp || fstype != "tmpfs" {
		t.Fatalf("want one tmpfs mount over %s; got %d calls, target %q, fstype %q", gp, calls, target, fstype)
	}
	if flags&unix.MS_RDONLY == 0 {
		t.Errorf("the mask must be read-only; flags=%#x", flags)
	}
	for _, want := range []string{gp, errMountRefused.Error(), "NOT masked", "can write into it"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note should contain %q; got %q", want, note)
		}
	}
}

// TestMaskDotGitStaysQuietWhenThereIsNothingToSay: no note when the mount works, and no mount (so
// no note) where there is nothing to mask or nothing writable to protect.
func TestMaskDotGitStaysQuietWhenThereIsNothingToSay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      func(wt string) Spec
		mountErr  error
		wantCalls int
	}{
		{"mount succeeds", func(wt string) Spec { return Spec{Worktree: wt, WorktreeWritable: true} }, nil, 1},
		{"read-only worktree", func(wt string) Spec { return Spec{Worktree: wt} }, errMountRefused, 0},
		{"no .git directory", func(wt string) Spec {
			return Spec{Worktree: filepath.Join(wt, "elsewhere"), WorktreeWritable: true}
		}, errMountRefused, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			mount := func(_, _, _ string, _ uintptr, _ string) error { calls++; return tc.mountErr }
			if note := maskDotGit(tc.spec(dotGitWorktree(t)), mount); note != "" {
				t.Errorf("nothing to report here; got %q", note)
			}
			if calls != tc.wantCalls {
				t.Errorf("mount called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestCageSaysWhenDotGitIsNotMasked runs the REAL cage and ties the note to what the kernel did,
// whatever this host allows: a write into <worktree>/.git reaches the real repository exactly when
// the cage says it is not masked. On a host where the mount works the file never appears and the
// stream is quiet; on one where the mount is refused the file appears and the cage said so. The
// failure this guards against is the file appearing with no word from the cage.
func TestCageSaysWhenDotGitIsNotMasked(t *testing.T) {
	requireCage(t)
	wt := dotGitWorktree(t)
	planted := filepath.Join(wt, ".git", "hooks", "post-checkout")
	res, _ := Run(context.Background(), Spec{
		Argv:             []string{selfExe(t), "__probe__", "write", planted},
		Worktree:         wt,
		Scratch:          filepath.Join(wt, ".scratch"),
		ReadFiles:        []string{selfExe(t)},
		WorktreeWritable: true,
		ABIFloor:         1,
	})
	if res.Refused {
		t.Fatalf("cage refused unexpectedly: %q", res.Stderr)
	}
	_, statErr := os.Stat(planted)
	reached := statErr == nil
	said := strings.Contains(res.Stderr, "NOT masked")
	switch {
	case reached && !said:
		t.Errorf("SECURITY: a write into <worktree>/.git reached the real repository and the cage said nothing; stderr=%q stdout=%q", res.Stderr, res.Stdout)
	case !reached && said:
		t.Errorf("the cage says .git is not masked, yet the write did not reach it (stat err=%v); the note is wrong. stderr=%q stdout=%q", statErr, res.Stderr, res.Stdout)
	}
	if said && !strings.HasPrefix(res.Stderr, noteMarker) {
		t.Errorf("the note leads stderr and starts with %q; got %q", noteMarker, res.Stderr)
	}
	t.Logf("mask in place: %v (write reached .git: %v); stderr=%q", !reached, reached, res.Stderr)
}
