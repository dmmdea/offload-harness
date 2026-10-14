package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// listToolsWire connects a real MCP client to the built server over an in-memory transport and returns the
// tools/list result re-marshalled to the JSON a client receives on the wire: {"tools":[...]}.
func listToolsWire(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "dump", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestToolsListDump is the measurement helper behind the token table in docs/systems/opencode-integration.md:
// with OFFLOAD_TOOLS_DUMP_DIR set it writes the tools/list JSON a client receives, one file per label, so a
// tokenizer can count exactly what the server emits. It asserts nothing and is skipped otherwise.
func TestToolsListDump(t *testing.T) {
	dir := os.Getenv("OFFLOAD_TOOLS_DUMP_DIR")
	if dir == "" {
		t.Skip("set OFFLOAD_TOOLS_DUMP_DIR to write the tools/list JSON for token counting")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The surface the opencode plugin sees: the delegation tools (agent_delegate, offload_research) are in its
	// recon set, so the measured box has agent_delegation_enabled on.
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	label := os.Getenv("OFFLOAD_TOOLS_DUMP_LABEL")
	if label == "" {
		label = "tools-list"
	}
	if err := os.WriteFile(filepath.Join(dir, label+".json"), listToolsWire(t, cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}
