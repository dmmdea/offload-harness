package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every fixture here spells out its line endings; none depends on how the checkout stores a file.

const (
	topLF  = "# Changelog\n\nAll notable changes are documented in this file.\n\n"
	tailLF = "## [0.185.1] - 2026-10-13 - the old title\n\n### Fixed - the old thing\n\n- The old bullet.\n\n" +
		"## [0.185.0] - 2026-10-12 - the older title\n\n- The older bullet, with no section of its own.\n"

	fragFirst  = "### Added - first thing\n\n- First bullet.\n"
	fragSecond = "### Fixed - second thing\n\n- Second bullet.\n"
	legacyLF   = "### Changed - a legacy entry\n\n- Legacy bullet, written straight into the block.\n"

	blankHead = topLF + "## [Unreleased]\n\n"
	deletes   = "Deletes: changes/unreleased/a-first.md, changes/unreleased/b-second.md"
)

func baseLF() string { return blankHead + tailLF }

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func releasedLF(version, date, title string) string {
	return blankHead + "## [" + version + "] - " + date + " - " + title + "\n\n" +
		fragFirst + "\n" + fragSecond + "\n" + tailLF
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// newRoot makes a repository root with a VERSION, a CHANGELOG.md and the two fragments the tests release.
func newRoot(t *testing.T, version, changelog string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "VERSION"), version)
	if changelog != "" {
		writeFile(t, filepath.Join(root, "CHANGELOG.md"), changelog)
	}
	writeFile(t, filepath.Join(root, "changes", "unreleased", "b-second.md"), fragSecond)
	writeFile(t, filepath.Join(root, "changes", "unreleased", "a-first.md"), fragFirst)
	writeFile(t, filepath.Join(root, "changes", "README.md"), "# About this directory\n")
	writeFile(t, filepath.Join(root, "changes", "unreleased", "README.md"), "# Not a fragment\n")
	writeFile(t, filepath.Join(root, "tools", "comfyui", "CHANGELOG.md"), "# A vendored ledger\n\n## [Unreleased]\n\n- not ours\n")
	return root
}

// snapshot is every file and directory under root, with the file bytes: two equal snapshots mean nothing changed,
// and a temp file left behind is a difference.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(path)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fixedClock() time.Time { return time.Date(2031, 2, 3, 12, 0, 0, 0, time.Local) }

// quietOps is the real file operations with the retry waits skipped.
func quietOps() fileOps {
	ops := defaultOps()
	ops.sleep = func(time.Duration) {}
	return ops
}

func runIt(t *testing.T, ops fileOps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runWith(args, &out, &errb, fixedClock, ops)
	return code, out.String(), errb.String()
}

func assemble(t *testing.T, root string, extra ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"assemble", "-root", root, "-title", "the title", "-date", "2026-10-14"}, extra...)
	return runIt(t, quietOps(), args...)
}

func TestAssembleWritesTheChangelogDeletesFragmentsAndPrintsTheDeletesLine(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	code, stdout, stderr := assemble(t, root)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got, want := readFile(t, filepath.Join(root, "CHANGELOG.md")), releasedLF("0.186.0", "2026-10-14", "the title"); got != want {
		t.Fatalf("CHANGELOG.md\n got: %q\nwant: %q", got, want)
	}
	for _, name := range []string{"a-first.md", "b-second.md"} {
		if exists(filepath.Join(root, "changes", "unreleased", name)) {
			t.Errorf("%s was assembled and is still there", name)
		}
	}
	// What is not a fragment is not consumed, and neither is a ledger that is not ours.
	for _, keep := range []string{"changes/README.md", "changes/unreleased/README.md", "tools/comfyui/CHANGELOG.md", "VERSION"} {
		if !exists(filepath.Join(root, filepath.FromSlash(keep))) {
			t.Errorf("%s was removed", keep)
		}
	}
	if got := readFile(t, filepath.Join(root, "tools", "comfyui", "CHANGELOG.md")); !strings.Contains(got, "not ours") || strings.Contains(got, "0.186.0") {
		t.Errorf("the vendored ledger was edited: %q", got)
	}
	// One line, forward slashes, in file-name order, ready to paste into the release commit.
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "Deletes:") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || lines[0] != deletes {
		t.Errorf("Deletes lines = %q, want exactly [%q]\nstdout:\n%s", lines, deletes, stdout)
	}
	if stderr != "" {
		t.Errorf("stderr on success: %q", stderr)
	}
}

