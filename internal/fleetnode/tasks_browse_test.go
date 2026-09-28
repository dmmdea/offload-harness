package fleetnode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

const browsePayload = `{"schema_version":1,"goal":"read the dashboard","allow_browse":true,"browse_hosts":["example.com"],"output_schema":` + agentSchemaJSON + `}`

func withBrowseLane(c config.Config) config.Config {
	c.BrowsePython, c.BrowseScript, c.BrowseDecisionURL = "py", "runner.py", "http://127.0.0.1:18720/v1/systemone"
	return c
}

// The browse door (ADR 0060) is refused at ACK on a node that has not opted in or
// has no configured lane, exactly like the write door: a 400 the delegator can act
// on, never a browser driven by a contract the node's operator did not allow.
func TestBuildRequestAgentRefusesTheBrowseDoorWithoutOptIn(t *testing.T) {
	for name, mk := range map[string]func() config.Config{
		"not opted in": func() config.Config { return withBrowseLane(agentNodeCfg(t)) },
		"opted in, no lane": func() config.Config {
			c := agentNodeCfg(t)
			c.AgentAllowBrowse = true
			return c
		},
		"neither": func() config.Config { return agentNodeCfg(t) },
	} {
		_, cleanup, err := BuildRequest(context.Background(), mk(), true, "agent", json.RawMessage(browsePayload))
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "agent_allow_browse") {
			t.Errorf("%s: want an ACK refusal naming agent_allow_browse, got %v", name, err)
		}
	}
}

// The other half: the gate is the CONFIG, not the presence of allow_browse.
func TestBuildRequestAgentAcceptsTheBrowseDoorWhenOptedIn(t *testing.T) {
	cfg := withBrowseLane(agentNodeCfg(t))
	cfg.AgentAllowBrowse = true
	_, cleanup, err := BuildRequest(context.Background(), cfg, true, "agent", json.RawMessage(browsePayload))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("an opted-in node with a configured lane refused a browse contract: %v", err)
	}
}
