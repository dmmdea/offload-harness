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
	"os"
	"strings"
	"testing"
)

func TestTheProseSaysWhichNameSelectsWhichRows(t *testing.T) {
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
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "//", " ")), " ")
		if !strings.Contains(text, "--node") && !strings.Contains(text, "nodeMatches") {
			t.Errorf("%s no longer describes the node filter: this check is blind", tc.path)
			continue
		}
		for _, phrase := range tc.retired {
			if strings.Contains(text, phrase) {
				t.Errorf("%s still says %q: a row written before the change carries only the hostname, so the card id never selects it, and a redeployed node's rows carry the card id in both fields, so the hostname stops selecting them", tc.path, phrase)
			}
		}
		if !strings.Contains(text, tc.want) {
			t.Errorf("%s does not say %q", tc.path, tc.want)
		}
	}
}
