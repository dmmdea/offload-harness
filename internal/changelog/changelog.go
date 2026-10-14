// Package changelog is the rule set behind the changelog fragments: one file per change under
// changes/unreleased/, assembled under a new version heading at the release step by cmd/changelog.
//
// A branch that adds a change adds one new file and never edits CHANGELOG.md, so two branches cannot
// conflict on it, and the release step is one deterministic call that cannot eat a heading. The
// incidents it closes: a new entry that overwrote the release heading above it (twice), and a union
// merge that joined two entries with no blank line between them.
//
// What this package does not do: it does not remove the conflicts between two RELEASES on VERSION,
// internal/buildinfo and .printing-press.json, and it touches only the root CHANGELOG.md (the vendored
// tools/*/CHANGELOG.md ledgers are never read or edited). Everything here is stdlib only.
package changelog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// FragmentDir is where the fragments live, relative to the repository root, with forward slashes.
const FragmentDir = "changes/unreleased"

// ChangelogFile is the one file the tool edits, relative to the repository root.
const ChangelogFile = "CHANGELOG.md"

const unreleasedHeading = "## [Unreleased]"

var (
	// A fragment's file name: lower case, no spaces, no path, no leading dot.
	fragmentName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\.md$`)
	// A release version, x.y.z, with nothing else.
	versionForm = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// A line that opens a "#" or "##" heading. A fragment holds "###" sections only: the two levels above are the
	// changelog's own (its title and the release headings), and a forged "## [9.9.9] - x" line is the incident class.
	topLevelHeading = regexp.MustCompile(`^#{1,2}[ \t]`)
	// A release date between two separators, in either of the two the file has used: " - " (since 0.113.39) and an
	// em dash (before). Only a heading carries one.
	dateBetweenSeparators = regexp.MustCompile(`\s[-\x{2014}]\s\d{4}-\d{2}-\d{2}\s[-\x{2014}]\s`)
	// A release heading with a semver version, whatever follows it.
	semverHeading = regexp.MustCompile(`^## \[(\d+\.\d+\.\d+)\]`)
)

// Fragment is one file of FragmentDir.
type Fragment struct {
	Name string // the file name, e.g. feat-0187-recipe-fingerprint.md
	Body string // the text exactly as read
}

// Release is the heading Splice writes: `## [Version] - Date - Title`.
type Release struct {
	Version string // x.y.z
	Date    string // YYYY-MM-DD
	Title   string // one line, no padding
}

// ---- reading and validating fragments

// ReadFragments reads every fragment of dir, sorted by file name in byte order (so a-10.md comes before a-2.md; a
// numeric prefix is the way to force another order). A directory that does not exist holds no fragments. README.md is
// the one file that is not a fragment; any other entry that is not a well-formed fragment is an error, because a
// silently skipped fragment is a lost entry. The error names every problem, not the first.
func ReadFragments(dir string) ([]Fragment, error) {
	frags, problems := readFragmentsFS(os.DirFS(dir), "")
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return frags, nil
}

// readFragmentsFS is ReadFragments over any file system, with the problems as a list and each one prefixed with
// prefix (the directory the names belong to). The well-formed fragments are returned beside the problems. The sort
// is its own, so a file system that lists in another order gives the same result.
func readFragmentsFS(fsys fs.FS, prefix string) ([]Fragment, []string) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{strings.TrimSuffix(prefix, "/") + ": " + err.Error()}
	}
	var frags []Fragment
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if name == "README.md" {
			continue
		}
		if e.IsDir() {
			problems = append(problems, prefix+name+": a directory is not a fragment")
			continue
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			problems = append(problems, prefix+name+": "+err.Error())
			continue
		}
		if bad := ValidateFragment(name, string(raw)); len(bad) > 0 {
			for _, p := range bad {
				problems = append(problems, prefix+p)
			}
			continue
		}
		frags = append(frags, Fragment{Name: name, Body: string(raw)})
	}
	sort.Slice(frags, func(i, j int) bool { return frags[i].Name < frags[j].Name })
	return frags, problems
}

