// Package changelog is the rule set behind the changelog fragments: one file per change under
// changes/unreleased/, assembled under a new version heading at the release step by cmd/changelog.
//
// A branch that adds a change adds one new file and never edits CHANGELOG.md, so two branches cannot
// conflict on it, and the release step is one deterministic call that cannot eat a heading (the
// incidents were a new entry that overwrote the release heading above it, twice, and a union merge
// that joined two entries with no blank line between them). Everything here is stdlib only.
package changelog

import "io/fs"

// FragmentDir is where the fragments live, relative to the repository root, with forward slashes.
const FragmentDir = "changes/unreleased"

// ChangelogFile is the one file the tool edits, relative to the repository root. The vendored
// tools/*/CHANGELOG.md ledgers are never touched or globbed.
const ChangelogFile = "CHANGELOG.md"

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

// ReadFragments reads every fragment of dir, sorted by file name in byte order. A directory that does
// not exist holds no fragments. Any entry that is not a well-formed fragment is an error, because a
// silently skipped fragment is a lost entry; the error names every problem, not the first.
func ReadFragments(dir string) ([]Fragment, error) {
	return nil, nil
}

// readFragmentsFS is ReadFragments over any file system, with the problems as a list and each one
// prefixed with prefix (the directory the names belong to). The sort is its own, so a file system that
// lists in another order gives the same result.
func readFragmentsFS(fsys fs.FS, prefix string) ([]Fragment, []string) {
	return nil, nil
}

// ValidateFragment returns every rule the named fragment breaks; an empty slice means it is well formed.
func ValidateFragment(name, body string) []string {
	return nil
}

// UnreleasedBlock locates the body of the `## [Unreleased]` section of text: start is the offset just
// after the heading line, end the offset of the next `## [` heading (or the end of text). ok is false
// unless there is exactly one `## [Unreleased]` heading.
func UnreleasedBlock(text string) (start, end int, ok bool) {
	return 0, 0, false
}

// Splice moves the fragments into text. With rel nil it is a preview (the fragments are appended
// inside the [Unreleased] block, for readers); with rel set it is the release (a new `## [V]` heading
// under an emptied [Unreleased] block) and the result is checked against text before it is returned.
func Splice(text string, frags []Fragment, rel *Release) (string, error) {
	return "", nil
}

// verify checks a release against the text it was made from.
func verify(before, after string, rel *Release) error {
	return nil
}

// CheckTree is the one rule set for the tree at root: the fragments are well formed, the [Unreleased]
// block of CHANGELOG.md is blank, no fragment is already in CHANGELOG.md, every release heading
// appears once and no line but a heading carries a release date. It returns one message per problem.
func CheckTree(root string) []string {
	return nil
}

// CheckForRelease is CheckTree minus the blank-[Unreleased] rule: text left there by an old-style
// branch is moved under the new heading by the release, never dropped.
func CheckForRelease(root string) []string {
	return nil
}

// Combined returns CHANGELOG.md of root with its fragments spliced in as a preview, for the tests that
// read the changelog's prose: a fragment is changelog text before the release assembles it.
func Combined(root string) (string, error) {
	return "", nil
}
