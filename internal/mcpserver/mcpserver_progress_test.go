// Progress notifications and the heartbeat of the MCP delegation doors (ADR 0065,
// register C-67 tail).
//
// A long delegation is silent for minutes. The MCP spec lets a server report
// progress on a request whose caller supplied a progress token
// (`_meta.progressToken`), and a client that resets its timeout on progress then
// keeps waiting instead of aborting. Whether the reference client does is UNVERIFIED
// (it is why the whole-call deadline is a hard limit, not this), so the reporter is
// strictly opt-in: nothing is sent unless the request carried a token. These tests
// drive the real tools over an in-memory MCP transport.

package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// progressLog collects the progress notifications a client received.
type progressLog struct {
	mu    sync.Mutex
	items []*mcp.ProgressNotificationParams
}

func (l *progressLog) add(p *mcp.ProgressNotificationParams) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, p)
}

func (l *progressLog) all() []*mcp.ProgressNotificationParams {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*mcp.ProgressNotificationParams(nil), l.items...)
}

// quiesce waits for notifications still in flight to the client. The Go SDK client
// dispatches incoming notifications off the response path, so CallTool can return a
// moment before the last of them reaches the handler — the server has already
// written every one (the reporter's stop() waits for its writes), so this waits for
// delivery, not for anything the server might still send. Quiet means 100 ms
// without a new one, and it gives up after two seconds.
func (l *progressLog) quiesce() {
	last, quiet := -1, 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if n := len(l.all()); n != last {
			last, quiet = n, 0
		} else if quiet++; quiet >= 5 {
			return
		}
	}
}

// callOverMCP calls a tool of s through a real in-memory MCP client/server pair,
// with a progress token when token != nil, and returns what the client received.
func callOverMCP(t *testing.T, s *Server, tool string, args map[string]any, token any) (*mcp.CallToolResult, *progressLog) {
	t.Helper()
	log := &progressLog{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := s.buildServer("test").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) { log.add(req.Params) },
	})
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	if token != nil {
		params.SetProgressToken(token)
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	log.quiesce()
	return res, log
}

// slowThenFast: subtask "fast" finishes at once, "slow" after 300 ms.
func slowThenFast(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
	if strings.Contains(c.Goal, "slow") {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "done", StopReason: "done"}, nil
}

var twoSubtasks = map[string]any{
	"subtasks": []any{map[string]any{"goal": "fast one"}, map[string]any{"goal": "slow one"}},
	"route":    "local",
}

// TestAgentDelegateReportsProgressWhenTheClientAsksForIt: a request with a
// progress token gets notifications on the subtasks' state changes — each subtask
// started and each finished — every one carrying the caller's token and a
// strictly increasing progress value, and none after the call has returned.
func TestAgentDelegateReportsProgressWhenTheClientAsksForIt(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	res, log := callOverMCP(t, s, "agent_delegate", twoSubtasks, "tok-1")
	if res.IsError {
		t.Fatalf("the call itself failed: %+v", res)
	}

	got := log.all()
	if len(got) < 5 {
		t.Fatalf("got %d progress notification(s), want an opening one plus a start and a finish per subtask (5)", len(got))
	}
	var started, finished int
	last := 0.0
	for i, p := range got {
		if p.ProgressToken != "tok-1" {
			t.Fatalf("notification %d carries token %v, want the caller's tok-1", i, p.ProgressToken)
		}
		if p.Progress <= last {
			t.Fatalf("notification %d progress %v does not increase past %v: the spec asks for a strictly increasing value", i, p.Progress, last)
		}
		last = p.Progress
		if strings.Contains(p.Message, "started") {
			started++
		}
		if strings.Contains(p.Message, "finished") {
			finished++
		}
	}
	if started != 2 || finished != 2 {
		t.Fatalf("%d start and %d finish notification(s), want 2 and 2: %v", started, finished, progressMessages(got))
	}
	if lastMsg := got[len(got)-1].Message; !strings.Contains(lastMsg, "2 of 2") {
		t.Fatalf("the last notification %q should report both subtasks done", lastMsg)
	}

	// Nothing arrives after the call has returned: the token is dead by then.
	n := len(log.all())
	time.Sleep(150 * time.Millisecond)
	if after := len(log.all()); after != n {
		t.Fatalf("%d notification(s) arrived after the call returned", after-n)
	}
}

// TestAgentDelegateSendsNoProgressWithoutAToken: the reporter is opt-in. A request
// with no progress token gets exactly what it always got — no notifications.
func TestAgentDelegateSendsNoProgressWithoutAToken(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	_, log := callOverMCP(t, s, "agent_delegate", twoSubtasks, nil)
	time.Sleep(100 * time.Millisecond)
	if got := log.all(); len(got) != 0 {
		t.Fatalf("%d progress notification(s) without a progress token: %v", len(got), progressMessages(got))
	}
}