// ValidateFragment returns every rule the named fragment breaks, each starting with the file name; an empty slice
// means it is well formed. The rules: the name is lower case and ends in .md; the body is valid UTF-8 with no control
// byte but tab, CR and LF; it is not empty; its first non-blank line starts "### "; and no line starts a "#" or "##"
// heading, even inside a code fence. The "### " opening is a tightening the changelog itself never had (122 of its 387
// release entries open with a bullet or a lead paragraph): a loose bullet would be glued under the previous fragment's
// section. It is for fragments only, and the legacy text a release absorbs from [Unreleased] is exempt from it.
func ValidateFragment(name, body string) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, name+": "+fmt.Sprintf(format, args...))
	}
	if !fragmentName.MatchString(name) {
		add("file name must match %s", fragmentName)
	}
	if !utf8.ValidString(body) {
		add("not valid UTF-8")
	}
	for i, line := 0, 1; i < len(body); i++ {
		c := body[i]
		if c == '\n' {
			line++
		}
		if c < 0x20 && c != '\t' && c != '\r' && c != '\n' || c == 0x7f {
			add("control character 0x%02x on line %d", c, line)
			break
		}
	}
	if strings.TrimSpace(body) == "" {
		add("is empty")
		return problems
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.HasPrefix(l, "### ") {
			add("must open with a \"### \" heading (line %d is %q)", i+1, clip(l))
		}
		break
	}
	for i, l := range lines {
		if topLevelHeading.MatchString(l) {
			add("line %d starts a \"#\" or \"##\" heading, and only \"###\" sections belong in a fragment: %q", i+1, clip(l))
		}
	}
	return problems
}

func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "..."
	}
	return s
}

// ---- the [Unreleased] block

// eachLine calls fn for every line of text with its byte offset, the offset of the next line and its content without the
// line ending (LF or CRLF); fn returns false to stop.
func eachLine(text string, fn func(start, next int, line string) bool) {
	for pos := 0; pos < len(text); {
		end, next := len(text), len(text)
		if i := strings.IndexByte(text[pos:], '\n'); i >= 0 {
			end, next = pos+i, pos+i+1
		}
		if !fn(pos, next, strings.TrimSuffix(text[pos:end], "\r")) {
			return
		}
		pos = next
	}
}

// locateUnreleased finds the `## [Unreleased]` heading: head is the offset of the heading line, start the offset just
// after it, end the offset of the next line that starts `## [` (any bracket heading: the file has a `## [0.44.x
// interim]` heading that is not semver) or the end of text, and count how many such headings there are. The other
// three are meaningful only when count is 1.
func locateUnreleased(text string) (head, start, end, count int) {
	end = len(text)
	ended := false
	eachLine(text, func(s, next int, line string) bool {
		switch {
		case line == unreleasedHeading:
			count++
			if count == 1 {
				head, start = s, next
			}
		case count == 1 && !ended && strings.HasPrefix(line, "## ["):
			end, ended = s, true
		}
		return true
	})
	return head, start, end, count
}

// UnreleasedBlock locates the body of the `## [Unreleased]` section of text: start is the offset just after the heading
// line, end the offset of the next `## [` heading (or the end of text). ok is false unless there is exactly one
// `## [Unreleased]` heading, written exactly so (CR before the line ending is tolerated).
func UnreleasedBlock(text string) (start, end int, ok bool) {
	_, start, end, count := locateUnreleased(text)
	return start, end, count == 1
}

// ---- Splice

// trimBlock normalises line endings to LF, drops leading blank lines and trailing space: the text of one section.
func trimBlock(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 || strings.TrimSpace(s[:i]) != "" {
			break
		}
		s = s[i+1:]
	}
	return strings.TrimRight(s, " \t\r\n")
}

// hasHeading reports whether a line of text starts with "## [version]".
func hasHeading(text, version string) bool {
	found := false
	eachLine(text, func(_, _ int, line string) bool {
		found = strings.HasPrefix(line, "## ["+version+"]")
		return !found
	})
	return found
}

