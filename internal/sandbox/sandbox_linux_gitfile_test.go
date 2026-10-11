//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// In a LINKED worktree, which is how the harness's own worktrees are made (git worktree add), .git
// is not a directory but a FILE: "gitdir: <repo>/.git/worktrees/<name>". The cage's write roots do not
// reach that git directory, so a command cannot change the repository itself. What it could change
// is the pointer: rewrite the file to name a git directory of its own (a directory in the worktree,
// with hooks and a config that run programs) and the next git command run on the HOST in that worktree
// follows it. These tests build the real thing, a linked worktree made by git, and run the real cage
// over it.

// requireGit returns the absolute path of git, or skips: the fixture is made by git itself.
func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not installed: the linked-worktree fixture is made by git itself (%v)", err)
	}
	return p
}

// linkedWorktree makes a repository with one commit and a linked worktree of it under base, and
// returns the worktree and the repository's git directory (outside the worktree). Every git call runs
// with a private HOME and no system config, so what the host's own git configuration says changes
// nothing. The premise of every test that uses it is checked here: <worktree>/.git is a file.
func linkedWorktree(t *testing.T, base string) (wt, gitDir string) {
	t.Helper()
	git := requireGit(t)
	repo, home := filepath.Join(base, "repo"), filepath.Join(base, "home")
	wt = filepath.Join(base, "linked")
	for _, d := range []string{repo, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=cage test", "GIT_AUTHOR_EMAIL=cage-test@example.invalid",
		"GIT_COMMITTER_NAME=cage test", "GIT_COMMITTER_EMAIL=cage-test@example.invalid",
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(repo, "init", "-q")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial")
	run(repo, "worktree", "add", "-q", "--detach", wt)
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the fixture's premise failed: %s/.git should be a regular file (err=%v, info=%v)", wt, err, fi)
	}
	return wt, filepath.Join(repo, ".git")
}

// dotGitPlace is somewhere a test builds its linked worktree: base returns a fresh directory there.
type dotGitPlace struct {
	name string
	base func(t *testing.T) string
}

// dotGitPlaces are the places the tests build their linked worktree: the test's temp directory and,
// where the host has one, a shared-memory mount. That mount is nosuid and nodev nearly everywhere,
// and the kernel LOCKS flags like those on every mount a user namespace inherits, so a read-only
// remount that drops one is refused. A worktree there is the case in which a mask built without
// that care fails, and it fails on a host whose temp directory has none of those flags.
func dotGitPlaces() []dotGitPlace {
	return []dotGitPlace{
		{"temp dir", func(t *testing.T) string { return t.TempDir() }},
		{"shared-memory mount", func(t *testing.T) string {
			if fi, err := os.Stat("/dev/shm"); err != nil || !fi.IsDir() {
				t.Skip("no /dev/shm on this host")
			}
			base, err := os.MkdirTemp("/dev/shm", "cage-dotgit-")
			if err != nil {
				t.Skipf("/dev/shm is not writable here: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			var st unix.Statfs_t
			if err := unix.Statfs(base, &st); err == nil {
				t.Logf("%s mount flags (statfs): %#x", base, st.Flags)
			}
			return base
		}},
	}
}

// cageRun runs argv in the cage over a WRITABLE worktree and returns what it printed. The cage
// refusing to start is a failure of the test, not a result.
func cageRun(t *testing.T, wt, scratch string, readDirs []string, argv ...string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, _ := Run(ctx, Spec{
		Argv:             argv,
		Worktree:         wt,
		WorktreeWritable: true,
		Scratch:          scratch,
		ReadDirs:         readDirs,
		ReadFiles:        []string{selfExe(t)},
		ABIFloor:         1,
	})
	if res.Refused {
		t.Fatalf("cage refused unexpectedly: %q", res.Stderr)
	}
	return res
}

// tamperAttempts are the attempts tamper makes, in its order. Each is a way to change what git finds
// at the entry without any right the cage is meant to deny: a worktree it may write.
var tamperAttempts = []string{"rewrite", "append", "truncate", "link-then-write", "rename-over", "rename-away", "remove"}

// tamper runs inside the cage (the "tamper" probe). It prints one line per attempt, "<name> DENIED:
// <error>" or "<name> OK", and leaves in place whatever it managed to change.
func tamper(path string) {
	dir := filepath.Dir(path)
	evil := "gitdir: " + filepath.Join(dir, "evil-gitdir") + "\n"
	say := func(name string, err error) {
		if err != nil {
			fmt.Printf("%s DENIED: %v\n", name, err)
			return
		}
		fmt.Printf("%s OK\n", name)
	}
	say("rewrite", os.WriteFile(path, []byte(evil), 0o644))
	say("append", func() error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("# appended\n")
		return err
	}())
	say("truncate", os.Truncate(path, 0))
	// A second name for the same file, written through: the read-only flag is the mount's, not the
	// file's, so a name that bypasses the mount would bypass the mask.
	say("link-then-write", func() error {
		alias := filepath.Join(dir, "alias-git")
		if err := os.Link(path, alias); err != nil {
			return err
		}
		return os.WriteFile(alias, []byte(evil), 0o644)
	}())
	alt := filepath.Join(dir, "alt-git")
	if err := os.WriteFile(alt, []byte(evil), 0o644); err != nil {
		fmt.Printf("setup FAILED: %v\n", err)
		return
	}
	say("rename-over", os.Rename(alt, path))
	say("rename-away", os.Rename(path, filepath.Join(dir, "moved-git")))
	say("remove", os.Remove(path))
}

// tamperResult reads the probe's lines into name -> denied. A name the probe did not print is absent.
func tamperResult(stdout string) map[string]bool {
	denied := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			denied[f[0]] = f[1] == "DENIED:"
		}
	}
	return denied
}

