package mcpserver

import (
	"strings"
	"testing"
)

// TestCompactKeepsAgentRunsRemoteModelPlacement: agent_run's model argument is not only a validation. A model this box does not serve is
// looked up on the fleet and, with no route or route remote, the run goes as ONE contract through the delegator to a node whose seat is
// that model, so the goal and setup_actions leave the box (read_root and judge do not travel). That is a successful remote placement
// the first sentence of the tool does not mention, and compact dropped both the tool-level sentence and the model parameter's own as
// "validation". Each pin is looked up in the full description first, so a rewording fails loudly.
func TestCompactKeepsAgentRunsRemoteModelPlacement(t *testing.T) {
	full, compact := map[string]string{}, map[string]string{}
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		if tool.Name == "agent_run" {
			full["tool"], full["model"] = tool.Description, paramDescription(t, tool, "model")
		}
	}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		if tool.Name == "agent_run" {
			compact["tool"], compact["model"] = tool.Description, paramDescription(t, tool, "model")
		}
	}
	for _, pin := range []struct{ where, phrase string }{
		{"tool", "it runs as ONE contract through the delegator on a node whose agent seat is that model"},
		{"tool", "notes read_root and judge, which do not travel"},
		{"model", "it runs on a node whose agent seat is that model, else it is refused naming where the fleet looked"},
		{"model", "route local never asks the fleet"},
	} {
		if !strings.Contains(full[pin.where], pin.phrase) {
			t.Errorf("agent_run %s: the pinned phrase is not in the full description any more (reworded?): %q", pin.where, pin.phrase)
			continue
		}
		if !strings.Contains(compact[pin.where], pin.phrase) {
			t.Errorf("agent_run %s: compact dropped the remote placement of a model this box does not serve: %q", pin.where, pin.phrase)
		}
	}
}