// Splice moves the fragments into text. With rel nil it is a preview (the fragments are appended inside the
// [Unreleased] block, for readers); with rel set it is the release: the output is
//
//	<head>## [Unreleased]\n\n## [V] - D - T\n\n<legacy text>\n\n<fragment 1>\n\n<fragment 2>...\n\n<rest>
//
// where rest is the old text from its top release heading to the end of the file, byte for byte. The line ending is the
// `## [Unreleased]` line's own, and the legacy text and the fragments are written in it; head and rest are never
// rewritten, so a file of mixed endings keeps them. Legacy text is whatever an old-style branch left under
// [Unreleased]: it moves under the new heading first and is never dropped, and no fragment rule applies to it. Sections
// are trimmed and joined by exactly one blank line. A release is refused for: a version that is not x.y.z, a date that
// is not a real YYYY-MM-DD, a title that is empty, has two lines or is padded, a `## [V]` heading the file already has
// (the overwrite and duplicate guard), nothing to release, and zero or two [Unreleased] headings. The result of a
// release is checked against text by verify before it is returned.
func Splice(text string, frags []Fragment, rel *Release) (string, error) {
	head, start, end, count := locateUnreleased(text)
	if count != 1 {
		return "", fmt.Errorf("%s must hold exactly one %q heading, found %d", ChangelogFile, unreleasedHeading, count)
	}
	eol := "\n"
	if strings.HasSuffix(text[head:start], "\r\n") {
		eol = "\r\n"
	}
	var sections []string
	if legacy := trimBlock(text[start:end]); legacy != "" {
		sections = append(sections, legacy)
	}
	for _, f := range frags {
		sections = append(sections, trimBlock(f.Body))
	}
	var b strings.Builder
	b.WriteString(text[:head])
	b.WriteString(unreleasedHeading + eol + eol)
	if rel == nil {
		if len(frags) == 0 {
			return text, nil
		}
	} else {
		if err := checkRelease(text, rel); err != nil {
			return "", err
		}
		if len(sections) == 0 {
			return "", errors.New("nothing to release: no fragment under " + FragmentDir + "/ and nothing under " + unreleasedHeading)
		}
		fmt.Fprintf(&b, "## [%s] - %s - %s%s%s", rel.Version, rel.Date, rel.Title, eol, eol)
	}
	b.WriteString(strings.ReplaceAll(strings.Join(sections, "\n\n"), "\n", eol))
	b.WriteString(eol)
	if rest := text[end:]; rest != "" {
		b.WriteString(eol + rest)
	}
	out := b.String()
	if rel != nil {
		if err := verify(text, out, rel); err != nil {
			return "", err
		}
	}
	return out, nil
}

// checkRelease refuses a release that cannot be written as a heading.
func checkRelease(text string, rel *Release) error {
	if !versionForm.MatchString(rel.Version) {
		return fmt.Errorf("version %q is not x.y.z", rel.Version)
	}
	if _, err := time.Parse("2006-01-02", rel.Date); err != nil {
		return fmt.Errorf("date %q is not a real YYYY-MM-DD date", rel.Date)
	}
	if rel.Title == "" {
		return errors.New("title is empty")
	}
	if strings.ContainsAny(rel.Title, "\r\n") {
		return errors.New("title must be one line")
	}
	if rel.Title != strings.TrimSpace(rel.Title) {
		return errors.New("title has leading or trailing space")
	}
	if hasHeading(text, rel.Version) {
		return fmt.Errorf("%s already has a %q heading: bump VERSION before assembling, and never release one version twice", ChangelogFile, "## ["+rel.Version+"]")
	}
	return nil
}

// verify checks a release against the text it was made from, so a bug in Splice cannot cost the file a line: the bytes
// before the [Unreleased] heading are unchanged, everything from the old top release heading to the end is byte for
// byte the old text, the new release heading appears exactly once, and the [Unreleased] block is blank.
func verify(before, after string, rel *Release) error {
	head, _, end, old := locateUnreleased(before)
	if old != 1 {
		return fmt.Errorf("verify: the old text has %d %q headings, want 1", old, unreleasedHeading)
	}
	prefix, suffix := before[:head], before[end:]
	if len(after) < len(prefix)+len(suffix) || !strings.HasPrefix(after, prefix) {
		return errors.New("verify: the text before the [Unreleased] heading changed")
	}
	if !strings.HasSuffix(after, suffix) {
		return errors.New("verify: the old text from its top release heading to the end of the file is not byte for byte what it was")
	}
	heading := "## [" + rel.Version + "]"
	n := 0
	eachLine(after, func(_, _ int, line string) bool {
		if strings.HasPrefix(line, heading) {
			n++
		}
		return true
	})
	if n != 1 {
		return fmt.Errorf("verify: the release heading %q appears %d times, want 1", heading, n)
	}
	_, s, e, c := locateUnreleased(after)
	if c != 1 || strings.TrimSpace(after[s:e]) != "" {
		return fmt.Errorf("verify: the %q block is not empty after the release", unreleasedHeading)
	}
	return nil
}

