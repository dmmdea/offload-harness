package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAcceptanceRuleSentencesAreKeepOnly: the sentences that teach how to write acceptance are never dropped. A delegation contract
// whose acceptance is a lone nonempty: check, or whose content checks all match the goal text, passes garbage, and the lint that warns
// of it (acceptance_lint) arrives only after the run. agent_delegate's description says what the lint flags and where to anchor a
// check; compact keeps both, the gate treats any such sentence as keep-only, and the dropped list cannot hold one.
func TestAcceptanceRuleSentencesAreKeepOnly(t *testing.T) {
	// The live sentences: found in the full description, kept in the compact one.
	var full, compact *mcp.Tool
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		if tool.Name == "agent_delegate" {
			full = tool
		}
	}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		if tool.Name == "agent_delegate" {
			compact = tool
		}
	}
	if full == nil || compact == nil {
		t.Fatal("agent_delegate is not advertised")
	}
	found := 0
	for _, s := range splitSentences(full.Description)[1:] {
		if !acceptanceRule.MatchString(s) {
			continue
		}
		found++
		if !strings.Contains(compact.Description, s) {
			t.Errorf("compact dropped an acceptance-rule sentence: %.140q", s)
		}
	}
	if found != 2 {
		t.Fatalf("agent_delegate has %d acceptance-rule sentences outside its opening, want 2 (the lint that flags acceptance which verifies less than it looks, and the remedy that anchors a check)", found)
	}
	// The exact sentence the lint is described by: acceptance must test content that is not in the goal, and a lone nonempty: passes garbage.
	for _, pin := range []string{
		"PARROT-PASSABLE (every content check also matches the goal text, so an echoed question passes as verified and the retry never fires)",
		"SHAPE-ONLY (nonempty:/min_items: alone — passes garbage)",
		"fix the acceptance (anchor >=1 contains:/regex: to content that appears only in the docs) before reusing the contract",
	} {
		if !strings.Contains(full.Description, pin) {
			t.Errorf("the full description no longer says %q (reworded?)", pin)
		} else if !strings.Contains(compact.Description, pin) {
			t.Errorf("compact dropped %q", pin)
		}
	}

	// The gate: such a sentence is keep-only, even when it is on the dropped list with a reason.
	const name = "synthetic_acceptance_tool"
	const rule = "Acceptance must test content that is not in the goal, because a lone nonempty: check passes garbage."
	synth := &mcp.Tool{Name: name, Description: "Place a contract on the fleet for the caller here. " + rule}
	row := droppedSentence{Tools: []string{name}, Sentence: rule, Class: dropResult, Reason: "a reason of enough words to be explicit, which must not matter for this sentence"}
	r := gateTool(synth, compactTool(synth), map[string]droppedSentence{rule: row})
	if len(r.problems) != 1 || !strings.Contains(r.problems[0], "keep-only") {
		t.Fatalf("an acceptance-rule sentence dropped with a reason was not reported as keep-only: %v", r.problems)
	}
	compactKeepByTool[name] = map[string]bool{rule: true}
	t.Cleanup(func() { delete(compactKeepByTool, name) })
	if r := gateTool(synth, compactTool(synth), nil); len(r.problems) != 0 || r.kept != 1 {
		t.Fatalf("a kept acceptance-rule sentence was reported: %+v", r)
	}

	// The list: a row for such a sentence is a defect, and the live list has none.
	if got := checkDroppedRows([]droppedSentence{row}); len(got) != 1 || !strings.Contains(got[0], "keep-only") {
		t.Errorf("an acceptance-rule row on the dropped list gave %v, want one keep-only defect", got)
	}
	for _, d := range compactDropped {
		if acceptanceRule.MatchString(d.Sentence) {
			t.Errorf("the dropped list holds an acceptance-rule sentence: %.140q", d.Sentence)
		}
	}
}