// TestProgressHeartbeatKeepsTalkingWhileNothingChanges: a subtask that runs for a
// while produces no state change, and the client's timer needs a sign of life — so
// the reporter repeats itself on the heartbeat, still strictly increasing, saying
// how far along the call is and how long until its deadline.
func TestProgressHeartbeatKeepsTalkingWhileNothingChanges(t *testing.T) {
	old := progressHeartbeat
	progressHeartbeat = 40 * time.Millisecond
	t.Cleanup(func() { progressHeartbeat = old })

	s := deadlineServer(t, 30, slowThenFast)
	_, log := callOverMCP(t, s, "agent_delegate", map[string]any{
		"subtasks": []any{map[string]any{"goal": "slow one"}}, "route": "local"}, 7)

	var beats int
	last := 0.0
	finished := false
	for _, p := range log.all() {
		if p.Progress <= last {
			t.Fatalf("progress %v does not increase past %v", p.Progress, last)
		}
		last = p.Progress
		if strings.Contains(p.Message, "finished") {
			finished = true
		}
		if strings.Contains(p.Message, "still working") {
			beats++
			// A heartbeat that lands after the subtask finished says so ("1 of 1").
			if (!finished && !strings.Contains(p.Message, "0 of 1")) || !strings.Contains(p.Message, "deadline") {
				t.Fatalf("heartbeat %q should say how far along the call is and when its deadline is", p.Message)
			}
		}
	}
	if beats < 3 {
		t.Fatalf("%d heartbeat(s) during a 300ms run at a 40ms cadence, want at least 3: %v", beats, progressMessages(log.all()))
	}
}

// TestProgressCountsTheCallDeadlineDefersAsDone: a subtask the call deadline cuts —
// or one that never got a run slot — is a finished subtask like any other. Its
// finish is reported (with the defer class), so the client's last word is
// "6 of 6 done", not a call that seems to have stopped halfway.
func TestProgressCountsTheCallDeadlineDefersAsDone(t *testing.T) {
	var cancelled atomic.Int64
	s := deadlineServer(t, 1, slowSeat(&cancelled))
	subtasks := make([]any, 6) // four run slots: two of these never start
	for i := range subtasks {
		subtasks[i] = map[string]any{"goal": "slow one"}
	}
	_, log := callOverMCP(t, s, "agent_delegate", map[string]any{"subtasks": subtasks, "route": "local"}, "dl-1")

	got := log.all()
	if len(got) == 0 {
		t.Fatal("no progress notifications")
	}
	var never int
	for _, p := range got {
		if strings.Contains(p.Message, "finished (deferred (budget))") {
			never++
		}
	}
	if never != 6 {
		t.Fatalf("%d finish notification(s) for deferred subtasks, want 6 (four cut, two never started): %v", never, progressMessages(got))
	}
	if last := got[len(got)-1].Message; !strings.Contains(last, "6 of 6 done") {
		t.Fatalf("the last notification %q should report all six done: %v", last, progressMessages(got))
	}
}

// TestResearchReportsProgressAcrossChunks: nine pages run as two chunks; the
// notifications count against the WHOLE call (n of 9), not against each chunk.
func TestResearchReportsProgressAcrossChunks(t *testing.T) {
	s := researchServer(t, nil)
	urls := make([]any, 9)
	for i := range urls {
		urls[i] = "https://docs.example/p" + string(rune('a'+i))
	}
	res, log := callOverMCP(t, s, "offload_research", map[string]any{
		"goal": "digest the page", "urls": urls, "route": "local",
		"output_schema": map[string]any{"properties": map[string]any{"summary": map[string]any{"type": "string"}}}}, "r-1")
	if res.IsError {
		t.Fatalf("research call failed: %+v", res)
	}
	var sawNineth bool
	for _, p := range log.all() {
		if strings.Contains(p.Message, "9 of 9") {
			sawNineth = true
		}
		if strings.Contains(p.Message, "of 8") {
			t.Fatalf("notification %q counts against a chunk of 8, not the whole call of 9", p.Message)
		}
	}
	if !sawNineth {
		t.Fatalf("no notification reports 9 of 9 pages done: %v", progressMessages(log.all()))
	}
}

// progressMessages is the message text of each notification, for failure output.
func progressMessages(ps []*mcp.ProgressNotificationParams) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Message
	}
	return out
}

// TestProgressReporterFlushesWhatWasQueuedWhenTheCallEnds: the last events of a
// call — the finishes the engine produces right before it returns, and every one
// the call deadline itself publishes — are queued a moment before the handler
// stops the reporter. They must still reach the client, in order, or its final
// word is a stale "5 of 8 done" for a call that ended with eight. A burst of
// finishes followed at once by stop() proves it: all of them, and nothing after.
func TestProgressReporterFlushesWhatWasQueuedWhenTheCallEnds(t *testing.T) {
	const burst = 20
	srv := mcp.NewServer(&mcp.Implementation{Name: "burst", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "burst", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			opts := &delegate.RunOptions{}
			stop := (&Server{}).startProgress(ctx, req, opts, burst, time.Time{})
			for i := 0; i < burst; i++ {
				opts.OnProgress(delegate.ProgressEvent{Kind: "finished", Index: i, Done: i + 1, Total: burst, Outcome: "succeeded"})
			}
			stop()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})

	log := &progressLog{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) { log.add(req.Params) },
	}).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	params := &mcp.CallToolParams{Name: "burst"}
	params.SetProgressToken("b-1")
	if _, err := cs.CallTool(ctx, params); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	log.quiesce()

	got := log.all()
	if len(got) != burst+1 { // the opening notification plus one per finish
		t.Fatalf("got %d notification(s), want %d (the opening one and all %d finishes): %v", len(got), burst+1, burst, progressMessages(got))
	}
	if last := got[len(got)-1].Message; !strings.Contains(last, "20 of 20 done") {
		t.Fatalf("the last notification %q should report all twenty done", last)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Progress <= got[i-1].Progress {
			t.Fatalf("notification %d progress %v does not increase past %v", i, got[i].Progress, got[i-1].Progress)
		}
	}
}
