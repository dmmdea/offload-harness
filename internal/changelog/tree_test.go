package changelog

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// treeAt writes a repository root: CHANGELOG.md (when text is not empty) and one file per fragment.
func treeAt(t *testing.T, text string, fragments map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if text != "" {
		writeFile(t, filepath.Join(root, "CHANGELOG.md"), text)
	}
	for name, body := range fragments {
		writeFile(t, filepath.Join(root, "changes", "unreleased", name), body)
	}
	return root
}

func twoFragmentFiles() map[string]string {
	return map[string]string{"a-first.md": fragFirst, "b-second.md": fragSecond}
}

func wantProblem(t *testing.T, problems []string, substrs ...string) {
	t.Helper()
	joined := strings.Join(problems, "\n")
	if len(problems) == 0 {
		t.Fatalf("no problem reported, want one naming %q", substrs)
	}
	for _, s := range substrs {
		if !strings.Contains(joined, s) {
			t.Errorf("problems do not name %q:\n%s", s, joined)
		}
	}
}

func TestCheckTreeAcceptsACleanTree(t *testing.T) {
	for name, text := range map[string]string{"LF": baseLF(), "CRLF": eolOf("\r\n", baseLF())} {
		t.Run(name, func(t *testing.T) {
			if p := CheckTree(treeAt(t, text, twoFragmentFiles())); len(p) != 0 {
				t.Errorf("a clean tree has problems: %v", p)
			}
		})
	}
}

func TestAnAbsentFragmentDirectoryIsACleanTree(t *testing.T) {
	// Nothing to release is not a violation of the tree rules: the directory is made by the first branch that needs it.
	if p := CheckTree(treeAt(t, baseLF(), nil)); len(p) != 0 {
		t.Errorf("a tree with no changes/unreleased directory has problems: %v", p)
	}
}

func TestCheckTreeNeedsTheChangelog(t *testing.T) {
	wantProblem(t, CheckTree(treeAt(t, "", twoFragmentFiles())), "CHANGELOG.md")
}

func TestCheckTreeNeedsExactlyOneUnreleasedHeading(t *testing.T) {
	wantProblem(t, CheckTree(treeAt(t, topLF+tailLF, nil)), "[Unreleased]", "found 0")
	wantProblem(t, CheckTree(treeAt(t, topLF+"## [Unreleased]\n\n## [Unreleased]\n\n"+tailLF, nil)), "[Unreleased]", "found 2")
}

func TestTheUnreleasedBlockMustBeBlank(t *testing.T) {
	root := treeAt(t, legacyBase(), nil)
	wantProblem(t, CheckTree(root), "CHANGELOG.md", "entries go in changes/unreleased/")

	// Blank lines are not an entry.
	if p := CheckTree(treeAt(t, topLF+"## [Unreleased]\n\n   \n\n\n"+tailLF, nil)); len(p) != 0 {
		t.Errorf("a block of blank lines is refused: %v", p)
	}
	// A leftover under [Unreleased] is the old habit coming back, CRLF or not.
	wantProblem(t, CheckTree(treeAt(t, eolOf("\r\n", legacyBase()), nil)), "entries go in changes/unreleased/")
}

func TestCheckForReleaseAllowsLegacyTextButNothingElse(t *testing.T) {
	root := treeAt(t, legacyBase(), map[string]string{"a-first.md": fragFirst, "bad.md": "- loose bullet\n"})
	p := CheckForRelease(root)
	wantProblem(t, p, "changes/unreleased/bad.md")
	if joined := strings.Join(p, "\n"); strings.Contains(joined, "entries go in") {
		t.Errorf("CheckForRelease refuses the legacy text the release exists to absorb:\n%s", joined)
	}
	if p := CheckForRelease(treeAt(t, legacyBase(), twoFragmentFiles())); len(p) != 0 {
		t.Errorf("legacy text and two good fragments are a releasable tree: %v", p)
	}
	// And the rules after the block are still the release's: a duplicate heading is refused.
	dup := topLF + "## [Unreleased]\n\n" + tailLF + "\n" + tailLF
	wantProblem(t, CheckForRelease(treeAt(t, dup, nil)), "appears 2 times")
}

func TestCheckTreeNamesEveryBadFragmentAndStrayFile(t *testing.T) {
	root := treeAt(t, baseLF(), map[string]string{
		"a-first.md": fragFirst,
		"bad.md":     "### Added - x\n\n## [9.9.9] - forged\n",
		"Notes.txt":  "stray",
		"empty.md":   "",
	})
	p := CheckTree(root)
	wantProblem(t, p, "changes/unreleased/bad.md", "changes/unreleased/Notes.txt", "changes/unreleased/empty.md")
	if joined := strings.Join(p, "\n"); strings.Contains(joined, "a-first.md") {
		t.Errorf("the good fragment is named:\n%s", joined)
	}
}

