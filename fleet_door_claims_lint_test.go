package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Every description of a fleet node's doors says what the node does now: each lane needs the fleet bearer on a node that has a
// token, and a node with none answers only on loopback. A sentence that calls a media door open, says health ignores the
// bearer, or names the agent and vision lanes as the only gated ones is false, and it stays false in a guide, a comment or an
// error string long after the code moved. Three pins:
//
//   - a sentence that was true before the bearer covered every lane is not in the file that carried it;
//   - each row of that list carries the sentence it was taken from, and the lint finds the phrase in it: a row that matched
//     nothing in any version of its file would pin nothing, and the file could say the claim again with the lint green;
//   - a decision record that still says a lane is tokenless carries the dated amendment that points at the one for the bearer,
//     because a record keeps what was decided and says, beside it, what replaced it.

// staleDoorClaim is one sentence that no longer holds, in a file that describes the node as it is today.
type staleDoorClaim struct {
	file   string // the file that carried it
	phrase string // what it said, searched for with whitespace and comment slashes collapsed, so a wrapped sentence is still found
	was    string // the file's words around it as they stood before the bearer covered every lane: the phrase must be found in them
}

var staleFleetDoorClaims = []staleDoorClaim{
	{file: "internal/fleetnode/compose_task.go", phrase: "not token-gated",
		was: "off, and media dispatch is not token-gated (ADR 0023). A free-form `html`"},
	{file: "internal/fleetnode/tasks.go", phrase: "not token-gated",
		was: "the caller's. Media dispatch is not token-gated (ADR 0023), so a caller-supplied"},
	{file: "internal/fleetnode/remote_out_test.go", phrase: "not token-gated",
		was: "} TestRemoteMediaTaskOutNeverReachesThePipeline: media dispatch is not token-gated (ADR 0023), so a remote"},
	{file: "internal/fleetnode/server.go", phrase: "an agent (or vision) job",
		was: "the wire: the job is an agent (or vision) job, a token is configured,"},
	{file: "internal/rosterprobe/token.go", phrase: "ignores the Authorization header",
		was: "read of a node: /fleet/health ignores the Authorization header, so a token mismatch"},
	{file: "internal/rosterprobe/token.go", phrase: "the agent lane is token-gated",
		was: "the bearer check first (the agent lane is token-gated) and validates the envelope"},
	{file: "docs/systems/media-generation.md", phrase: "not token-gated",
		was: "`run-graph`. The fleet door is not token-gated ([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)), so it refuses"},
	{file: "docs/systems/media-generation.md", phrase: "are still read by bare name",
		was: "input file (the tokenless dispatch) are still read by bare name. **What comes back.** The"},
	{file: "docs/systems/fleet-node.md", phrase: "For an agent or vision job",
		was: "job in `error` returns `409`. For an agent or vision job on a node with a"},
	{file: "docs/systems/security.md", phrase: "on the agent, vision, chat and kv-slot lanes",
		was: "listeners; one shared fleet bearer on the agent, vision, chat and kv-slot lanes; tailnet transport; `--listen-trusted-network` permits"},
	{file: "docs/FLEET-NODE.md", phrase: "still reaches the media lane",
		was: "that is not a browser still reaches the media lane. A node shared in"},
	{file: "docs/OPERATOR-GUIDE.md", phrase: "reads the bearer only to decide"},
	{file: "docs/architecture/decisions/0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md", phrase: "tokenless tokenless"},
}

var lineCommentPrefix = regexp.MustCompile(`(?m)^[ 	]*//+`)

// collapseSpace joins a file's words with single spaces and drops the leading slashes of Go line comments, so a sentence
// that wraps over several comment lines reads as one.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(lineCommentPrefix.ReplaceAllString(s, "")), " ")
}

// staleClaimsIn lists the phrases of the rows for file that text carries.
func staleClaimsIn(file, text string) []string {
	collapsed := collapseSpace(text)
	var found []string
	for _, c := range staleFleetDoorClaims {
		if c.file == file && strings.Contains(collapsed, c.phrase) {
			found = append(found, c.phrase)
		}
	}
	return found
}

func TestEveryDescriptionOfTheFleetDoorSaysItNeedsTheBearer(t *testing.T) {
	seen := map[string]bool{}
	var files []string
	for _, c := range staleFleetDoorClaims {
		if !seen[c.file] {
			seen[c.file] = true
			files = append(files, c.file)
		}
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		for _, phrase := range staleClaimsIn(file, string(raw)) {
			t.Errorf("%s still says %q, which no longer holds: every lane of a node with a token needs the fleet bearer", file, phrase)
		}
	}
}

// Each row is run over the sentence it was taken from, standing in for the file as it was before: the lint must find it.
func TestEveryStaleClaimRowFindsTheSentenceItWasTakenFrom(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range staleFleetDoorClaims {
		key := c.file + "\x00" + c.phrase
		if seen[key] {
			t.Errorf("%s: the phrase %q is listed twice", c.file, c.phrase)
			continue
		}
		seen[key] = true
		if c.was == "" {
			t.Errorf("%s: the row for %q carries no sentence it was taken from, so nothing shows that it ever matched", c.file, c.phrase)
			continue
		}
		reverted := "a line that says nothing about the doors\n" + c.was + "\na last line that says nothing either\n"
		if !slices.Contains(staleClaimsIn(c.file, reverted), c.phrase) {
			t.Errorf("%s: the row for %q does not find the sentence it was taken from: %q", c.file, c.phrase, c.was)
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
