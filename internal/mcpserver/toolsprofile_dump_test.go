package mcpserver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// wireCapture records the result bytes of every JSON-RPC response the server writes, before any client decodes them: the
// client side of the in-memory transport would re-marshal the tools (alphabetical keys, schema order lost), the server's own
// bytes are what a stdio client receives.
type wireCapture struct {
	mcp.Transport
	mu      sync.Mutex
	results [][]byte
}

type wireCaptureConn struct {
	mcp.Connection
	c *wireCapture
}

func (w *wireCapture) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := w.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &wireCaptureConn{Connection: conn, c: w}, nil
}

func (w *wireCaptureConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if resp, ok := msg.(*jsonrpc.Response); ok && len(resp.Result) > 0 {
		w.c.mu.Lock()
		w.c.results = append(w.c.results, append([]byte(nil), resp.Result...))
		w.c.mu.Unlock()
	}
	return w.Connection.Write(ctx, msg)
}

// listToolsWire answers a tools/list over an in-memory transport and returns the result exactly as the server wrote it.
func listToolsWire(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	capture := &wireCapture{Transport: serverT}
	ctx := context.Background()
	ss, err := srv.Connect(ctx, capture, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "dump", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	for _, r := range capture.results {
		if bytes.Contains(r, []byte(`"tools":[`)) {
			return r
		}
	}
	t.Fatal("no tools/list result seen on the server's side of the transport")
	return nil
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
	if p := os.Getenv("OFFLOAD_TOOLS_DUMP_PROFILE"); p != "" {
		cfg.ToolsSchemaProfile = p
	}
	// OFFLOAD_TOOLS_DUMP_SURFACE picks a larger surface than the default box: "full-surface" is the 45-tool one the review gate
	// reads (the browse lane and the three accelerators on), "fleet-surface" the same with the accelerators as fleet accelerators.
	switch os.Getenv("OFFLOAD_TOOLS_DUMP_SURFACE") {
	case "":
	case "full-surface":
		cfg = fullSurfaceCfg(cfg.ToolsSchemaProfile)
	case "fleet-surface":
		cfg = fleetSurfaceCfg(cfg.ToolsSchemaProfile)
	default:
		t.Fatalf("OFFLOAD_TOOLS_DUMP_SURFACE = %q: want full-surface or fleet-surface", os.Getenv("OFFLOAD_TOOLS_DUMP_SURFACE"))
	}
	label := os.Getenv("OFFLOAD_TOOLS_DUMP_LABEL")
	if label == "" {
		label = "tools-list"
	}
	if err := os.WriteFile(filepath.Join(dir, label+".json"), listToolsWire(t, cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}
