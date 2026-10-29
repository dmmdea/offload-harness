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
// TestTheDocsDoNotClaimAnIdleBoxNeverTouchesTheServingConfig). A fragment is changelog text from the day it is written, so
// CHANGELOG.md is read with its fragments spliced in: a retired phrase that rides in on a fragment fails here, on the branch
// that wrote it, and not first on the release branch after the assembly.
func docText(root, name string) (string, error) {
	if name == changelog.ChangelogFile {
		return changelog.Combined(root) // the live file, its fragments, then the archived releases
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	return string(raw), err
}

// writeFixtureTree writes files (slash-separated paths relative to root) under root.
func writeFixtureTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const fragmentPath = "changes/unreleased/fix-the-card-text.md"

// The check that refuses the retired SIGTERM card wording reads the unreleased text: a fragment that carries it is refused
// where it is written. The control is the same tree without the fragment, which is clean.
func TestTheSIGTERMQuoteCheckSeesAChangelogFragment(t *testing.T) {
	root := t.TempDir()
	quote := "ended on SIGTERM: " + termCauseLikely
	writeFixtureTree(t, root, map[string]string{
		"CHANGELOG.md": "# Changelog\n\n## [Unreleased]\n\n## [0.180.1] - 2026-10-10 - x\n\n- The card says " + quote + ".\n\n" +
			"## [0.180.0] - 2026-10-09 - y\n\n- History: the card said terminated: its console was closed.\n",
		"docs/systems/pair-workloads.md": "The card says " + quote + ".\n",
		"docs/systems/gpu-lease.md":      "No quote here.\n",
	})
	if p := sigtermDocProblems(root); len(p) != 0 {
		t.Fatalf("the control tree is not clean: %v", p)
	}

	writeFixtureTree(t, root, map[string]string{
		fragmentPath: "### Fixed - the card text\n\n- The card says terminated: its console was closed, or the session is ending.\n",
	})
	p := strings.Join(sigtermDocProblems(root), "\n")
	if !strings.Contains(p, "CHANGELOG.md still quotes the retired card text") {
		t.Errorf("a fragment carrying the retired wording is not refused:\n%s", p)
	}

	// A fragment that quotes the clause wrongly is held to the same text.
	writeFixtureTree(t, root, map[string]string{
		fragmentPath: "### Fixed - the card text\n\n- The card says " + "ended on SIGTERM: " + "something the code does not say.\n",
	})
	if p := strings.Join(sigtermDocProblems(root), "\n"); !strings.Contains(p, "CHANGELOG.md quotes the SIGTERM clause as") {
		t.Errorf("a fragment quoting the clause wrongly is not refused:\n%s", p)
	}
}

// The check that refuses the retired idle-box wording reads the unreleased text too.
func TestTheFenceCostCheckSeesAChangelogFragment(t *testing.T) {
	root := t.TempDir()
	writeFixtureTree(t, root, map[string]string{
		"CHANGELOG.md":                        "# Changelog\n\n## [Unreleased]\n\n## [0.183.0] - 2026-10-12 - x\n\n- The fence reads the file.\n",
		"docs/systems/gpu-lease.md":           "The fence reads the file under a lease that names cards.\n",
		"internal/modelaffinity/seatscope.go": "package modelaffinity\n",
	})
	if p := fenceDocProblems(root); len(p) != 0 {
		t.Fatalf("the control tree is not clean: %v", p)
	}
	writeFixtureTree(t, root, map[string]string{
		fragmentPath: "### Changed - the fence\n\n- The idle path pays nothing new.\n",
	})
	p := strings.Join(fenceDocProblems(root), "\n")
	if !strings.Contains(p, "CHANGELOG.md still says \"The idle path pays nothing new.\"") {
		t.Errorf("a fragment carrying the retired wording is not refused:\n%s", p)
	}
}
