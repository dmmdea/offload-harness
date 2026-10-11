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

// In a LINKED worktree .git is a FILE ("gitdir: <path>"), and the cage makes it read-only with two
// mounts: a bind of the file onto itself, then a read-only remount of that bind (a bind mount takes
// its flags only through a remount). The stand-ins below record the calls the way the tests above
// do; sandbox_linux_gitfile_test.go runs the real thing over a worktree made by git.

// dotGitFileWorktree is a worktree whose .git is a file, as git worktree add leaves it.
func dotGitFileWorktree(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nowhere/.git/worktrees/linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

type mountCall struct {
	source, target, fstype string
	flags                  uintptr
}

// TestMaskDotGitFileBindsItReadOnly: two mounts, in order, with the flags the kernel needs. The
// remount names every flag a user namespace locks on the mount (nosuid, nodev, noexec, read-only) and
// no atime flag, so the kernel keeps the mount's own atime mode, which is locked as well. A remount
// that drops a locked flag is refused, and the mount a worktree sits on often carries them.
func TestMaskDotGitFileBindsItReadOnly(t *testing.T) {
	wt := dotGitFileWorktree(t)
	var calls []mountCall
	record := func(source, target, fstype string, flags uintptr, _ string) error {
		calls = append(calls, mountCall{source, target, fstype, flags})
		return nil
	}

	if note := maskDotGit(Spec{Worktree: wt, WorktreeWritable: true}, record); note != "" {
		t.Errorf("both mounts went through, so there is nothing to report; got %q", note)
	}

	gp := filepath.Join(wt, ".git")
	if len(calls) != 2 {
		t.Fatalf("want two mounts (the bind, then the read-only remount); got %d: %+v", len(calls), calls)
	}
	bind, remount := calls[0], calls[1]
	if bind.source != gp || bind.target != gp {
		t.Errorf("the bind is of the file onto itself (%s); got %q onto %q", gp, bind.source, bind.target)
	}
	if bind.flags&unix.MS_BIND == 0 || bind.flags&(unix.MS_REMOUNT|unix.MS_RDONLY) != 0 {
		t.Errorf("the first call is a plain bind; flags=%#x", bind.flags)
	}
	if remount.target != gp {
		t.Errorf("the remount is of %s; got %q", gp, remount.target)
	}
	for name, bit := range map[string]uintptr{
		"MS_BIND": unix.MS_BIND, "MS_REMOUNT": unix.MS_REMOUNT, "MS_RDONLY": unix.MS_RDONLY,
		"MS_NOSUID": unix.MS_NOSUID, "MS_NODEV": unix.MS_NODEV, "MS_NOEXEC": unix.MS_NOEXEC,
	} {
		if remount.flags&bit == 0 {
			t.Errorf("the remount needs %s; flags=%#x", name, remount.flags)
		}
	}
	if atime := uintptr(unix.MS_NOATIME | unix.MS_NODIRATIME | unix.MS_RELATIME | unix.MS_STRICTATIME); remount.flags&atime != 0 {
		t.Errorf("the remount must name no atime flag, so the kernel keeps the mount's own; flags=%#x", remount.flags)
	}
}

// TestMaskDotGitFileSaysWhenTheKernelRefuses: either mount refused is a note that names the file, the
// step, the cause and what the caller lost, in the words the directory note uses. A refused bind is
// not followed by a remount.
func TestMaskDotGitFileSaysWhenTheKernelRefuses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refuse    int // the call (1-based) the kernel refuses
		wantCalls int
		wantStep  string
	}{
		{"bind", 1, 1, "bind mount"},
		{"read-only remount", 2, 2, "read-only remount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := dotGitFileWorktree(t)
			calls := 0
			mount := func(_, _, _ string, _ uintptr, _ string) error {
				calls++
				if calls == tc.refuse {
					return errMountRefused
				}
				return nil
			}

			note := maskDotGit(Spec{Worktree: wt, WorktreeWritable: true}, mount)

			gp := filepath.Join(wt, ".git")
			for _, want := range []string{gp, errMountRefused.Error(), "NOT masked", "can write into it", tc.wantStep} {
				if !strings.Contains(note, want) {
					t.Errorf("the note should contain %q; got %q", want, note)
				}
			}
			if calls != tc.wantCalls {
				t.Errorf("mount called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestMaskDotGitFileStaysQuietWhenThereIsNothingToSay: no mount, so no note, where there is nothing
// writable to protect, nothing to mask, or a .git that a mount cannot protect.
func TestMaskDotGitFileStaysQuietWhenThereIsNothingToSay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) Spec
	}{
		{"read-only worktree", func(t *testing.T) Spec { return Spec{Worktree: dotGitFileWorktree(t)} }},
		{"no .git", func(t *testing.T) Spec { return Spec{Worktree: t.TempDir(), WorktreeWritable: true} }},
		// A mount follows a symlink, so it would land on the link's target and leave the link, which sits
		// in the writable worktree, replaceable. Neither git worktree add nor git init makes one.
		{"a symlink named .git", func(t *testing.T) Spec {
			wt := t.TempDir()
			target := filepath.Join(t.TempDir(), "gitfile")
			if err := os.WriteFile(target, []byte("gitdir: /nowhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(wt, ".git")); err != nil {
				t.Skipf("cannot make a symlink here: %v", err)
			}
			return Spec{Worktree: wt, WorktreeWritable: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			refuse := func(_, _, _ string, _ uintptr, _ string) error { calls++; return errMountRefused }
			if note := maskDotGit(tc.build(t), refuse); note != "" {
				t.Errorf("nothing to report here; got %q", note)
			}
			if calls != 0 {
				t.Errorf("mount called %d times, want none", calls)
			}
		})
	}
}
