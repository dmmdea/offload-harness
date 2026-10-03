package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func ownersOf(t *testing.T, root string) []string {
	t.Helper()
	dir := gpulease.OwnersDirFor(filepath.Join(root, "gpu", "lease"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// An MCP server is one Claude Code session, and a session cannot be probed by its id. So
// each server writes its own process into the session registry at start and removes it at
// exit (no timer), which is how a lease's owner is told present from gone.
func TestServerRegistersItsSessionAtStartAndRemovesItAtExit(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = root
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.originSession = func() string { return "sess-registry-test" }

	ctx, cancel := context.WithCancel(context.Background())
	t1, _ := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, "test", t1) }()

	var got []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got = ownersOf(t, root); len(got) > 0 {
			break
		}
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "sess-registry-test.") {
		t.Fatalf("the running server must be in the session registry: %v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop")
	}
	if left := ownersOf(t, root); len(left) != 0 {
		t.Fatalf("the registry entry must be removed at exit: %v", left)
	}
}

// A server with no session (a bare shell, a service) registers nothing.
func TestServerWithoutASessionRegistersNothing(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = root
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.originSession = func() string { return "" }
	unreg := s.registerSession()
	defer unreg()
	if got := ownersOf(t, root); len(got) != 0 {
		t.Fatalf("no session, no entry: %v", got)
	}
}