func TestAssembleKeepsACRLFChangelogCRLF(t *testing.T) {
	root := newRoot(t, "0.186.0\n", crlf(baseLF()))
	if code, _, stderr := assemble(t, root); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got := readFile(t, filepath.Join(root, "CHANGELOG.md"))
	if want := crlf(releasedLF("0.186.0", "2026-10-14", "the title")); got != want {
		t.Fatalf("CRLF CHANGELOG.md\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Error("a bare LF in a CRLF file")
	}
}

func TestAssembleAbsorbsLegacyUnreleasedText(t *testing.T) {
	// A branch written the old way left an entry under [Unreleased]: the release moves it, it never drops it.
	root := newRoot(t, "0.186.0\n", blankHead+legacyLF+"\n"+tailLF)
	if code, _, stderr := assemble(t, root); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := blankHead + "## [0.186.0] - 2026-10-14 - the title\n\n" + legacyLF + "\n" + fragFirst + "\n" + fragSecond + "\n" + tailLF
	if got := readFile(t, filepath.Join(root, "CHANGELOG.md")); got != want {
		t.Fatalf("CHANGELOG.md\n got: %q\nwant: %q", got, want)
	}
	if code, _, stderr := runIt(t, quietOps(), "check", "-root", root); code != 0 {
		t.Errorf("the assembled tree fails its own check (exit %d): %s", code, stderr)
	}
}

func TestAssembleRefusalsLeaveEverythingByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		extra []string
		want  string
	}{
		{"a bad fragment", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "changes", "unreleased", "c-bad.md"), "- a loose bullet\n")
		}, nil, "c-bad.md"},
		{"a forged release heading in a fragment", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "changes", "unreleased", "c-forged.md"), "### Added - x\n\n## [9.9.9] - 2026-01-01 - forged\n")
		}, nil, "c-forged.md"},
		{"a stray file", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "changes", "unreleased", "Notes.txt"), "stray")
		}, nil, "Notes.txt"},
		{"a version that already has a heading", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "VERSION"), "0.185.1\n")
		}, nil, "0.185.1"},
		{"no VERSION file", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "VERSION")); err != nil {
				t.Fatal(err)
			}
		}, nil, "VERSION"},
		{"a VERSION that is not a version", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "VERSION"), "banana\n")
		}, nil, "version"},
		{"a date that is not a date", nil, []string{"-date", "2026-13-40"}, "date"},
		{"a two-line title", nil, []string{"-title", "one\ntwo"}, "title"},
		{"a fragment already in the changelog", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "CHANGELOG.md"), blankHead+"## [0.185.2] - 2026-10-13 - x\n\n"+fragFirst+"\n"+tailLF)
		}, nil, "already in CHANGELOG.md"},
		{"nothing to release", func(t *testing.T, root string) {
			for _, n := range []string{"a-first.md", "b-second.md"} {
				if err := os.Remove(filepath.Join(root, "changes", "unreleased", n)); err != nil {
					t.Fatal(err)
				}
			}
		}, nil, "nothing to release"},
		{"two [Unreleased] headings", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "CHANGELOG.md"), blankHead+"## [Unreleased]\n\n"+tailLF)
		}, nil, "[Unreleased]"},
		{"no CHANGELOG.md", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "CHANGELOG.md")); err != nil {
				t.Fatal(err)
			}
		}, nil, "CHANGELOG.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRoot(t, "0.186.0\n", baseLF())
			if tc.setup != nil {
				tc.setup(t, root)
			}
			before := snapshot(t, root)
			code, stdout, stderr := assemble(t, root, tc.extra...)
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name %q:\n%s", tc.want, stderr)
			}
			if strings.Contains(stdout, "Deletes:") {
				t.Errorf("a refused run printed a Deletes line:\n%s", stdout)
			}
			if after := snapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Errorf("a refused run changed the tree\nbefore: %v\n after: %v", before, after)
			}
		})
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	before := snapshot(t, root)
	code, stdout, stderr := assemble(t, root, "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if after := snapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Errorf("-dry-run changed the tree")
	}
	for _, want := range []string{"## [0.186.0] - 2026-10-14 - the title", "First bullet.", "Second bullet.", deletes} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run does not show %q:\n%s", want, stdout)
		}
	}
	// A dry run of a refusal is still a refusal.
	writeFile(t, filepath.Join(root, "VERSION"), "0.185.1\n")
	if code, _, _ := assemble(t, root, "-dry-run"); code != 1 {
		t.Errorf("a dry run of a duplicate version exits %d, want 1", code)
	}
}

