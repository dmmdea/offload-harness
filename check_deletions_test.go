package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeletionGuardRefusesAnUndeclaredDeletion drives scripts/check-deletions.sh
// (register H-51) over synthetic repositories.
//
// The gate it certifies is the one nothing in this repo had: `go build`, `go vet`
// and `go test` all read the tree they are handed, and a tree that is simply
// MISSING a merged feature passes all three. Pull request #324 merged a tree
// rebuilt on an older base -- its first parent was 0.123.1, its tree was not --
// and six files from two merged releases disappeared with every check green.
//
// The two shapes below are the two that matter, and the second is the incident:
// a merge commit whose tree drops what its own first parent carried.
func TestDeletionGuardRefusesAnUndeclaredDeletion(t *testing.T) {
	script := scriptPath(t)

	t.Run("a branch that deletes a file main has", func(t *testing.T) {
		repo := newSynthRepo(t)
		repo.write("keep.txt", "keep\n")
		repo.write("doomed.txt", "doomed\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "base")
		base := repo.rev("HEAD")

		repo.git("checkout", "-q", "-b", "feature")
		repo.rm("doomed.txt")
		repo.git("commit", "-am", "drop it")

		out, err := repo.run(script, base, "HEAD")
		if err == nil {
			t.Fatalf("an undeclared deletion passed the guard:\n%s", out)
		}
		if !strings.Contains(out, "doomed.txt") {
			t.Errorf("the failure does not name the deleted file:\n%s", out)
		}
	})

	t.Run("the same deletion, declared in the commit message", func(t *testing.T) {
		repo := newSynthRepo(t)
		repo.write("keep.txt", "keep\n")
		repo.write("doomed.txt", "doomed\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "base")
		base := repo.rev("HEAD")

		repo.git("checkout", "-q", "-b", "feature")
		repo.rm("doomed.txt")
		repo.git("commit", "-am", "drop it\n\nDeletes: doomed.txt")

		if out, err := repo.run(script, base, "HEAD"); err != nil {
			t.Fatalf("a declared deletion was refused:\n%s", out)
		}
	})

	t.Run("declared in the pull request body", func(t *testing.T) {
		repo := newSynthRepo(t)
		repo.write("a.txt", "a\n")
		repo.write("b.txt", "b\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "base")
		base := repo.rev("HEAD")

		repo.git("checkout", "-q", "-b", "feature")
		repo.rm("a.txt")
		repo.rm("b.txt")
		repo.git("commit", "-am", "drop both")

		repo.env = append(repo.env, "PR_BODY=Why this is fine.\n\n- Deletes: a.txt, b.txt\n")
		if out, err := repo.run(script, base, "HEAD"); err != nil {
			t.Fatalf("deletions declared in the PR body were refused:\n%s", out)
		}
	})

	// The #324 shape, exactly: a merge commit whose FIRST PARENT carries a file
	// its own tree does not. Nothing diffs a merge against its first parent, so
	// this is the call that has to exist.
	t.Run("a merge whose tree drops what its first parent carried", func(t *testing.T) {
		repo := newSynthRepo(t)
		repo.write("shared.txt", "shared\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "base")
		mergeBase := repo.rev("HEAD")

		// main moves on: a later release adds a file.
		repo.write("added-on-main.txt", "a merged release\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "0.123.1: a release that is already on main")
		main := repo.rev("HEAD")

		// the branch was replayed onto the OLD base and never saw it.
		repo.git("checkout", "-q", "-b", "replayed", mergeBase)
		repo.write("branch-work.txt", "the branch's own work\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "the composite tier")
		branch := repo.rev("HEAD")

		// the merge: first parent main, tree the branch's.
		tree := strings.TrimSpace(repo.out("rev-parse", "replayed^{tree}"))
		merge := strings.TrimSpace(repo.out("commit-tree", tree, "-p", main, "-p", branch, "-m", "Merge pull request #324"))

		out, err := repo.run(script, merge+"^1", merge)
		if err == nil {
			t.Fatalf("the merge that dropped a merged release passed the guard:\n%s", out)
		}
		if !strings.Contains(out, "added-on-main.txt") {
			t.Errorf("the failure does not name the file the merge lost:\n%s", out)
		}
		// And a clean merge of the same two commits must stay quiet, or the gate
		// is just noise on every merge commit.
		repo.git("checkout", "-q", "replayed")
		repo.git("merge", "-q", "--no-edit", main)
		if out, err := repo.run(script, "HEAD^1", "HEAD"); err != nil {
			t.Fatalf("an honest merge was refused:\n%s", out)
		}
	})

	t.Run("no deletions at all", func(t *testing.T) {
		repo := newSynthRepo(t)
		repo.write("a.txt", "a\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "base")
		base := repo.rev("HEAD")
		repo.write("b.txt", "b\n")
		repo.git("add", "-A")
		repo.git("commit", "-m", "add only")

		if out, err := repo.run(script, base, "HEAD"); err != nil {
			t.Fatalf("a branch that deletes nothing was refused:\n%s", out)
		}
	})
}

// scriptPath returns the guard's absolute path, skipping when this host has no
// bash. CI runs the build job on ubuntu, where it always runs.
func scriptPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH; the guard runs in CI's ubuntu build job")
	}
	abs, err := filepath.Abs(filepath.Join("scripts", "check-deletions.sh"))
	if err != nil {
		t.Fatalf("resolve the guard: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("the guard is missing: %v", err)
	}
	return abs
}

type synthRepo struct {
	t   *testing.T
	dir string
	env []string
}

func newSynthRepo(t *testing.T) *synthRepo {
	t.Helper()
	r := &synthRepo{t: t, dir: t.TempDir()}
	// A sandboxed identity and config: the guard must never read the developer's
	// own git config, and a test must never write to it.
	r.env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(r.dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(r.dir, "gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *synthRepo) write(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", name, err)
	}
}

func (r *synthRepo) rm(name string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
		r.t.Fatalf("remove %s: %v", name, err)
	}
}

func (r *synthRepo) git(args ...string) {
	r.t.Helper()
	if out, err := r.exec("git", args...); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *synthRepo) out(args ...string) string {
	r.t.Helper()
	out, err := r.exec("git", args...)
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (r *synthRepo) rev(ref string) string {
	r.t.Helper()
	return strings.TrimSpace(r.out("rev-parse", ref))
}

// run invokes the guard in the synthetic repo and returns its combined output.
func (r *synthRepo) run(script string, base, head string) (string, error) {
	r.t.Helper()
	return r.exec("bash", script, base, head)
}

func (r *synthRepo) exec(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
