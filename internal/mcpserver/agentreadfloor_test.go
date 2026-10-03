package mcpserver

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// TestAgentRunPassesTheReadFloor (SF-07 review S8): the MCP agent_run door hands the
// node's agent_read_floor to Build; an unknown value (a hand-built config past Load's
// validation) reaches Build and defers the run naming the key.
func TestAgentRunPassesTheReadFloor(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1" // unreachable: the roster check proceeds
	cfg.AgentReadFloor = "bogus"
	s := New(pipeline.New(cfg, nil, nil, nil))
	text := browseCallText(t, s.handleAgentRun, map[string]any{"goal": "g"})
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "agent_read_floor") {
		t.Errorf("agent_run did not pass agent_read_floor to Build: %s", text)
	}
}
