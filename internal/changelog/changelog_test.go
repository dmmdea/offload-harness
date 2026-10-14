package changelog

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Every fixture is built here with its line endings spelled out. None of these tests may depend on how the
// checkout stores CHANGELOG.md (CRLF in a Windows working tree, LF in the index and on Linux).

const (
	topLF = "# Changelog\n\nAll notable changes are documented in this file.\n\n"
	// tailLF starts at the top release heading, as the old file did; Splice must leave it byte for byte.
	tailLF = "## [0.185.1] - 2026-10-13 - the old title\n\n### Fixed - the old thing\n\n- The old bullet.\n\n" +
		"## [0.185.0] - 2026-10-12 - the older title\n\n- The older bullet, with no section of its own.\n"

	fragFirst  = "### Added - first thing\n\n- First bullet.\n"
	fragSecond = "### Fixed - second thing\n\n- Second bullet.\n"
	legacyLF   = "### Changed - a legacy entry\n\n- Legacy bullet, written straight into the block.\n"
)

var rel0186 = &Release{Version: "0.186.0", Date: "2026-10-14", Title: "the title"}

func eolOf(eol, s string) string { return strings.ReplaceAll(s, "\n", eol) }

// baseLF is a changelog whose [Unreleased] block is blank, as main's is.
func baseLF() string { return topLF + "## [Unreleased]\n\n" + tailLF }

// legacyBase is a changelog with entry text left in [Unreleased] by an old-style branch.
func legacyBase() string { return topLF + "## [Unreleased]\n\n" + legacyLF + "\n" + tailLF }

func frags(pairs ...string) []Fragment {
	var out []Fragment
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Fragment{Name: pairs[i], Body: pairs[i+1]})
	}
	return out
}

func twoFragments() []Fragment {
	return frags("a-first.md", fragFirst, "b-second.md", fragSecond)
}

func mustSplice(t *testing.T, text string, f []Fragment, rel *Release) string {
	t.Helper()
	got, err := Splice(text, f, rel)
	if err != nil {
		t.Fatalf("Splice: %v", err)
	}
	return got
}

func wantErr(t *testing.T, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one naming %q", substrs)
	}
	for _, s := range substrs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not name %q", err, s)
		}
	}
}

// ---- Splice, the release

func TestSpliceMovesFragmentsUnderANewHeadingAndLeavesTheRestByteForByte(t *testing.T) {
	want := topLF +
		"## [Unreleased]\n\n" +
		"## [0.186.0] - 2026-10-14 - the title\n\n" +
		"### Added - first thing\n\n- First bullet.\n\n" +
		"### Fixed - second thing\n\n- Second bullet.\n\n" +
		tailLF
	got := mustSplice(t, baseLF(), twoFragments(), rel0186)
	if got != want {
		t.Fatalf("release text differs\n got: %q\nwant: %q", got, want)
	}
	// The invariant the two overwrite incidents broke, stated on its own: from the old top release heading to the end
	// of the file the bytes are the old bytes, and nothing before the [Unreleased] heading moved.
	if !strings.HasSuffix(got, tailLF) {
		t.Errorf("the old top release heading and everything after it is not byte for byte the old text:\n%q", got)
	}
	if !strings.HasPrefix(got, topLF+"## [Unreleased]\n") {
		t.Errorf("the text before the new heading changed: %q", got)
	}
	if n := strings.Count(got, "\n## [0.185.1]"); n != 1 {
		t.Errorf("the old top heading appears %d times, want 1", n)
	}
}

func TestFragmentsAreJoinedByExactlyOneBlankLine(t *testing.T) {
	// A body may end with no newline, one, or several; the joined text has exactly one blank line between sections.
	f := frags(
		"a.md", "### Added - a\n\n- a",
		"b.md", "### Added - b\n\n- b\n",
		"c.md", "### Added - c\n\n- c\n\n\n\n",
	)
	want := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
		"### Added - a\n\n- a\n\n### Added - b\n\n- b\n\n### Added - c\n\n- c\n\n" + tailLF
	if got := mustSplice(t, baseLF(), f, rel0186); got != want {
		t.Errorf("sections are not joined by exactly one blank line\n got: %q\nwant: %q", got, want)
	}
}

