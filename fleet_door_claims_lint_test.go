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
	{file: "docs/OPERATOR-GUIDE.md", phrase: "`/fleet/health` ignores the token",
		was: "one `NOT SET` line instead. `/fleet/health` ignores the token, so every other row"},
	{file: "docs/OPERATOR-GUIDE.md", phrase: "endpoints are unauthenticated, so the tailnet is the trust boundary",
		was: "address** for production — the endpoints are unauthenticated, so the tailnet is the trust boundary; port **18811**, **never `0.0.0.0`**):"},
	{file: "docs/FLEET-NODE.md", phrase: "Media lanes carry no auth",
		was: "and ignored (contract-reserved). - **Media lanes carry no auth** — the trusted-network posture"},
	{file: "docs/FLEET-NODE.md", phrase: "Bearer token for the **agent lane only**",
		was: "| `fleet_auth_token` | `\"\"` | Bearer token for the **agent lane only** (agent dispatches + polls of"},
	{file: "docs/FLEET-NODE.md", phrase: "The **media** endpoints are unauthenticated by design",
		was: "guidance (read before exposing anything) The **media** endpoints are unauthenticated by design (matching the dispatcher's posture): anyone"},
	{file: "docs/FLEET-NODE.md", phrase: "### Auth — the one token-gated lane",
		was: "reasoning never crosses the wire. ### Auth — the one token-gated lane - `fleet_auth_token` set → agent"},
	{file: "docs/FLEET-NODE.md", phrase: "deployed tokenless media clients keep working byte-identically",
		was: "never check the token** — deployed tokenless media clients keep working byte-identically (the one media door that"},
	{file: "docs/FLEET-NODE.md", phrase: "A node with no token keeps its legacy lane open",
		was: "any file it can read. A node with no token keeps its legacy lane open. Compatibility: the only external"},
	{file: "docs/FLEET-NODE.md", phrase: "media stays tokenless in v1",
		was: "+ polls of agent-created jobs; media stays tokenless in v1). Same value on every"},
	{file: "docs/FLEET-NODE.md", phrase: "is open to any peer",
		was: "the seventh, because the door is open to any peer and a graph can name"},
	{file: "docs/systems/fleet-node.md", phrase: "exactly two lanes",
		was: "0.116.0) `fleet_auth_token`, when set, bearer-gates exactly two lanes: agent dispatches (and the"},
	{file: "docs/systems/fleet-node.md", phrase: "(a node with none is unchanged)",
		was: "the node has a `fleet_auth_token` (a node with none is unchanged), and both stt lanes,"},
	{file: "docs/systems/fleet-node.md", phrase: "the legacy `stt` when the node has a token",
		was: "types ride the bearer rule, the legacy `stt` when the node has a token), the vision lane -"},
	{file: "docs/systems/fleet-node.md", phrase: "### Auth (v1 scope: the agent lane",
		was: "success and is not counted. ### Auth (v1 scope: the agent lane — joined by the vision"},
	{file: "docs/systems/fleet-node.md", phrase: "so the media lane stays unauthenticated",
		was: "address) is admitted as before, so the media lane stays unauthenticated on a non-loopback bind. Per-node"},
	{file: "docs/systems/fleet-node.md", phrase: "The **media** contract is unauthenticated and assumes a trusted network",
		was: "tailnet grants are separate work. The **media** contract is unauthenticated and assumes a trusted network — in practice a tailnet."},
	{file: "docs/systems/fleet-node.md", phrase: "health — ignores the token",
		was: "dispatch, media job polls, `/fleet/media/*`, health — ignores the token, so already-deployed tokenless media"},
	{file: "docs/systems/fleet-node.md", phrase: "Because media dispatch is tokenless",
		was: "recorded follow-up ([ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)). **Because media dispatch is tokenless, a remote media task"},
	{file: "docs/systems/fleet-node.md", phrase: "gates dispatches to the agent, vision, text and speech-to-text lanes",
		was: "unauthenticated on every node: `fleet_auth_token` gates dispatches to the agent, vision, text and speech-to-text lanes and the polls of the"},
	{file: "docs/flows/fleet-job-lifecycle.md", phrase: "The contract is unauthenticated and assumes a trusted network",
		was: "## Security and privacy notes The contract is unauthenticated and assumes a trusted network. Binding beyond loopback requires"},
	{file: "docs/systems/media-generation.md", phrase: "is open to any peer",
		was: "at the seventh (the door is open to any peer and a graph can name"},
	{file: "internal/mediacap/graphfiles.go", phrase: "is open to any peer",
		was: "MiB), the door that asks is open to any peer on the tailnet, and a"},
	{file: "internal/mediacap/graphfiles_test.go", phrase: "a door open to any peer",
		was: "shows, not by the graph: a door open to any peer takes a graph of thousands"},
	{file: "internal/fleetnode/server.go", phrase: "media polls never see this branch",
		was: "agent dispatch are gated — media polls never see this branch, so the deployed media"},
	{file: "internal/fleetnode/stt_upload_test.go", phrase: "keeps its behaviour (the lane stays open beyond loopback)",
		was: "A node with NO token keeps its behaviour (the lane stays open beyond loopback), so no deployed tokenless"},
	{file: "internal/fleetnode/auth_test.go", phrase: "(media polls must stay open)",
		was: "Agent marker set, want false (media polls must stay open)\") } } agentLaneCfg is"},
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
	oldClaim := regexp.MustCompile(`(?i)tokenless|not token-gated|ignores the bearer|agent or vision job|open to any peer`)
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