// hostRefusesTheCagesMounts is the control for a mask of one shape against a mask of another: it runs
// the cage over a worktree whose .git is a DIRECTORY, the mask this cage has always made, and
// reports whether the cage says that mask was refused, with the note it printed. A host that refuses
// it (the GitHub Actions runner does; so does any host whose policy strips an unprivileged namespace
// of its capabilities) refuses every mount the cage makes, and what it does with the .git file says
// nothing about this code.
func hostRefusesTheCagesMounts(t *testing.T) (refused bool, note string) {
	t.Helper()
	wt := dotGitWorktree(t)
	res := cageRun(t, wt, filepath.Join(wt, ".scratch"), nil,
		selfExe(t), "__probe__", "write", filepath.Join(wt, ".git", "hooks", "post-checkout"))
	return strings.Contains(res.Stderr, "NOT masked"), strings.TrimSpace(res.Stderr)
}

// TestCageMasksTheDotGitFileOfALinkedWorktree is the proof that matters: with a writable linked
// worktree, a command in the cage cannot rewrite, truncate, append to, link, rename over, rename away
// or remove the .git file, so git on the host keeps opening the git directory the file names. It is
// the counterpart of TestCageMasksDotGit, which proves it for a .git directory.
//
// The mask is a mount made inside the worker's user and mount namespace, which a host can refuse.
// Where the cage says so, the test asks the directory mask (hostRefusesTheCagesMounts): if that is
// refused too, the refusal is the host's and the test skips; if the host mounts over a .git directory
// and still refuses the file, it is this code's and the test fails. TestCageSaysWhenTheDotGitFileIsNotMasked
// runs on every host and holds the cage to saying so.
func TestCageMasksTheDotGitFileOfALinkedWorktree(t *testing.T) {
	requireCage(t)
	for _, loc := range dotGitPlaces() {
		t.Run(loc.name, func(t *testing.T) {
			base := loc.base(t)
			wt, _ := linkedWorktree(t, base)
			dotGit := filepath.Join(wt, ".git")
			before, err := os.ReadFile(dotGit)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, _ := os.Lstat(dotGit)

			res := cageRun(t, wt, filepath.Join(base, "scratch"), nil, selfExe(t), "__probe__", "tamper", dotGit)

			if strings.Contains(res.Stderr, "NOT masked") {
				if refused, ctl := hostRefusesTheCagesMounts(t); refused {
					t.Skipf("this host refuses the cage's unprivileged mounts (the .git directory mask is refused too: %s); the .git file mask is verified on hosts where the mount works", ctl)
				}
				t.Fatalf("the host mounts a read-only tmpfs over a .git directory, yet the .git file mask was refused: %s", strings.TrimSpace(res.Stderr))
			}
			denied := tamperResult(res.Stdout)
			for _, name := range tamperAttempts {
				ok, reported := denied[name]
				switch {
				case !reported:
					t.Errorf("the probe did not report %q; stdout=%q stderr=%q", name, res.Stdout, res.Stderr)
				case !ok:
					t.Errorf("SECURITY: %q succeeded on a linked worktree's .git file inside the cage: git on the host could be pointed at a directory the command controls; stdout=%q stderr=%q",
						name, res.Stdout, res.Stderr)
				}
			}
			after, err := os.ReadFile(dotGit)
			afterInfo, _ := os.Lstat(dotGit)
			switch {
			case err != nil:
				t.Errorf("SECURITY: the .git file is gone or unreadable after the cage ran (%v)", err)
			case !bytes.Equal(before, after):
				t.Errorf("SECURITY: the cage left the .git file changed:\nbefore %q\nafter  %q", before, after)
			case beforeInfo == nil || afterInfo == nil || !os.SameFile(beforeInfo, afterInfo):
				t.Errorf("SECURITY: the .git entry is no longer the file it was (replaced by another one)")
			}
			if strings.Contains(res.Stderr, noteMarker) {
				t.Errorf("the mask is in place, so the cage has nothing to report; stderr=%q", res.Stderr)
			}
		})
	}
}

