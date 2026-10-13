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

// results[].node is the id the placement used (the node_id /fleet/health publishes); results[].node_reported is the id the
// node stamped on its own result, present only when it differs. A description that lists the keys and says nothing of what
// they name leaves a reader to guess which of two ids to match a node by.
func TestAgentDelegateDescriptionSaysWhatNodeAndNodeReportedMean(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" {
			continue
		}
		for _, want := range []string{
			`{node,node_reported,seat,placement,ran_on,job_id`, // listed right after node, and placement,ran_on,job_id stays together
			`results[].node is the fleet node_id the node publishes in /fleet/health`,
			`the name results[].placement uses`,
			`results[].node_reported appears only when the node's own result named itself by another id`,
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("the description does not say %q", want)
			}
		}
		return
	}
	t.Fatal("agent_delegate is not advertised with agent_delegation_enabled on")
}

// results[].node_reported has two sources on a node of this release's build, and only two: a node on a build before 0.185.0
// (it stamps its hostname where it was launched with another --node-id) and a local run attributed to another box's engine
// (register C-58: the runner stamps its own box, the placement names the engine's). A node on this build that was launched
// under another name stamps the id it advertises, so "a node launched under another name" describes a case that cannot
// differ. And results[].node is the id /fleet/health publishes only for a run on a fleet node the delegator dispatched to: a
// queue answer carries its claimant's stamp, and a local run carries a box's own name.
func TestAgentDelegateDescriptionNamesTheCasesNodeAndNodeReportedActuallyHave(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" {
			continue
		}
		// control: the sentences this one qualifies are still there, so the absences below mean something
		for _, keep := range []string{
			`results[].node is the fleet node_id the node publishes in /fleet/health`,
			`results[].node_reported appears only when the node's own result named itself by another id`,
		} {
			if !strings.Contains(tool.Description, keep) {
				t.Fatalf("the description lost %q: this test's other assertions would pass for the wrong reason", keep)
			}
		}
		for _, want := range []string{
			`for a run on a fleet node this delegator dispatched`,
			`a route:"queue" answer carries the id its claimant stamped`,
			`a run on this box's own seat (ran_on "local") carries this box's own name, or the box whose engine ran it`,
			`a node on a build before 0.185.0`,
			`a local run attributed to another box's engine`,
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("the description does not say %q", want)
			}
		}
		if strings.Contains(tool.Description, `a node launched under another name`) {
			t.Error("the description still names \"a node launched under another name\" as a source of node_reported: a node on this build stamps the id it advertises, so that case cannot differ")
		}
		return
	}
	t.Fatal("agent_delegate is not advertised with agent_delegation_enabled on")
}