// ---- the tree

// CheckTree is the one rule set for the tree at root, and the root test and `cmd/changelog check` both call it: every
// file of changes/unreleased/ is a well-formed fragment; CHANGELOG.md holds one [Unreleased] heading and its block is
// blank ("entries go in changes/unreleased/"); no fragment's text is already in CHANGELOG.md (the duplicate a failed
// delete leaves behind); every semver release heading appears once; and no line but a heading carries a release date
// (a heading glued onto another line). A missing changes/unreleased/ is not a problem: it holds no fragments. It
// returns one message per problem, in a stable order.
func CheckTree(root string) []string { return checkTree(root, false) }

// CheckForRelease is CheckTree minus the blank-[Unreleased] rule: text left there by an old-style branch is moved under
// the new heading by the release, never dropped, so the release must not refuse it.
func CheckForRelease(root string) []string { return checkTree(root, true) }

func checkTree(root string, allowLegacy bool) []string {
	frags, problems := readFragmentsFS(os.DirFS(filepath.Join(root, filepath.FromSlash(FragmentDir))), FragmentDir+"/")
	raw, err := os.ReadFile(filepath.Join(root, ChangelogFile))
	if err != nil {
		return append(problems, ChangelogFile+": "+err.Error())
	}
	text := string(raw)

	if _, start, end, count := locateUnreleased(text); count != 1 {
		problems = append(problems, fmt.Sprintf("%s: want exactly one %q heading, found %d", ChangelogFile, unreleasedHeading, count))
	} else if !allowLegacy && strings.TrimSpace(text[start:end]) != "" {
		problems = append(problems, fmt.Sprintf("%s: the %q block is not empty: entries go in changes/unreleased/ (one file per change, see changes/README.md), never in CHANGELOG.md", ChangelogFile, unreleasedHeading))
	}

	released := strings.ReplaceAll(text, "\r\n", "\n")
	for _, f := range frags {
		if body := trimBlock(f.Body); strings.Contains(released, body) {
			problems = append(problems, fmt.Sprintf("%s/%s: its text is already in %s (a release assembled it and could not delete the file): delete the file", FragmentDir, f.Name, ChangelogFile))
		}
	}

	counts := map[string]int{}
	n := 0
	eachLine(text, func(_, _ int, line string) bool {
		n++
		if m := semverHeading.FindStringSubmatch(line); m != nil {
			counts[m[1]]++
		} else if !strings.HasPrefix(line, "## [") && dateBetweenSeparators.MatchString(line) {
			problems = append(problems, fmt.Sprintf("%s line %d: carries a release date but is not a heading (a heading glued onto another line?)", ChangelogFile, n))
		}
		return true
	})
	var dups []string
	for v, c := range counts {
		if c > 1 {
			dups = append(dups, v)
		}
	}
	sort.Strings(dups)
	for _, v := range dups {
		problems = append(problems, fmt.Sprintf("%s: release heading %q appears %d times", ChangelogFile, "## ["+v+"]", counts[v]))
	}
	return problems
}

// Combined returns CHANGELOG.md of root with its fragments spliced in as a preview, for the tests that read the
// changelog's prose: a fragment is changelog text from the day it is written, and a gate that read only CHANGELOG.md
// would see it first on the release branch. With no fragments it is the file's own text.
func Combined(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ChangelogFile))
	if err != nil {
		return "", err
	}
	frags, err := ReadFragments(filepath.Join(root, filepath.FromSlash(FragmentDir)))
	if err != nil {
		return "", err
	}
	return Splice(string(raw), frags, nil)
}
