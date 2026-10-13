package mcpserver

// results[].node_reported (0.185.0) is stamped in one place on this side, contractOnFleetAt, and every door that dispatches one
// contract through it publishes the key when the ids differ: agent_run and offload_ask with a route, the named-seat door of
// agent_run(model: <seat>), and offload_review_diff's fence fallthrough (withReviewExtra folds the whole map). The first draft
// of the docs listed the first two only. The behaviour is pinned below; the docs are pinned against the surfaces that publish it.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The review lane publishes where a fenced review ran. Its result names the node the placement used and, beside it, the id
// the node's own result carried, only when the two differ.
func TestTheReviewLanePublishesNodeReportedOnlyWhenTheIdsDiffer(t *testing.T) {
	for _, tc := range []struct {
		name, node, stamp, wantReported string
	}{
		{"the node's result names it by another id", "node-id-aaaa", "host-bbbb", "host-bbbb"},
		{"the ids agree", "node-id-aaaa", "node-id-aaaa", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pinServer(t, nil)
			s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, _ []core.AgentContract, _ string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
				return []delegate.PlacedResult{{Node: tc.node, Seat: "seat-b", PlacementReason: "route=remote forced → " + tc.node,
					Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "the seat is busy", Seat: "seat-b", NodeID: tc.stamp}}}, delegate.Summary{Deferred: 1}, nil
			}
			wire, extra, _, _ := s.reviewOnFleet(context.Background(), core.AgentContract{Goal: "review"}, "a lease fences the seat")
			out, _ := reviewPayload(wire, "diff --git a/f b/f", 5, extra)
			if out["node"] != tc.node || out["executed_on"] != "fleet" {
				t.Fatalf("the review must name the node the placement used and say it ran on the fleet (control): %v", out)
			}
			got, present := out["node_reported"]
			if tc.wantReported == "" {
				if present {
					t.Errorf("node_reported = %v where the node's own id adds nothing: the key must be absent", got)
				}
				return
			}
			if got != tc.wantReported {
				t.Errorf("node_reported = %v (present %v), want %q: the id the node's own result carried", got, present, tc.wantReported)
			}
		})
	}
}

// docParagraph returns the blank-line-separated paragraph of the file that contains anchor, whitespace collapsed, and says how
// many paragraphs held it, so a test that finds none (or two) is blind and says so.
func docParagraph(t *testing.T, path, anchor string) (string, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	n := 0
	for _, para := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
		flat := strings.Join(strings.Fields(para), " ")
		if strings.Contains(flat, anchor) {
			found = flat
			n++
		}
	}
	return found, n
}

// Every door that publishes node_reported is named where its response keys are listed. The anchors are the sentences that
// list a door's keys; the first row is the control (it already names node_reported), and each anchor must be found once.
func TestTheDocsNameNodeReportedOnEveryDoorThatPublishesIt(t *testing.T) {
	const mcpDoc = "../../docs/systems/mcp-server.md"
	for _, tc := range []struct{ door, anchor string }{
		{"agent_run and offload_ask with a route (control)", "the response names `node`, `placement`, `seat` and `executed_on`"},
		{"the named-seat door of agent_run", "The response names `node`, `placement`, `seat`, `model` (the seat that ran it), `route` and"},
		{"offload_review_diff's fence fallthrough", "The published result names where it ran"},
	} {
		para, n := docParagraph(t, mcpDoc, tc.anchor)
		if n != 1 {
			t.Errorf("%s: %d paragraph(s) of %s hold %q, want 1: this check is blind", tc.door, n, mcpDoc, tc.anchor)
			continue
		}
		if !strings.Contains(para, "node_reported") {
			t.Errorf("%s: its response keys are listed without node_reported, which it publishes when the ids differ: %q", tc.door, tc.anchor)
		}
	}
	// The CHANGELOG sentence that says where it is stamped names the review lane and the named-seat door as well.
	raw, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(text, "## [Unreleased]")
	if start < 0 {
		t.Fatal("CHANGELOG.md has no [Unreleased] section: this check is blind")
	}
	section := text[start:]
	if end := strings.Index(section[len("## [Unreleased]"):], "\n## ["); end >= 0 {
		section = section[:len("## [Unreleased]")+end]
	}
	rule := ""
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- **The rule.**") {
			rule = line
		}
	}
	if !strings.Contains(rule, "`results[].node_reported`") {
		t.Fatal("the [Unreleased] \"The rule\" bullet no longer describes node_reported: this check is blind")
	}
	for _, door := range []string{"offload_review_diff", "named-seat"} {
		if !strings.Contains(rule, door) {
			t.Errorf("the CHANGELOG's node_reported sentence does not name the %s door, which publishes it", door)
		}
	}
}