// TestCageSaysWhenTheDotGitFileIsNotMasked ties the cage's note to what the kernel did, whatever
// this host allows, the way TestCageSaysWhenDotGitIsNotMasked does for a directory: a write into the
// .git file of a linked worktree reaches the real file exactly when the cage says it is not masked.
// It runs on the GitHub Actions runner too, where the mount is refused and the cage must say so.
func TestCageSaysWhenTheDotGitFileIsNotMasked(t *testing.T) {
	requireCage(t)
	for _, loc := range dotGitPlaces() {
		t.Run(loc.name, func(t *testing.T) {
			base := loc.base(t)
			wt, _ := linkedWorktree(t, base)
			dotGit := filepath.Join(wt, ".git")
			before, err := os.ReadFile(dotGit)
			if err != nil {
				t.Fatal(err)
			}

			res := cageRun(t, wt, filepath.Join(base, "scratch"), nil, selfExe(t), "__probe__", "write", dotGit)

			after, _ := os.ReadFile(dotGit)
			reached := !bytes.Equal(before, after)
			said := strings.Contains(res.Stderr, "NOT masked")
			switch {
			case reached && !said:
				t.Errorf("SECURITY: a write into a linked worktree's .git file reached the real file and the cage said nothing; stderr=%q stdout=%q", res.Stderr, res.Stdout)
			case !reached && said:
				t.Errorf("the cage says the .git file is not masked, yet the write did not reach it. The note is wrong. stderr=%q stdout=%q", res.Stderr, res.Stdout)
			}
			if said && !strings.HasPrefix(res.Stderr, noteMarker) {
				t.Errorf("the note leads stderr and starts with %q; got %q", noteMarker, res.Stderr)
			}
			t.Logf("mask in place: %v (write reached the file: %v); stderr=%q", !reached, reached, res.Stderr)
		})
	}
}

// TestCageStillReadsALinkedWorktree: the mask costs a legitimate command nothing. The .git file
// reads inside the cage (with the mask in place wherever the host allows it), and git itself, run in
// the cage over the worktree, finds the repository through it and reports its status and history.
// The git directory lives outside the worktree and outside the cage's write roots (it is granted
// read-only here, as a caller that wants git to work there must), so git can read it and still
// cannot write it, with or without the mask.
func TestCageStillReadsALinkedWorktree(t *testing.T) {
	requireCage(t)
	git := requireGit(t)
	// git runs from a directory the cage grants for reading; a git elsewhere (a store of its own, say)
	// would need a grant this test does not make.
	granted := false
	for _, d := range defaultReadDirs {
		if strings.HasPrefix(git, d+"/") {
			granted = true
		}
	}
	if !granted {
		t.Skipf("git is at %s, outside the cage's read-only system directories", git)
	}
	for _, loc := range dotGitPlaces() {
		t.Run(loc.name, func(t *testing.T) {
			base := loc.base(t)
			wt, gitDir := linkedWorktree(t, base)
			if err := os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			scratch := filepath.Join(base, "scratch")

			// The file itself reads, whole.
			want, _ := os.ReadFile(filepath.Join(wt, ".git"))
			read := cageRun(t, wt, scratch, nil, selfExe(t), "__probe__", "read", filepath.Join(wt, ".git"))
			if !strings.Contains(read.Stdout, "read OK") || !strings.Contains(read.Stdout, strings.TrimSpace(string(want))) {
				t.Errorf("the .git file must read inside the cage; got %q (stderr=%q)", read.Stdout, read.Stderr)
			}

			status := cageRun(t, wt, scratch, []string{gitDir}, git, "status", "--porcelain")
			if status.ExitCode != 0 || !strings.Contains(status.Stdout, "?? untracked.txt") {
				t.Errorf("git status must work in the cage over a masked linked worktree; exit=%d stdout=%q stderr=%q", status.ExitCode, status.Stdout, status.Stderr)
			}
			logged := cageRun(t, wt, scratch, []string{gitDir}, git, "log", "--format=%s", "-1")
			if logged.ExitCode != 0 || strings.TrimSpace(logged.Stdout) != "initial" {
				t.Errorf("git log must work in the cage over a masked linked worktree; exit=%d stdout=%q stderr=%q", logged.ExitCode, logged.Stdout, logged.Stderr)
			}
		})
	}
}