func TestTheVersionComesFromTheVersionFile(t *testing.T) {
	// A VERSION file with CRLF and no -version flag: bump first, assemble last.
	root := newRoot(t, "0.190.4\r\n", baseLF())
	if code, _, stderr := assemble(t, root); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := readFile(t, filepath.Join(root, "CHANGELOG.md")); !strings.Contains(got, "## [0.190.4] - 2026-10-14 - the title\n") {
		t.Errorf("the heading does not carry the VERSION file's version:\n%s", got)
	}
	if code, _, _ := runIt(t, quietOps(), "assemble", "-root", root, "-title", "t", "-version", "0.9.9"); code != 2 {
		t.Errorf("a -version flag exits %d, want 2: the version is the VERSION file's", code)
	}
}

func TestTheDateDefaultsToTheInjectedClockAndAFlagOverridesIt(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	if code, _, stderr := runIt(t, quietOps(), "assemble", "-root", root, "-title", "the title"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := readFile(t, filepath.Join(root, "CHANGELOG.md")); !strings.Contains(got, "## [0.186.0] - 2031-02-03 - the title\n") {
		t.Errorf("the default date is not the clock's (2031-02-03):\n%s", got)
	}
	root2 := newRoot(t, "0.186.0\n", baseLF())
	if code, _, stderr := assemble(t, root2); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := readFile(t, filepath.Join(root2, "CHANGELOG.md")); !strings.Contains(got, "## [0.186.0] - 2026-10-14 - the title\n") {
		t.Errorf("-date is not used:\n%s", got)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	before := snapshot(t, root)
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"assemble", "-root", root},
		{"assemble", "-root", root, "-title", ""},
		{"assemble", "-root", root, "-title"},
		{"assemble", "-root", root, "-title", "t", "extra"},
		{"assemble", "-root", root, "-title", "t", "-bogus"},
		{"check", "-root", root, "-bogus"},
		{"check", "-root", root, "extra"},
	} {
		code, stdout, stderr := runIt(t, quietOps(), args...)
		if code != 2 {
			t.Errorf("%q exits %d, want 2 (stdout %q)", args, code, stdout)
		}
		if !strings.Contains(stderr, "usage:") {
			t.Errorf("%q: stderr does not carry the usage line: %q", args, stderr)
		}
	}
	if after := snapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Error("a usage error changed the tree")
	}
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		if code, stdout, _ := runIt(t, quietOps(), args...); code != 0 || !strings.Contains(stdout, "usage:") {
			t.Errorf("%q: exit %d, stdout %q; want 0 and the usage", args, code, stdout)
		}
	}
}

func TestCheckExitCodes(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	code, stdout, stderr := runIt(t, quietOps(), "check", "-root", root)
	if code != 0 || !strings.Contains(stdout, "ok") {
		t.Errorf("a clean tree: exit %d, stdout %q, stderr %q; want 0 and ok", code, stdout, stderr)
	}
	// The old habit: an entry written under [Unreleased].
	writeFile(t, filepath.Join(root, "CHANGELOG.md"), blankHead+legacyLF+"\n"+tailLF)
	code, _, stderr = runIt(t, quietOps(), "check", "-root", root)
	if code != 1 || !strings.Contains(stderr, "entries go in changes/unreleased/") {
		t.Errorf("legacy text under [Unreleased]: exit %d, stderr %q; want 1 and the instruction", code, stderr)
	}
	// A bad fragment is named.
	writeFile(t, filepath.Join(root, "CHANGELOG.md"), baseLF())
	writeFile(t, filepath.Join(root, "changes", "unreleased", "c-bad.md"), "no heading\n")
	code, _, stderr = runIt(t, quietOps(), "check", "-root", root)
	if code != 1 || !strings.Contains(stderr, "c-bad.md") {
		t.Errorf("a bad fragment: exit %d, stderr %q; want 1 naming it", code, stderr)
	}
}

