package mcpserver

// exit_when_stale_test.go: the one check of exit-when-stale (exit_when_stale.go). The behaviour that matters shows only at a
// real deploy, so this drives a served session over an in-memory transport with the real middleware chain, the real watcher
// and a real installed file that is replaced, with the intervals shortened.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

func TestAnIdleStaleServerExitsOnlyForAListedClient(t *testing.T) {
	prevEvery, prevSettle, prevUptime, prevSlack := exitStaleEvery, exitStaleSettle, exitStaleMinUptime, exitStaleSlack
	t.Cleanup(func() {
		exitStaleEvery, exitStaleSettle, exitStaleMinUptime, exitStaleSlack = prevEvery, prevSettle, prevUptime, prevSlack
	})
	exitStaleEvery, exitStaleSettle, exitStaleMinUptime, exitStaleSlack = 20*time.Millisecond, 150*time.Millisecond, 0, 20*time.Millisecond

	// serveOn serves s over an in-memory transport to a client of the given name and returns what Run returned, the client
	// session and the path of the installed file.
	type served struct {
		done chan error
		cs   *mcp.ClientSession
		path string
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	serveOn := func(client string) (*Server, served) {
		s := delegateTestServer(t, func(ctx context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
			started <- struct{}{}
			<-release
			return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "done",
				Structured: json.RawMessage(`{"summary":"x"}`), StopReason: "done"}, nil
		})
		path := installOn(t, s, "0.183.0")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		clientT, serverT := mcp.NewInMemoryTransports()
		done := make(chan error, 1)
		go func() { done <- s.serve(ctx, "test", serverT) }()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: client, Version: "2.1.330.0"}, nil).Connect(ctx, clientT, nil)
		if err != nil {
			t.Fatalf("client connect: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return s, served{done, cs, path}
	}
	// replace puts other bytes at the installed path with a modification time well past exitStaleMinAge, as a finished deploy.
	replace := func(path string) {
		if err := os.WriteFile(path, imageBody("0.184.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Minute)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	stays := func(sv served, d time.Duration, why string) {
		t.Helper()
		select {
		case err := <-sv.done:
			t.Fatalf("Run returned (%v) %s", err, why)
		case <-time.After(d):
		}
	}

	// A listed client (compared case-insensitively), a replaced image and a call in flight: the server stays; once the call is
	// answered and the quiet window has passed, Run returns nil, and the call's own answer reached the client first.
	s, sv := serveOn("Claude-Code")
	replace(sv.path)
	answered := make(chan error, 1)
	go func() {
		res, err := sv.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "agent_delegate", Arguments: json.RawMessage(staleDelegateArgs)})
		if err == nil && (res == nil || len(res.Content) == 0) {
			err = context.DeadlineExceeded
		}
		answered <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the call never reached the door")
	}
	if got := s.exitWhenStaleState(); got != "armed" {
		t.Fatalf("exit_when_stale = %q once a listed client has spoken, want armed", got)
	}
	stays(sv, 600*time.Millisecond, "while a tools/call was in flight")
	close(release)
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the call in flight was not answered: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the call in flight was never answered")
	}
	select {
	case err := <-sv.done:
		if err != nil {
			t.Fatalf("Run returned %v on a self-exit, want nil (exit code 0)", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an armed, idle server whose installed file was replaced did not return from Run")
	}

	// A client that is not on the list keeps today's behaviour: the same replaced image, an idle server, and it stays.
	s, sv = serveOn("some-other-client")
	if _, err := sv.cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := s.exitWhenStaleState(); got == "armed" || !strings.Contains(got, "some-other-client") {
		t.Fatalf("exit_when_stale = %q for a client that is not on the list, want off, naming the client", got)
	}
	replace(sv.path)
	stays(sv, 800*time.Millisecond, "for a client that is not on the list")
}
