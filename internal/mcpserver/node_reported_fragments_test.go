package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A changelog fragment is changelog text from the day it is written, so the check on the CHANGELOG's account of where
// node_reported is stamped reads it too. The control is a tree whose released section carries the whole sentence; the
// fragment that follows is the first "The rule." bullet the check finds (the unreleased text sits above the releases), and it
// leaves a door out.
func TestTheNodeReportedRuleCheckSeesAChangelogFragment(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("CHANGELOG.md", "# Changelog\n\n## [Unreleased]\n\n## [0.185.0] - 2026-10-13 - x\n\n"+
		"- **The rule.** `results[].node_reported` is stamped in one place and published by agent_run, offload_ask, the named-seat door and offload_review_diff.\n")
	if p := changelogRuleProblems(root); len(p) != 0 {
		t.Fatalf("the control tree is not clean: %v", p)
	}

	write("changes/unreleased/fix-node-reported.md", "### Fixed - the node_reported sentence\n\n"+
		"- **The rule.** `results[].node_reported` is stamped in one place and published by agent_run only.\n")
	p := strings.Join(changelogRuleProblems(root), "\n")
	if !strings.Contains(p, "does not name the offload_review_diff door") {
		t.Errorf("a fragment whose sentence leaves a door out is not refused:\n%s", p)
	}
}
