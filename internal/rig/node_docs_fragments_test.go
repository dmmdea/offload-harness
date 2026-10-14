package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A changelog fragment is changelog text from the day it is written, so the check on the node-filter prose reads it too: a
// retired phrase that rides in on a fragment is refused where it is written, not first on the release branch. The control is
// a tree whose four surfaces all carry the corrected claim and the qualifier, which is clean.
func TestTheProseCheckSeesAChangelogFragment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "rig")
	good := "--node: the card id and the hostname each select a row once per name, and the rows carry the id in both fields, " +
		"for a node launched with --node-id and no fleet_node_id (the installers' form).\n"
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
	write("internal/rig/rig.go", good)
	write("docs/OPERATOR-GUIDE.md", good)
	write("docs/systems/fleet-node.md", good)
	write("CHANGELOG.md", "# Changelog\n\n## [Unreleased]\n\n## [0.185.0] - 2026-10-13 - x\n\n- "+good)
	if p := proseProblems(dir); len(p) != 0 {
		t.Fatalf("the control tree is not clean: %v", p)
	}

	write("changes/unreleased/fix-node-filter.md", "### Fixed - the node filter\n\n- A node picked by either name still selects both the old and the new rows.\n")
	p := strings.Join(proseProblems(dir), "\n")
	if !strings.Contains(p, "../../CHANGELOG.md still says \"either name still selects both the old and the new rows\"") {
		t.Errorf("a fragment carrying the retired wording is not refused:\n%s", p)
	}
}
