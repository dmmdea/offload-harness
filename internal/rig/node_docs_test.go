package rig

// `rig --node <name>` selects a row by its `node` or by the id the node stamped on its own result (result.node_id). Three kinds
// of row exist for a node launched with --node-id that differs from its hostname:
//
//	written by an older delegator:                  node = hostname, result.node_id = hostname
//	written by a 0.185.0 delegator for an old node: node = the card id, result.node_id = hostname
//	written by a 0.185.0 delegator for a new node:  node = the card id, result.node_id = the card id
//
// So the card id selects the last two and never the first, and the hostname selects the first two and never the last. The first
// draft of the comment, the guide, the node doc and the CHANGELOG said that a node picked by its card id "still finds the rows
// written before the change" (it does not: those rows carry only the hostname) and that either name selects old and new alike
// (a redeployed node's new rows carry the card id in both fields, so the hostname stops matching them). The code and the rig test
// were right; the prose was wider than the code. This refuses the retired wording and asks each surface for the corrected one.
//
// The comment is in rig.go (this directory); the docs are read from the repo root.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/changelog"
)

func TestTheProseSaysWhichNameSelectsWhichRows(t *testing.T) {
	for _, problem := range proseProblems(".") {
		t.Error(problem)
	}
}

// readProse reads a file of the check, named relative to dir (this package's directory: the docs and the CHANGELOG are
// two levels up). The CHANGELOG is read with its changelog fragments spliced in, because a fragment is changelog text from
// the day it is written and a gate that read only the file would see it first on the release branch, after the assembly.
func readProse(dir, path string) (string, error) {
	full := filepath.Join(dir, filepath.FromSlash(path))
	if filepath.Base(full) == changelog.ChangelogFile {
		return changelog.Combined(filepath.Dir(full))
	}
	raw, err := os.ReadFile(full)
	return string(raw), err
}

// proseProblems is that check over the files around dir, as a list, so a test can run it on a tree of its own
// (TestTheProseCheckSeesAChangelogFragment).
func proseProblems(dir string) []string {
	const qualifier = "for a node launched with --node-id and no fleet_node_id (the installers' form)"
	var problems []string
	for _, tc := range []struct {
		path    string
		retired []string // wording the code contradicts
		want    string   // the corrected claim, in this surface's words
	}{
		{"rig.go", []string{"selects the rows written before and after"}, "carry the id in both fields"},
		{"../../docs/OPERATOR-GUIDE.md", []string{"still finds the rows written before the change"}, "once per name"},
		{"../../docs/systems/fleet-node.md", []string{"finds the rows written before the change as well"}, "once per name"},
		{"../../CHANGELOG.md", []string{"either name still selects both the old and the new rows"}, "once per name"},
	} {
		raw, err := readProse(dir, tc.path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		text := strings.Join(strings.Fields(strings.ReplaceAll(raw, "//", " ")), " ")
		if !strings.Contains(text, "--node") && !strings.Contains(text, "nodeMatches") {
			problems = append(problems, fmt.Sprintf("%s no longer describes the node filter: this check is blind", tc.path))
			continue
		}
		for _, phrase := range tc.retired {
			if strings.Contains(text, phrase) {
				problems = append(problems, fmt.Sprintf("%s still says %q: a row written before the change carries only the hostname, so the card id never selects it, and a redeployed node's rows carry the card id in both fields, so the hostname stops selecting them", tc.path, phrase))
			}
		}
		if !strings.Contains(text, tc.want) {
			problems = append(problems, fmt.Sprintf("%s does not say %q", tc.path, tc.want))
		}
		// "never a row written before" holds only where the old row carries the hostname alone: a node launched with
		// --node-id (which an older build stamped as the hostname) and no fleet_node_id, the form the installers write. A node
		// whose id is its fleet_node_id, or its hostname, stamped the same id before, so the card id finds its older rows too.
		if plain := strings.ReplaceAll(text, "`", ""); !strings.Contains(plain, qualifier) {
			problems = append(problems, fmt.Sprintf("%s says the card id never selects a row written before without %q: that holds for one form of node only", tc.path, qualifier))
		}
	}
	return problems
}