func TestAFragmentAlreadyInTheChangelogIsRefused(t *testing.T) {
	// The duplicate a failed delete leaves behind: the text was assembled, the file is still there.
	released := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" + fragFirst + "\n" + tailLF
	for name, text := range map[string]string{"LF": released, "CRLF": eolOf("\r\n", released)} {
		t.Run(name, func(t *testing.T) {
			p := CheckTree(treeAt(t, text, map[string]string{"a-first.md": fragFirst}))
			wantProblem(t, p, "changes/unreleased/a-first.md", "already in CHANGELOG.md")
		})
	}
	t.Run("a CRLF fragment with trailing blank lines is the same text", func(t *testing.T) {
		body := eolOf("\r\n", fragFirst) + "\r\n\r\n"
		wantProblem(t, CheckTree(treeAt(t, released, map[string]string{"a-first.md": body})), "already in CHANGELOG.md")
	})
	t.Run("a fragment that shares only its heading is a different entry", func(t *testing.T) {
		other := "### Added - first thing\n\n- A different bullet.\n"
		if p := CheckTree(treeAt(t, released, map[string]string{"other.md": other})); len(p) != 0 {
			t.Errorf("a fragment that merely starts like a released entry is refused: %v", p)
		}
	})
	t.Run("the release path refuses it too", func(t *testing.T) {
		wantProblem(t, CheckForRelease(treeAt(t, released, map[string]string{"a-first.md": fragFirst})), "already in CHANGELOG.md")
	})
}

func TestEveryReleaseHeadingMustAppearOnce(t *testing.T) {
	dup := topLF + "## [Unreleased]\n\n" + tailLF + "\n## [0.185.0] - 2026-01-01 - pasted again\n\n- x\n"
	wantProblem(t, CheckTree(treeAt(t, dup, nil)), "0.185.0", "appears 2 times")

	// Near misses are other releases, and the interim heading is not a semver release at all.
	fine := topLF + "## [Unreleased]\n\n## [0.185.10] - 2026-10-14 - x\n\n- x\n\n" + tailLF +
		"\n## [0.44.x interim] - 2026-08-10\n\n- x\n\n## [0.44.x interim] - 2026-08-11\n\n- y\n"
	if p := CheckTree(treeAt(t, fine, nil)); len(p) != 0 {
		t.Errorf("0.185.10 beside 0.185.1, and a repeated interim heading, are fine: %v", p)
	}
}

func TestNoLineButAHeadingCarriesAReleaseDate(t *testing.T) {
	// The shape of the 0.183.0 incident: a heading's text glued onto the end of another line.
	const gluedLine = "- An entry that swallowed ## [0.183.0] - 2026-10-12 - a heading."
	glued := baseLF() + "\n" + gluedLine + "\n"
	line := strings.Count(glued[:strings.Index(glued, gluedLine)], "\n") + 1
	wantProblem(t, CheckTree(treeAt(t, glued, nil)), "CHANGELOG.md line "+strconv.Itoa(line), "not a heading")

	emDash := topLF + "## [Unreleased]\n\n" + tailLF + "\nA stray line \u2014 2026-01-01 \u2014 with the older separator.\n"
	wantProblem(t, CheckTree(treeAt(t, emDash, nil)), "not a heading")

	// Both separators are legal in a heading (the em dash is what the file used before 0.113.39), and a date in prose is fine.
	fine := topLF + "## [Unreleased]\n\n" + tailLF + "\n## [0.113.38] \u2014 2026-09-01 \u2014 an older heading\n\n- Shipped on 2026-09-01 - and on 2026-09-02.\n"
	if p := CheckTree(treeAt(t, fine, nil)); len(p) != 0 {
		t.Errorf("headings with either separator, and dates in prose, are fine: %v", p)
	}
}

func TestOnlyTheRootChangelogIsRead(t *testing.T) {
	root := treeAt(t, baseLF(), nil)
	// The vendored printed-CLI ledgers carry their own headings and are never ours to police or to edit.
	writeFile(t, filepath.Join(root, "tools", "comfyui", "CHANGELOG.md"), tailLF+tailLF+"## [Unreleased]\n- x\n")
	if p := CheckTree(root); len(p) != 0 {
		t.Errorf("a nested CHANGELOG.md was read: %v", p)
	}
}

// ---- Combined

func TestCombinedIncludesFragmentText(t *testing.T) {
	root := treeAt(t, baseLF(), twoFragmentFiles())
	got, err := Combined(root)
	if err != nil {
		t.Fatalf("Combined: %v", err)
	}
	want := mustSplice(t, baseLF(), twoFragments(), nil)
	if got != want {
		t.Fatalf("Combined\n got: %q\nwant: %q", got, want)
	}
	// The fragment text is above the first release heading, in the [Unreleased] block: it is the newest text.
	if i, j := strings.Index(got, "- First bullet."), strings.Index(got, "## [0.185.1]"); i < 0 || j < 0 || i > j {
		t.Errorf("the fragment text is not above the top release heading (at %d, heading at %d)", i, j)
	}
}

func TestCombinedWithoutFragmentsIsTheChangelogItself(t *testing.T) {
	for name, text := range map[string]string{"LF": baseLF(), "CRLF": eolOf("\r\n", baseLF())} {
		t.Run(name, func(t *testing.T) {
			got, err := Combined(treeAt(t, text, nil))
			if err != nil || got != text {
				t.Errorf("Combined = %q, %v; want the file's own bytes", got, err)
			}
		})
	}
}

func TestCombinedNamesABadFragmentAndAMissingChangelog(t *testing.T) {
	if _, err := Combined(treeAt(t, baseLF(), map[string]string{"bad.md": "- loose\n"})); err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("Combined with a bad fragment: err = %v, want one naming bad.md", err)
	}
	if _, err := Combined(treeAt(t, "", twoFragmentFiles())); err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Errorf("Combined with no changelog: err = %v, want one naming CHANGELOG.md", err)
	}
}