func TestSpliceDropsLeadingBlankLinesOfAFragment(t *testing.T) {
	f := frags("a.md", "\n\n   \n### Added - a\n\n- a\n")
	want := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n### Added - a\n\n- a\n\n" + tailLF
	if got := mustSplice(t, baseLF(), f, rel0186); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestSpliceKeepsTheFilesLineEndings(t *testing.T) {
	wantLF := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
		"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n" + tailLF

	t.Run("CRLF file, LF fragments: every line of the result ends CRLF", func(t *testing.T) {
		got := mustSplice(t, eolOf("\r\n", baseLF()), twoFragments(), rel0186)
		if got != eolOf("\r\n", wantLF) {
			t.Fatalf("got %q\nwant %q", got, eolOf("\r\n", wantLF))
		}
		if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
			t.Errorf("a bare LF in a CRLF file: %q", got)
		}
	})
	t.Run("LF file, CRLF fragments: no carriage return in the result", func(t *testing.T) {
		f := frags("a-first.md", eolOf("\r\n", fragFirst), "b-second.md", eolOf("\r\n", fragSecond))
		got := mustSplice(t, baseLF(), f, rel0186)
		if got != wantLF {
			t.Fatalf("got %q\nwant %q", got, wantLF)
		}
		if strings.Contains(got, "\r") {
			t.Errorf("a carriage return in an LF file: %q", got)
		}
	})
	t.Run("the ending is the [Unreleased] line's own: CRLF heading, LF rest", func(t *testing.T) {
		in := topLF + "## [Unreleased]\r\n\r\n" + tailLF
		got := mustSplice(t, in, twoFragments(), rel0186)
		want := topLF + eolOf("\r\n", "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n"+
			"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n") + tailLF
		if got != want {
			t.Fatalf("mixed file\n got: %q\nwant: %q", got, want)
		}
		if !strings.HasSuffix(got, tailLF) {
			t.Errorf("the LF tail of a mixed file was rewritten")
		}
	})
	t.Run("the ending is the [Unreleased] line's own: LF heading, CRLF rest", func(t *testing.T) {
		in := eolOf("\r\n", topLF) + "## [Unreleased]\n\n" + eolOf("\r\n", tailLF)
		got := mustSplice(t, in, twoFragments(), rel0186)
		want := eolOf("\r\n", topLF) + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
			"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n" + eolOf("\r\n", tailLF)
		if got != want {
			t.Fatalf("mixed file\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestSpliceRefusesAVersionThatAlreadyHasAHeading(t *testing.T) {
	old := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-01 - an earlier cut\n\n- x\n\n" + tailLF
	_, err := Splice(old, twoFragments(), rel0186)
	wantErr(t, err, "0.186.0", "already")

	// The refusal is the heading, whole: a longer version that merely starts with the same digits is another release.
	other := topLF + "## [Unreleased]\n\n## [0.186.10] - 2026-10-01 - x\n\n- x\n\n" + tailLF
	if _, err := Splice(other, twoFragments(), &Release{Version: "0.186.1", Date: "2026-10-14", Title: "the title"}); err != nil {
		t.Errorf("0.186.10 exists and 0.186.1 is a different release, but Splice refused: %v", err)
	}
	// The overwrite incident: the version being released is the one at the top.
	top := topLF + "## [Unreleased]\n\n" + tailLF
	_, err = Splice(top, twoFragments(), &Release{Version: "0.185.1", Date: "2026-10-14", Title: "t"})
	wantErr(t, err, "0.185.1", "already")
}

func TestSpliceNeedsExactlyOneUnreleasedHeading(t *testing.T) {
	_, err := Splice(topLF+tailLF, twoFragments(), rel0186)
	wantErr(t, err, "[Unreleased]", "found 0")
	two := topLF + "## [Unreleased]\n\n" + "## [Unreleased]\n\n" + tailLF
	_, err = Splice(two, twoFragments(), rel0186)
	wantErr(t, err, "[Unreleased]", "found 2")
	// A preview needs the heading as well: there is nowhere to put the fragments.
	_, err = Splice(topLF+tailLF, twoFragments(), nil)
	wantErr(t, err, "[Unreleased]", "found 0")
}

func TestAnEmptyReleaseIsRefusedButLegacyUnreleasedTextIsKept(t *testing.T) {
	t.Run("no fragments and a blank block: nothing to release", func(t *testing.T) {
		_, err := Splice(baseLF(), nil, rel0186)
		wantErr(t, err, "nothing to release")
	})
	t.Run("a block of blank lines only is still nothing", func(t *testing.T) {
		_, err := Splice(topLF+"## [Unreleased]\n\n   \n\n"+tailLF, nil, rel0186)
		wantErr(t, err, "nothing to release")
	})
	t.Run("legacy text alone moves under the heading and [Unreleased] is left blank", func(t *testing.T) {
		want := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" + legacyLF + "\n" + tailLF
		if got := mustSplice(t, legacyBase(), nil, rel0186); got != want {
			t.Fatalf("got %q\nwant %q", got, want)
		}
	})
	t.Run("legacy text and fragments: the legacy text comes first, unvalidated", func(t *testing.T) {
		// A bullet first and a heading of the wrong level: legacy text is exempt from every fragment rule.
		legacy := "- A loose bullet with no section heading.\n\n## Not a release heading\n\n- Another.\n"
		in := topLF + "## [Unreleased]\n\n" + legacy + "\n" + tailLF
		want := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
			legacy + "\n" + "### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n" + tailLF
		if got := mustSplice(t, in, twoFragments(), rel0186); got != want {
			t.Fatalf("got %q\nwant %q", got, want)
		}
	})
	t.Run("legacy text is not dropped when the file is CRLF, and takes its endings", func(t *testing.T) {
		got := mustSplice(t, eolOf("\r\n", legacyBase()), twoFragments(), rel0186)
		if !strings.Contains(got, "Legacy bullet, written straight into the block.") {
			t.Fatalf("legacy text dropped: %q", got)
		}
		if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
			t.Errorf("a bare LF in a CRLF file: %q", got)
		}
	})
}

func TestSpliceWithTheHeadingAtTheEndOfTheFile(t *testing.T) {
	got := mustSplice(t, topLF+"## [Unreleased]\n", twoFragments(), rel0186)
	want := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
		"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestSpliceRefusesABadVersionDateOrTitle(t *testing.T) {
	good := Release{Version: "0.186.0", Date: "2026-10-14", Title: "a title"}
	if _, err := Splice(baseLF(), twoFragments(), &good); err != nil {
		t.Fatalf("the control release is refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		rel  Release
		want string
	}{
		{"version with two parts", Release{"0.2", good.Date, good.Title}, "version"},
		{"version with a v", Release{"v0.2.0", good.Date, good.Title}, "version"},
		{"empty version", Release{"", good.Date, good.Title}, "version"},
		{"version with a suffix", Release{"0.2.0-rc1", good.Date, good.Title}, "version"},
		{"version with a space", Release{"0.2.0 ", good.Date, good.Title}, "version"},
		{"date with month 13", Release{good.Version, "2026-13-40", good.Title}, "date"},
		{"date in the wrong order", Release{good.Version, "13-10-2026", good.Title}, "date"},
		{"date without zero padding", Release{good.Version, "2026-1-5", good.Title}, "date"},
		{"empty date", Release{good.Version, "", good.Title}, "date"},
		{"date with a trailing space", Release{good.Version, "2026-10-14 ", good.Title}, "date"},
		{"empty title", Release{good.Version, good.Date, ""}, "title"},
		{"two-line title", Release{good.Version, good.Date, "one\ntwo"}, "title"},
		{"title with a carriage return", Release{good.Version, good.Date, "one\rtwo"}, "title"},
		{"title with a leading space", Release{good.Version, good.Date, " padded"}, "title"},
		{"title with a trailing space", Release{good.Version, good.Date, "padded "}, "title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := tc.rel
			_, err := Splice(baseLF(), twoFragments(), &rel)
			wantErr(t, err, tc.want)
		})
	}
}

func TestSpliceRefusesAFragmentThatWouldLeaveTheTreeRed(t *testing.T) {
	// Splice takes the fragments it is given (the reader validates them), so its own output check is the last line of
	// defence: it must not write a file that CheckTree refuses ("carries a release date but is not a heading").
	for _, tc := range []struct{ name, body string }{
		{"a dated ### heading", "### Fixed - 2026-10-14 - the card text\n\n- x\n"},
		{"a dated bullet", fragFirst + "\n- logged as foo - 2026-10-13 - bar in the output.\n"},
		{"a dated line in a CRLF fragment", eolOf("\r\n", fragFirst+"\n- logged as foo - 2026-10-13 - bar.\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Splice(baseLF(), frags("a.md", tc.body), rel0186)
			wantErr(t, err, "release date")
			if got != "" {
				t.Errorf("text returned beside the error: %q", got)
			}
		})
	}
}

func TestSpliceDoesNotJudgeTheOldText(t *testing.T) {
	// The old text from its top release heading on is kept byte for byte, whatever it says: the tree check polices it
	// (and cmd/changelog runs that check before it splices), the output check only the section Splice wrote.
	tail := tailLF + "\nA stray line - 2026-01-01 - glued long ago.\n"
	got := mustSplice(t, topLF+"## [Unreleased]\n\n"+tail, twoFragments(), rel0186)
	if !strings.HasSuffix(got, tail) {
		t.Errorf("the old text is not kept byte for byte: %q", got)
	}
}

func TestPreviewPutsFragmentsInsideTheUnreleasedBlock(t *testing.T) {
	want := topLF + "## [Unreleased]\n\n" +
		"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n" + tailLF
	got := mustSplice(t, baseLF(), twoFragments(), nil)
	if got != want {
		t.Fatalf("preview\n got: %q\nwant: %q", got, want)
	}
	if n, base := strings.Count(got, "\n## ["), strings.Count(baseLF(), "\n## ["); n != base {
		t.Errorf("the preview adds a release heading: %d headings, the file had %d", n, base)
	}
	// The preview is the block's own text: the legacy text stays, then the fragments.
	withLegacy := mustSplice(t, legacyBase(), twoFragments(), nil)
	wantLegacy := topLF + "## [Unreleased]\n\n" + legacyLF + "\n" +
		"### Added - first thing\n\n- First bullet.\n\n### Fixed - second thing\n\n- Second bullet.\n\n" + tailLF
	if withLegacy != wantLegacy {
		t.Errorf("preview with legacy text\n got: %q\nwant: %q", withLegacy, wantLegacy)
	}
	// No fragments: the text itself, byte for byte.
	if same := mustSplice(t, eolOf("\r\n", baseLF()), nil, nil); same != eolOf("\r\n", baseLF()) {
		t.Errorf("a preview with no fragments changed the text: %q", same)
	}
}

// ---- the [Unreleased] block

func TestUnreleasedBlockEndsAtAnyBracketHeading(t *testing.T) {
	for _, tc := range []struct {
		name, text, block string
	}{
		{"a semver heading", "a\n## [Unreleased]\n\nlegacy\n\n## [0.185.1] - x\nz\n", "\nlegacy\n\n"},
		{"the interim heading is not semver", "a\n## [Unreleased]\n\nlegacy\n\n## [0.44.x interim] - 2026-08-10\nz\n", "\nlegacy\n\n"},
		{"CRLF", "a\r\n## [Unreleased]\r\n\r\nlegacy\r\n\r\n## [0.44.x interim] - 2026-08-10\r\nz\r\n", "\r\nlegacy\r\n\r\n"},
		{"the end of the file", "a\n## [Unreleased]\n\nlegacy\n", "\nlegacy\n"},
		{"an empty block", "a\n## [Unreleased]\n## [0.1.0] - x\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := UnreleasedBlock(tc.text)
			if !ok {
				t.Fatalf("no block found in %q", tc.text)
			}
			if start < 0 || end < start || end > len(tc.text) {
				t.Fatalf("bad bounds %d..%d for a text of %d bytes", start, end, len(tc.text))
			}
			if got := tc.text[start:end]; got != tc.block {
				t.Errorf("block = %q, want %q", got, tc.block)
			}
		})
	}
}

func TestUnreleasedBlockWantsExactlyOneExactHeading(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"none", "a\n## [0.1.0] - x\n"},
		{"two", "## [Unreleased]\n\n## [Unreleased]\n"},
		{"a heading with more text on the line", "## [Unreleased] - soon\n"},
		{"a different spelling", "## [unreleased]\n"},
		{"indented", "  ## [Unreleased]\n"},
		{"a third-level heading", "### [Unreleased]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := UnreleasedBlock(tc.text); ok {
				t.Errorf("ok for %q, want not", tc.text)
			}
		})
	}
}

