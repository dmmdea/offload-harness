package mcpserver

// The agent_delegate tool description is the contract a calling model reads. results[].ran_on (D-32) is the closed
// value that says where a subtask ran, and the opencode plugin reads it first; a description that lists the result keys
// without it sends a reader back to the word "local" in placement, the misreading ran_on exists to end (10 of the 214
// remote rows of one delegation log said "local seat busy" and read as local).

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestAgentDelegateDescriptionListsRanOnAndSaysWhatItMeans(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" {
			continue
		}
		if !strings.Contains(tool.Description, "placement,ran_on,job_id") {
			t.Errorf("the description's result keys do not list ran_on beside placement")
		}
		for _, want := range []string{`ran_on says where a subtask ran`, `"local"`, `"node"`, `absent when no seat ran it`} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("the description does not say %s about ran_on", want)
			}
		}
		return
	}
	t.Fatal("agent_delegate is not advertised with agent_delegation_enabled on")
}
