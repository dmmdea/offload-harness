package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// composeRouteSentence is the start of the tool-level sentence of offload_compose_video that says where a render goes when this
// machine has no composition lane: to a fleet node, the html or project_dir bundle with it. It holds no prohibition word and none
// of the older data-leaves-the-box words (forward*, travel*, tailnet, sent, cloud), so the review gate skipped it and the
// compact list dropped it with no entry on either list.
const composeRouteSentence = "route places the render: auto (default) runs here when this machine has the composition lane and otherwise on a fleet node"

// TestGateReadsTheComposeVideoRouteSentence: the sentence above is read by the review gate and is classified, not skipped. The
// gate must recognise it as one that may say the caller's data leaves the box, and it must be either in the compact description of
// the tool or on the dropped-with-reason list; the compact list keeps it.
func TestGateReadsTheComposeVideoRouteSentence(t *testing.T) {
	dropped := droppedIndex(t)
	for _, surface := range gateSurfaces() {
		var full, compact *mcp.Tool
		for _, tool := range listTools(t, surface.cfg("full")) {
			if tool.Name == "offload_compose_video" {
				full = tool
			}
		}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			if tool.Name == "offload_compose_video" {
				compact = tool
			}
		}
		if full == nil || compact == nil {
			continue
		}
		var sentence string
		for _, s := range splitSentences(full.Description) {
			if strings.HasPrefix(s, composeRouteSentence) {
				sentence = s
			}
		}
		if sentence == "" {
			t.Fatalf("%s: the compose-video route sentence is not in the full description any more (reworded?)", surface.name)
		}
		if !gateTrigger.MatchString(sentence) {
			t.Errorf("%s: the gate does not classify the sentence that sends a render to a fleet node: %q", surface.name, sentence)
		}
		if !leadGuard.MatchString(sentence) {
			t.Errorf("%s: the lead guard does not protect the sentence that sends a render to a fleet node: %q", surface.name, sentence)
		}
		if r := gateTool(full, compact, dropped[full.Name]); len(r.problems) != 0 {
			t.Errorf("%s: %v", surface.name, r.problems)
		}
		if !strings.Contains(compact.Description, sentence) {
			t.Errorf("%s: compact dropped the sentence that sends a render to a fleet node: %q", surface.name, sentence)
		}
	}
}

// TestGateFiresOnAFleetNodeSentence: a sentence whose only rule is that the call may run on a fleet node, with the caller's files
// going there, is reported until it is kept or dropped with a reason.
func TestGateFiresOnAFleetNodeSentence(t *testing.T) {
	const name = "synthetic_fleetnode_tool"
	const rule = "route places the render: auto runs here when this machine has the lane and otherwise on a fleet node, which receives the project bundle."
	full := &mcp.Tool{Name: name, Description: "Render a clip on the local gadget for the caller. Returns {video}. " + rule}
	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 1 || !strings.Contains(r.problems[0], rule) {
		t.Fatalf("a fleet-node route sentence was not reported exactly once: %v", r.problems)
	}
	if r := gateTool(full, compactTool(full), map[string]string{rule: "reviewed"}); len(r.problems) != 0 {
		t.Fatalf("a dropped-with-reason route sentence was still reported: %v", r.problems)
	}
}
