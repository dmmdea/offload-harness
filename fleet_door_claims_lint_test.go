package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every description of a fleet node's doors says what the node does now: each lane needs the fleet bearer on a node that has a
// token, and a node with none answers only on loopback. A sentence that calls a media door open, says health ignores the
// bearer, or names the agent and vision lanes as the only gated ones is false, and it stays false in a guide, a comment or an
// error string long after the code moved. Two pins:
//
//   - a sentence that was true before the bearer covered every lane is not in the file that carried it;
//   - a decision record that still says a lane is tokenless carries the dated amendment that points at the one for the bearer,
//     because a record keeps what was decided and says, beside it, what replaced it.

// staleFleetDoorClaims lists (file, phrase): the phrase said something that no longer holds, in a file that describes the
// node as it is today. Whitespace and comment slashes are collapsed before the match, so a wrapped sentence is still found.
var staleFleetDoorClaims = []struct{ file, phrase string }{
	{"internal/fleetnode/compose_task.go", "not token-gated"},
	{"internal/fleetnode/tasks.go", "not token-gated"},
	{"internal/fleetnode/remote_out_test.go", "not token-gated"},
	{"internal/rosterprobe/token.go", "ignores the Authorization header"},
	{"internal/rosterprobe/token.go", "the agent lane is token-gated"},
	{"docs/systems/media-generation.md", "not token-gated"},
	{"docs/systems/media-generation.md", "are still read by bare name"},
	{"docs/systems/fleet-node.md", "For an agent or vision job"},
	{"docs/systems/security.md", "on the agent, vision, chat and kv-slot lanes"},
	{"docs/FLEET-NODE.md", "still reaches the media lane"},
	{"docs/OPERATOR-GUIDE.md", "reads the bearer only to decide"},
	{"docs/architecture/decisions/0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md", "tokenless tokenless"},
}

var lineCommentPrefix = regexp.MustCompile(`(?m)^[ 	]*//+`)

// collapseSpace joins a file's words with single spaces and drops the leading slashes of Go line comments, so a sentence
// that wraps over several comment lines reads as one.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(lineCommentPrefix.ReplaceAllString(s, "")), " ")
}

func TestEveryDescriptionOfTheFleetDoorSaysItNeedsTheBearer(t *testing.T) {
	for _, c := range staleFleetDoorClaims {
		raw, err := os.ReadFile(filepath.FromSlash(c.file))
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if strings.Contains(collapseSpace(string(raw)), c.phrase) {
			t.Errorf("%s still says %q, which no longer holds: every lane of a node with a token needs the fleet bearer", c.file, c.phrase)
		}
	}
}

func TestADecisionRecordThatCallsALaneTokenlessCarriesTheAmendmentForTheBearer(t *testing.T) {
	const dir = "docs/architecture/decisions"
	const amendedBy = "0023-agent-lane-tailnet-auth-and-locality.md"
	oldClaim := regexp.MustCompile(`(?i)tokenless|not token-gated|ignores the bearer|agent or vision job`)
	heading := regexp.MustCompile(`(?m)^## Amendment[^\n]*every lane needs the fleet bearer[^\n]*$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == amendedBy || !regexp.MustCompile(`^\d{4}-`).MatchString(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		text := string(raw)
		if !oldClaim.MatchString(text) {
			continue
		}
		loc := heading.FindStringIndex(text)
		if loc == nil {
			t.Errorf("%s calls a fleet lane tokenless or open and has no `## Amendment ... every lane needs the fleet bearer` section", name)
			continue
		}
		if !strings.Contains(text[loc[0]:], amendedBy) {
			t.Errorf("%s: the amendment for the bearer does not link %s", name, amendedBy)
		}
	}
}
