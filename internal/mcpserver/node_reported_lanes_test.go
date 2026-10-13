package mcpserver

// results[].node_reported (0.185.0) is stamped in one place on this side, contractOnFleetAt, and every door that dispatches one
// contract through it publishes the key when the ids differ: agent_run and offload_ask with a route, the named-seat door of
// agent_run(model: <seat>), and offload_review_diff's fence fallthrough (withReviewExtra folds the whole map). The first draft
// of the docs listed the first two only. The behaviour is pinned below; the docs are pinned against the surfaces that publish it.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// The tool descriptions a calling model reads list the response keys of the fleet door too. agent_run (its description and its
// route text) and offload_ask (its route text) used to list "node, placement and seat" and stop, while contractOnFleetAt also
// publishes node_reported when the node's own result named itself by another id. The key is listed with its condition, so a
// reader neither misses it nor expects it on every row.
func TestTheRouteDescriptionsOfAgentRunAndAskListNodeReportedWithItsCondition(t *testing.T) {
	const want = "node_reported only when the node's own result named itself by another id"
	routeText := func(tool *mcp.Tool) string {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("schema not JSON: %v", err)
		}
		return schema.Properties["route"].Description
	}
	seen := map[string]bool{}
	for _, tool := range listTools(t, config.Default()) {
		var texts map[string]string
		switch tool.Name {
		case "agent_run":
			texts = map[string]string{"agent_run description": tool.Description, "agent_run route text": routeText(tool)}
		case "offload_ask":
			texts = map[string]string{"offload_ask route text": routeText(tool)}
		default:
			continue
		}
		for label, text := range texts {
			seen[label] = true
			if !strings.Contains(text, "placement") {
				t.Errorf("%s no longer lists the fleet door's response keys: this check is blind", label)
				continue
			}
			if !strings.Contains(text, want) {
				t.Errorf("%s lists the fleet door's response keys without %q", label, want)
			}
		}
	}
	for _, label := range []string{"agent_run description", "agent_run route text", "offload_ask route text"} {
		if !seen[label] {
			t.Errorf("%s was not found on tools/list: this check is blind", label)
		}
	}
}

// The operator guide's account of the named-seat door lists the same keys and carries the same condition.
func TestTheOperatorGuideListsNodeReportedOnTheNamedSeatDoor(t *testing.T) {
	const guide = "../../docs/OPERATOR-GUIDE.md"
	para, n := docParagraph(t, guide, "**A seat only the fleet runs (register D-125).**")
	if n != 1 {
		t.Fatalf("%d paragraph(s) of %s hold the named-seat door's account, want 1: this check is blind", n, guide)
	}
	if !strings.Contains(para, "`executed_on`") {
		t.Fatal("the named-seat paragraph no longer lists its response keys: this check is blind")
	}
	if !strings.Contains(para, "`node_reported` only when the node's own result named itself by another id") {
		t.Errorf("the operator guide lists the named-seat door's response keys without `node_reported` and its condition: %q", para)
	}
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
	// The bullet is found wherever it sits (under [Unreleased] before the release bump, under the release's own heading
	// after it), so the version bump cannot blind this check: the first "The rule." bullet that describes node_reported.
	rule := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- **The rule.**") && strings.Contains(line, "`results[].node_reported`") {
			rule = line
			break
		}
	}
	if rule == "" {
		t.Fatal("no CHANGELOG \"The rule\" bullet describes node_reported: this check is blind")
	}
	for _, door := range []string{"offload_review_diff", "named-seat"} {
		if !strings.Contains(rule, door) {
			t.Errorf("the CHANGELOG's node_reported sentence does not name the %s door, which publishes it", door)
		}
	}
}
