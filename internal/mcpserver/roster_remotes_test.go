package mcpserver

// agent_delegate's `remotes` may only narrow the configured fleet (the diagnosis' R2-F08, the operator's
// standing rule that a box never delegates to the standalone machine).
//
// A model-named list REPLACED delegate_remotes after one check, the shape of each URL (netguard
// TailnetURL), so any tailnet host passed and was dialled with the fleet's bearer on its health read, and
// keeping the standalone box out was a habit. The door now refuses a node that delegate_remotes does not
// list, before any contract is prepared and before anything is dialled, and says in the tool's own schema
// that the list narrows and replaces.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

func neverDialled(t *testing.T) func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return noLocalRun(t)
}

// TestAgentDelegateRefusesARemoteOutsideTheConfiguredFleet: the standalone box stands in as a node the
// operator never listed. It is named by the call; it must be refused as a config mistake, naming it and
// the roster, with nothing dispatched to it or to the member.
func TestAgentDelegateRefusesARemoteOutsideTheConfiguredFleet(t *testing.T) {
	member, memberURL := newCaptureNode(t)
	outsider, outsiderURL := newCaptureNode(t)
	s := delegateTestServer(t, neverDialled(t), memberURL)

	res, err := s.handleAgentDelegate(context.Background(), callReq(remoteDelegateArgs(outsiderURL, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || m["defer_class"] != core.DeferClassConfig {
		t.Fatalf("result = %v, want a config-class defer", m)
	}
	for _, want := range []string{outsiderURL, "not in this box's delegate_remotes", memberURL, "can only narrow the configured fleet, never add a node"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to contain %q", reason, want)
		}
	}
	if len(outsider.dispatches()) != 0 || len(member.dispatches()) != 0 {
		t.Fatalf("dispatches: outsider %d, member %d, want none: the refusal comes before any placement", len(outsider.dispatches()), len(member.dispatches()))
	}
}

// TestAgentDelegateWithAListThatNarrowsTheFleetRunsThere: naming a configured node is the one thing the
// argument is for.
func TestAgentDelegateWithAListThatNarrowsTheFleetRunsThere(t *testing.T) {
	member, memberURL := newCaptureNode(t)
	other, otherURL := newCaptureNode(t)
	s := delegateTestServer(t, neverDialled(t), memberURL, otherURL)

	res, err := s.handleAgentDelegate(context.Background(), callReq(remoteDelegateArgs(memberURL, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeResult(t, res); m["deferred"] == true {
		t.Fatalf("a list naming a configured node was refused: %v", m)
	}
	if len(member.dispatches()) != 1 || len(other.dispatches()) != 0 {
		t.Fatalf("dispatches: member %d, other %d, want the call confined to the node it named", len(member.dispatches()), len(other.dispatches()))
	}
}

// TestAgentDelegateOnABoxWithNoRosterAcceptsNoRemotes: a box that configured no fleet cannot be told one
// by a call.
func TestAgentDelegateOnABoxWithNoRosterAcceptsNoRemotes(t *testing.T) {
	node, url := newCaptureNode(t)
	s := delegateTestServer(t, neverDialled(t)) // no roster

	res, err := s.handleAgentDelegate(context.Background(), callReq(remoteDelegateArgs(url, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	if reason, _ := m["reason"].(string); m["deferred"] != true || !strings.Contains(reason, "has no delegate_remotes") {
		t.Fatalf("result = %v, want a refusal that says the box has no delegate_remotes", m)
	}
	if len(node.dispatches()) != 0 {
		t.Fatalf("the node was dispatched %d time(s) on a box with no roster", len(node.dispatches()))
	}
}

// TestAgentDelegateSchemaSaysRemotesNarrowAndReplace: tools/list is what a model reads to choose its
// arguments. The `remotes` description must say the three facts a caller needs: the list narrows (every
// entry must be configured), a URL outside the roster is refused, and a list REPLACES the roster.
func TestAgentDelegateSchemaSaysRemotesNarrowAndReplace(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	var description string
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("schema not JSON: %v", err)
		}
		description = schema.Properties["remotes"].Description
	}
	if description == "" {
		t.Fatal("agent_delegate has no remotes description (is it registered?)")
	}
	for _, want := range []string{"each of which must already be in this server's delegate_remotes", "never add a node", "refused and never dialled", "REPLACES the roster", "Omit it to use every configured node"} {
		if !strings.Contains(description, want) {
			t.Errorf("remotes description = %q, want it to say %q", description, want)
		}
	}
}

// TestAgentDelegateTellsTheEngineTheListCameFromAModel pins the engine half of the guard at the one place
// the door builds its options: the early check above refuses first, so the engine's own refusal of an
// outsider (RunOptions.RosterOnly) would otherwise never be reached by any test of the door, and
// dropping the field would go unseen.
func TestAgentDelegateTellsTheEngineTheListCameFromAModel(t *testing.T) {
	s := delegateTestServer(t, neverDialled(t))
	opts := s.agentDelegateOptions(1, time.Time{}, nil)
	if !opts.RosterOnly {
		t.Fatal("agent_delegate's RunOptions leave RosterOnly false: a model-named remotes list could then reach the engine unchecked")
	}
	if opts.Priority != 1 || opts.Tenant != s.tenant || opts.Quarantine != s.quarantine || opts.Rescue == nil {
		t.Fatalf("options = %+v, want the call's priority, the session's tenant, quarantine and rescue carried as before", opts)
	}
}
