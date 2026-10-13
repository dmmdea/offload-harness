package delegate

// results[].node_reported has two sources on a node of the 0.185.0 build: a node on a build before it (the node stamps its
// hostname where it was launched with another --node-id) and a local run attributed to another box's engine (register C-58,
// where the runner stamps its own box and the placement names the engine's). The first wording of the field's comment and docs
// also named "a node that names itself by another id than the one it advertises", and a node on this build cannot: fleet-serve
// applies the id it resolved (fleetServeConfig) to the config its pipeline stamps results from, so health, the queue claim and
// the envelope carry one id. The code was right and the prose was wider than the code; this refuses the retired wording and
// asks each surface for the two cases that exist.
//
// The comment is in wire.go (this directory); the docs are read from the repo root.

import (
	"os"
	"strings"
	"testing"
)

// proseOf reads a file and flattens it the way a quote wraps across lines: whitespace collapsed, a Go comment's // markers
// dropped.
func proseOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "//", " ")), " ")
}

func TestTheProseNamesTheCasesNodeReportedActuallyHas(t *testing.T) {
	for _, tc := range []struct {
		path    string
		retired string   // a source that cannot differ on this build
		want    []string // the sources that exist, in this surface's words
	}{
		{"wire.go", "a different name than the one it advertises", []string{"a build before 0.185.0", "another box's engine"}},
		{"../../docs/OPERATOR-GUIDE.md", "than the one it advertises", []string{"a build before 0.185.0", "another box's engine"}},
		{"../../docs/systems/fleet-node.md", "than the one it advertises", []string{"a build before 0.185.0", "another box's engine"}},
	} {
		text := proseOf(t, tc.path)
		if !strings.Contains(text, "node_reported") {
			t.Errorf("%s no longer mentions node_reported: this check is blind", tc.path)
			continue
		}
		if strings.Contains(text, tc.retired) {
			t.Errorf("%s still says a node can name itself by another id %q: a node on this build stamps the id it advertises", tc.path, tc.retired)
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not name %q among the cases that produce node_reported", tc.path, want)
			}
		}
	}
}

// results[].node is the id /fleet/health publishes for a run on a fleet node this delegator dispatched to. A queue answer
// carries its claimant's stamp (the delegator has no health view of the claimant) and a local run carries a box's own name,
// so the operator guide, which says what node is, says so for the local run as well as the queue.
func TestTheOperatorGuideSaysWhatNodeIsForALocalRun(t *testing.T) {
	text := proseOf(t, "../../docs/OPERATOR-GUIDE.md")
	for _, want := range []string{
		"`results[].node` is the node id the node publishes in",           // control: the sentence this one qualifies
		"A `route: \"queue\"` answer carries the id its claimant stamped", // control: the queue clause was already there
		"A run on this box's own seat (`ran_on: \"local\"`)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("OPERATOR-GUIDE.md does not say %q", want)
		}
	}
}
