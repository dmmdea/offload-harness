package mcpserver

// agent_rig's description is what the delegating model reads to decide whether to call the rigger and how to read its
// answer, and it publishes the order in which a failed row is put on an axis. The rigger gained axes (capacity,
// reasoning-starved, final-tool-call) and a published precedence that the description did not follow: a caller reading it
// would have filed a full fleet under "timeout" and a forced-final tool call under "budget", the two readings the new axes
// exist to correct. The order is rig.Precedence, so the test reads it from there and cannot go stale a second time.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/rig"
)

func TestAgentRigDescriptionPublishesTheRiggersPrecedenceInOrder(t *testing.T) {
	desc := ""
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "agent_rig" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("agent_rig is not advertised")
	}
	const from, until = "published precedence order (", "and return the triage report"
	i, j := strings.Index(desc, from), strings.Index(desc, until)
	if i < 0 || j < i {
		t.Fatalf("the description no longer states the precedence order between %q and %q", from, until)
	}
	seg := desc[i+len(from) : j]
	at := 0
	for n, axis := range rig.Precedence {
		token := string(axis)
		if n > 0 {
			token = "→ " + token
		}
		k := strings.Index(seg[at:], token)
		if k < 0 {
			t.Fatalf("axis %q is missing from the description's precedence order (or out of order after %q): %s", axis, rig.Precedence[max(n-1, 0)], seg)
		}
		at += k + len(token)
	}
}

// The remedies the description names are the ones the rigger hands back for the new axes.
func TestAgentRigDescriptionNamesTheCapacityLeverAndTheRefusalLine(t *testing.T) {
	desc := ""
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "agent_rig" {
			desc = tool.Description
		}
	}
	for _, want := range []string{"agent_call_deadline_sec", "agent_placement_wait_sec", "refusals"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
}
