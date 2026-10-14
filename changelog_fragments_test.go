package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/changelog"
)

// TestTheChangelogTreeRulesHoldOnTheRealTree is the repository's own call of the one rule set (internal/changelog.CheckTree,
// the same code `go run ./cmd/changelog check` runs): every file of changes/unreleased/ is a well-formed fragment, the
// `## [Unreleased]` block of CHANGELOG.md is empty, no fragment's text is already in CHANGELOG.md, every release heading
// appears once and no line but a heading carries a release date.
//
// The blank [Unreleased] block is the rule that keeps the old habit from coming back. A change adds one new file under
// changes/unreleased/ and never edits CHANGELOG.md, so two branches cannot conflict there and the release step cannot
// overwrite a heading (it did, twice); the release command moves the fragments under the new heading.
//
// The rules themselves are tested, red first, on fixture trees in internal/changelog; this is the thin caller that holds
// the real tree to them. The tracked-tree lints (conflict markers, control characters, operator identity, the leak gate)
// already read every fragment, because a fragment is a tracked file.
func TestTheChangelogTreeRulesHoldOnTheRealTree(t *testing.T) {
	if problems := changelog.CheckTree("."); len(problems) > 0 {
		t.Errorf("the changelog tree breaks %d rule(s):\n  %s\nA change adds a fragment under changes/unreleased/ (format and rules: changes/README.md) "+
			"and leaves CHANGELOG.md alone.", len(problems), strings.Join(problems, "\n  "))
	}
}

// docText reads a document of the tree at root for the prose gates (TestTheDocsQuoteTheSIGTERMCardTextTheCodeProduces and
// TestTheDocsDoNotClaimAnIdleBoxNeverTouchesTheServingConfig).
func docText(root, name string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	return string(raw), err
}
