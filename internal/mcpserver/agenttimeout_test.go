package mcpserver

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The agent-run budget chain: explicit per-call timeout > tier-seeded
// agent_timeout_sec > the built-in 180s. Pinned because a silent inversion
// either ignores the caller or re-arms the 180s timeout machine the config
// seat exists to cure (roast finding, 2026-08-02).
func TestAgentTimeoutPrecedence(t *testing.T) {
	cfg := config.Config{AgentTimeoutSec: 600}
	if got := agentTimeout(90, cfg); got != 90*time.Second {
		t.Fatalf("explicit per-call timeout must win, got %v", got)
	}
	if got := agentTimeout(0, cfg); got != 600*time.Second {
		t.Fatalf("tier-seeded agent_timeout_sec must be used when no explicit timeout, got %v", got)
	}
	if got := agentTimeout(0, config.Config{}); got != 180*time.Second {
		t.Fatalf("unset everything must fall back to 180s, got %v", got)
	}
}