// ---- ValidateFragment

func TestValidateFragment(t *testing.T) {
	const ok = "### Added - a thing\n\n- It works.\n"
	for _, name := range []string{"feat-0187-recipe-fingerprint.md", "a.md", "0.md", "a.b_c-9.md", "x-2.md"} {
		if p := ValidateFragment(name, ok); len(p) != 0 {
			t.Errorf("%s: a well-formed fragment is refused: %v", name, p)
		}
	}
	// Tabs, carriage returns, other levels of heading and a hash that is not a heading are all fine.
	fine := "### Added - a\r\n\r\n- tab\there\r\n\r\n#### Detail\r\n\r\n#hashtag is not a heading\r\n"
	if p := ValidateFragment("a.md", fine); len(p) != 0 {
		t.Errorf("a tab, CRLF, a #### line and a bare #tag are legal: %v", p)
	}

	for _, tc := range []struct {
		name, file, body, want string
	}{
		{"an upper-case name", "Notes.md", ok, "file name"},
		{"a name with a space", "my notes.md", ok, "file name"},
		{"a name that is not .md", "notes.txt", ok, "file name"},
		{"a name with no extension", "notes", ok, "file name"},
		{"a dotfile", ".hidden.md", ok, "file name"},
		{"a name with a path", "sub/notes.md", ok, "file name"},
		{"an empty body", "a.md", "", "is empty"},
		{"a body of blank lines", "a.md", "\n  \n\t\n", "is empty"},
		{"a body that opens with a bullet", "a.md", "- A loose bullet.\n", "must open with"},
		{"a body that opens with prose", "a.md", "Some text.\n\n### Added - x\n", "must open with"},
		{"a body that opens with a fourth-level heading", "a.md", "#### Added - x\n", "must open with"},
		{"a ### with no space", "a.md", "###Added\n", "must open with"},
		{"a forged release heading", "a.md", ok + "\n## [9.9.9] - 2026-10-14 - forged\n", `starts a "#" or "##" heading`},
		{"a ## heading", "a.md", ok + "\n## A section\n", `starts a "#" or "##" heading`},
		{"a # heading", "a.md", ok + "\n# A title\n", `starts a "#" or "##" heading`},
		{"a ## heading with a tab", "a.md", ok + "\n##\tA section\n", `starts a "#" or "##" heading`},
		{"a ## heading inside a code fence", "a.md", ok + "\n```\n## [9.9.9] - x\n```\n", `starts a "#" or "##" heading`},
		{"a ## heading in a CRLF body", "a.md", eolOf("\r\n", ok+"\n## A section\n"), `starts a "#" or "##" heading`},
		{"a NUL byte", "a.md", "### Added - a\x00b\n", "control character"},
		{"a backspace", "a.md", "### Added - a\x08b\n", "control character"},
		{"a DEL byte", "a.md", "### Added - a\x7fb\n", "control character"},
		{"an escape", "a.md", "### Added - a\x1b[0m\n", "control character"},
		{"invalid UTF-8", "a.md", "### Added - a\xffb\n", "UTF-8"},
		// The tree rule "no line but a heading carries a release date" has to hold for a fragment too, or the assembler
		// writes a file its own check refuses, after the fragment is gone.
		{"a release date between separators in a ### heading", "a.md", "### Fixed - 2026-10-14 - the card text\n\n- x\n", "release date"},
		{"a release date between separators in a bullet", "a.md", ok + "\n- logged as foo - 2026-10-13 - bar in the output.\n", "release date"},
		{"a release date between em dashes", "a.md", ok + "\n- logged as foo — 2026-10-13 — bar.\n", "release date"},
		{"a release date between separators in a CRLF body", "a.md", eolOf("\r\n", ok+"\n- logged as foo - 2026-10-13 - bar.\n"), "release date"},
		// A CR that is not part of a CRLF ends a line in Markdown, and every line rule here splits on LF: a heading can hide
		// behind it from the "##" rule and from the heading count, and still render as a release heading.
		{"a lone CR before a forged heading", "a.md", ok + "\r## [9.9.9] - forged\r- y\n", "carriage return"},
		{"a lone CR at the end of the body", "a.md", ok + "x\r", "carriage return"},
		{"a lone CR as the first byte", "a.md", "\r" + ok, "carriage return"},
		{"a CR followed by a CR and a line feed", "a.md", "### Added - a\r\r\n- b\n", "carriage return"},
		{"one bare CR in a CRLF body", "a.md", eolOf("\r\n", ok) + "- b\r- c\r\n", "carriage return"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := ValidateFragment(tc.file, tc.body)
			if len(problems) == 0 {
				t.Fatalf("%q with body %q is accepted, want a problem naming %q", tc.file, tc.body, tc.want)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("problems %q do not name %q", joined, tc.want)
			}
			if !strings.Contains(joined, tc.file) {
				t.Errorf("problems %q do not name the file %q", joined, tc.file)
			}
		})
	}

	t.Run("the line of a heading is named", func(t *testing.T) {
		problems := ValidateFragment("a.md", "### Added - a\n\n- b\n\n## [9.9.9] - x\n")
		if joined := strings.Join(problems, "\n"); !strings.Contains(joined, "line 5") {
			t.Errorf("the forged heading is on line 5 and the problem says %q", joined)
		}
	})

	t.Run("the line of a dated line is named", func(t *testing.T) {
		problems := ValidateFragment("a.md", "### Added - a\n\n- b\n\n- c - 2026-10-14 - d\n")
		if joined := strings.Join(problems, "\n"); !strings.Contains(joined, "line 5") {
			t.Errorf("the dated line is line 5 and the problem says %q", joined)
		}
	})

	t.Run("the line of a bare carriage return is named", func(t *testing.T) {
		problems := ValidateFragment("a.md", "### Added - a\n\n- b\n\n- c\r- d\n")
		if joined := strings.Join(problems, "\n"); !strings.Contains(joined, "line 5") {
			t.Errorf("the bare CR is on line 5 and the problem says %q", joined)
		}
	})

	t.Run("a date in prose is not a release date", func(t *testing.T) {
		// One separator, a date after a word, or a date with no separator on its far side: none of these is a heading's shape.
		fine := "### Added - a\n\n- Shipped on 2026-09-01 - and on 2026-09-02.\n- Open from 2026-10-14 - on.\n- Due 2026-10-14 only.\n- A range - 2026-10-14 onwards.\n"
		if p := ValidateFragment("a.md", fine); len(p) != 0 {
			t.Errorf("dates in prose are refused: %v", p)
		}
	})
}