func TestARemoveFailureIsReportedAndNamesTheFilesStillToDelete(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	var attempts, sleeps atomic.Int32
	ops := quietOps()
	ops.sleep = func(time.Duration) { sleeps.Add(1) }
	realRemove := os.Remove
	ops.remove = func(path string) error {
		if strings.HasSuffix(filepath.ToSlash(path), "/b-second.md") {
			attempts.Add(1)
			return errors.New("the process cannot access the file because it is being used by another process")
		}
		return realRemove(path)
	}
	code, stdout, stderr := runIt(t, ops, "assemble", "-root", root, "-title", "the title", "-date", "2026-10-14")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "changes/unreleased/b-second.md") {
		t.Errorf("stderr does not name the file still to delete:\n%s", stderr)
	}
	if strings.Contains(stderr, "a-first.md") {
		t.Errorf("stderr names a file that was removed:\n%s", stderr)
	}
	if strings.Contains(stdout, "Deletes:") {
		t.Errorf("a run that left a fragment behind claims success with a Deletes line:\n%s", stdout)
	}
	if attempts.Load() < 2 || sleeps.Load() < 1 {
		t.Errorf("the remove was tried %d time(s) with %d wait(s): a Windows sharing violation must be retried", attempts.Load(), sleeps.Load())
	}
	if exists(filepath.Join(root, "changes", "unreleased", "a-first.md")) || !exists(filepath.Join(root, "changes", "unreleased", "b-second.md")) {
		t.Error("the fragment that could be removed was not, or the one that could not was")
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "CHANGELOG.md")), "## [0.186.0] - 2026-10-14 - the title") {
		t.Error("the changelog was not written")
	}
	// The survivor is what the duplicate guard exists for: its text is in the changelog now.
	code, _, stderr = runIt(t, quietOps(), "check", "-root", root)
	if code != 1 || !strings.Contains(stderr, "already in CHANGELOG.md") || !strings.Contains(stderr, "b-second.md") {
		t.Errorf("check after a failed remove: exit %d, stderr %q; want 1 naming b-second.md as already in CHANGELOG.md", code, stderr)
	}
	if err := os.Remove(filepath.Join(root, "changes", "unreleased", "b-second.md")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runIt(t, quietOps(), "check", "-root", root); code != 0 {
		t.Errorf("check after the hand delete: exit %d: %s", code, stderr)
	}
}

func TestARenameThatFailsOnceIsRetried(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	var calls, sleeps atomic.Int32
	ops := quietOps()
	ops.sleep = func(time.Duration) { sleeps.Add(1) }
	ops.rename = func(oldpath, newpath string) error {
		if calls.Add(1) <= 2 {
			return errors.New("access is denied")
		}
		return os.Rename(oldpath, newpath)
	}
	code, _, stderr := runIt(t, ops, "assemble", "-root", root, "-title", "the title", "-date", "2026-10-14")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if calls.Load() != 3 || sleeps.Load() != 2 {
		t.Errorf("rename tried %d time(s) with %d wait(s), want 3 and 2", calls.Load(), sleeps.Load())
	}
	if got, want := readFile(t, filepath.Join(root, "CHANGELOG.md")), releasedLF("0.186.0", "2026-10-14", "the title"); got != want {
		t.Errorf("CHANGELOG.md after a retried rename\n got: %q\nwant: %q", got, want)
	}
}

func TestARenameThatNeverSucceedsLeavesEverythingAsItWas(t *testing.T) {
	root := newRoot(t, "0.186.0\n", baseLF())
	before := snapshot(t, root)
	ops := quietOps()
	ops.rename = func(string, string) error { return errors.New("access is denied") }
	code, stdout, stderr := runIt(t, ops, "assemble", "-root", root, "-title", "the title", "-date", "2026-10-14")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "CHANGELOG.md") {
		t.Errorf("stderr does not name the file that could not be written: %s", stderr)
	}
	// Byte-identical, and no temp file left in the repository root: the fragments were not touched either.
	if after := snapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Errorf("a failed write changed the tree\nbefore: %v\n after: %v", before, after)
	}
}