// ---- ReadFragments

func TestFragmentsAreOrderedByFileName(t *testing.T) {
	// Byte order: "-" (0x2d) sorts before "." (0x2e), so a-10 comes before a-2, and both before a.md.
	mem := fstest.MapFS{
		"b.md":    {Data: []byte("### Added - b\n")},
		"a.md":    {Data: []byte("### Added - a\n")},
		"a-2.md":  {Data: []byte("### Added - a2\n")},
		"a-10.md": {Data: []byte("### Added - a10\n")},
		"0-x.md":  {Data: []byte("### Added - zero\n")},
	}
	// Whatever order the file system lists them in, the result is sorted: this one lists them backwards.
	got, problems := readFragmentsFS(reversedFS{mem}, "")
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	want := []string{"0-x.md", "a-10.md", "a-2.md", "a.md", "b.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", names, want)
	}
	if got[0].Body != "### Added - zero\n" {
		t.Errorf("the body is not the file's text: %q", got[0].Body)
	}
}

// reversedFS lists a directory backwards, so the test needs the sort and not the file system's order.
type reversedFS struct{ fstest.MapFS }

func (r reversedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	es, err := r.MapFS.ReadDir(name)
	for i, j := 0, len(es)-1; i < j; i, j = i+1, j-1 {
		es[i], es[j] = es[j], es[i]
	}
	return es, err
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

func TestReadFragmentsOfARealDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "changes", "unreleased")
	writeFile(t, filepath.Join(dir, "b.md"), fragSecond)
	writeFile(t, filepath.Join(dir, "a.md"), fragFirst)
	writeFile(t, filepath.Join(dir, "README.md"), "# How this directory works\n\nNot a fragment.\n")
	got, err := ReadFragments(dir)
	if err != nil {
		t.Fatalf("ReadFragments: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a.md" || got[1].Name != "b.md" {
		t.Fatalf("got %+v, want a.md and b.md (README.md is not a fragment)", got)
	}
	if got[0].Body != fragFirst {
		t.Errorf("body = %q", got[0].Body)
	}
}

func TestAnAbsentFragmentDirectoryMeansNoFragments(t *testing.T) {
	got, err := ReadFragments(filepath.Join(t.TempDir(), "changes", "unreleased"))
	if err != nil || len(got) != 0 {
		t.Errorf("ReadFragments(absent) = %v, %v; want no fragments and no error", got, err)
	}
	empty := t.TempDir()
	if got, err := ReadFragments(empty); err != nil || len(got) != 0 {
		t.Errorf("ReadFragments(empty) = %v, %v", got, err)
	}
}

func TestReadFragmentsNamesEveryProblem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.md"), fragFirst)
	writeFile(t, filepath.Join(dir, "bad-one.md"), "- no heading\n")
	writeFile(t, filepath.Join(dir, "bad-two.md"), "### Added - x\n\n## [9.9.9] - forged\n")
	writeFile(t, filepath.Join(dir, "Notes.txt"), "stray")
	writeFile(t, filepath.Join(dir, "sub", "inner.md"), fragFirst)
	got, err := ReadFragments(dir)
	if err == nil {
		t.Fatalf("no error for a directory with four bad entries; got %+v", got)
	}
	for _, name := range []string{"bad-one.md", "bad-two.md", "Notes.txt", "sub"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "sub: a directory is not a fragment") {
		t.Errorf("a directory is refused for what it is, not for a read error: %v", err)
	}
	if strings.Contains(err.Error(), "good.md") {
		t.Errorf("the error names the good fragment: %v", err)
	}
	if got != nil {
		t.Errorf("fragments returned beside an error: %+v", got)
	}
}

// ---- verify

func TestVerifyRefusesAnOutputThatLostALineOfTheOldFile(t *testing.T) {
	before := baseLF()
	good := topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n" +
		"### Added - first thing\n\n- First bullet.\n\n" + tailLF
	if err := verify(before, good, rel0186); err != nil {
		t.Fatalf("the control output is refused: %v", err)
	}
	for _, tc := range []struct {
		name, after string
	}{
		{"the old top heading is gone",
			strings.Replace(good, "## [0.185.1] - 2026-10-13 - the old title\n", "", 1)},
		{"the new heading took the old top heading's place",
			topLF + "## [Unreleased]\n\n## [0.186.0] - 2026-10-14 - the title\n\n### Added - first thing\n\n- First bullet.\n\n" +
				strings.TrimPrefix(tailLF, "## [0.185.1] - 2026-10-13 - the old title\n")},
		{"a line of the old tail changed",
			strings.Replace(good, "- The old bullet.", "- The old bullet, edited.", 1)},
		{"the old tail lost its last line",
			strings.TrimSuffix(good, "- The older bullet, with no section of its own.\n")},
		{"the head changed", strings.Replace(good, "All notable changes", "Some notable changes", 1)},
		{"the new heading is missing", strings.Replace(good, "## [0.186.0] - 2026-10-14 - the title\n\n", "", 1)},
		{"the release is gone entirely: the old text, nothing added", before},
		{"the new heading is there twice", strings.Replace(good, "### Added", "## [0.186.0] - 2026-10-14 - again\n\n### Added", 1)},
		{"[Unreleased] is not blank", strings.Replace(good, "## [Unreleased]\n\n", "## [Unreleased]\n\n- stray text\n\n", 1)},
		// The tree check refuses a non-heading line with a release date; an output that holds one is refused before the write.
		{"a release date glued onto a line of the new section",
			strings.Replace(good, "- First bullet.", "- First bullet - 2026-10-14 - glued.", 1)},
		{"a dated ### heading in the new section",
			strings.Replace(good, "### Added - first thing", "### Added - 2026-10-14 - first thing", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verify(before, tc.after, rel0186); err == nil {
				t.Errorf("verify accepted %q", tc.after)
			}
		})
	}
}
