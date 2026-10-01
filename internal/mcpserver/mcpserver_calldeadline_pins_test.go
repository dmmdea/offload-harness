package mcpserver

// Pins for what the two delegation doors hand the engine and for the progress
// reporter's lifetime (ADR 0065). A mutation battery over the whole-call deadline
// found each of these unpinned: the door tests set `agent_call_deadline_sec`
// explicitly and never read the dispatch envelope, so a one-line regression in the
// wiring left the suite green. Every test here passes on the code as it stands and
// fails when the line it names is undone.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
)

// TestDoorAppliesTheBuiltInCallDeadline: a deployment that never sets
// agent_call_deadline_sec (config.example.json ships the key unset) must get the
// built-in deadline at BOTH doors, and only a negative value switches it off. Every
// other door test sets the key, so a door that read the raw key (0 = no deadline) went
// unnoticed — and with it the 2,103 s call this deadline exists to end.
func TestDoorAppliesTheBuiltInCallDeadline(t *testing.T) {
	entered := time.Now()
	if got, want := delegateTestServer(t, nil).callDeadlineAt(entered), entered.Add(time.Duration(config.DefaultCallDeadlineSec)*time.Second); !got.Equal(want) {
		t.Fatalf("callDeadlineAt with the key unset = %v, want %v (the built-in %d s): the default deadline is not applied", got, want, config.DefaultCallDeadlineSec)
	}
	if got := deadlineServer(t, -1, nil).callDeadlineAt(entered); !got.IsZero() {
		t.Fatalf("callDeadlineAt with a negative key = %v, want no deadline", got)
	}
	if got := deadlineServer(t, 7, nil).callDeadlineAt(entered); !got.Equal(entered.Add(7 * time.Second)) {
		t.Fatalf("callDeadlineAt with 7 = %v, want entry + 7 s", got)
	}
}

var deadlineNoteRe = regexp.MustCompile(`call deadline in (?:(\d+)m)?(\d+)s`)

// deadlineSecondsIn reads the seconds-to-deadline a progress opening notification
// announces; ok=false for "no call deadline".
func deadlineSecondsIn(msg string) (sec int, ok bool) {
	m := deadlineNoteRe.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	mins := 0
	if m[1] != "" {
		mins = atoiOrZero(m[1])
	}
	return mins*60 + atoiOrZero(m[2]), true
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// TestAgentDelegateReportsTheDeadlineItRunsUnder: the same fact through the real door
// and a real MCP client. The opening progress notification says how long the call has —
// the built-in 25 minutes for an unset key, "no call deadline" for a negative one, and
// the configured seconds otherwise — so a door that dropped the deadline on the way to
// the engine, or read the wrong key, is heard by the one client that asked.
func TestAgentDelegateReportsTheDeadlineItRunsUnder(t *testing.T) {
	one := map[string]any{"subtasks": []any{map[string]any{"goal": "fast one"}}, "route": "local"}
	opening := func(s *Server) string {
		t.Helper()
		_, log := callOverMCP(t, s, "agent_delegate", one, "dl-note")
		all := log.all()
		if len(all) == 0 {
			t.Fatal("no progress notification")
		}
		return all[0].Message
	}
	def := int(config.DefaultCallDeadlineSec)
	if sec, ok := deadlineSecondsIn(opening(delegateTestServer(t, slowThenFast))); !ok || sec < def-5 || sec > def {
		t.Fatalf("an unset key announced %d s (ok %v), want the built-in %d s", sec, ok, def)
	}
	if _, ok := deadlineSecondsIn(opening(deadlineServer(t, -1, slowThenFast))); ok {
		t.Fatal("a negative key still announced a call deadline")
	}
	if sec, ok := deadlineSecondsIn(opening(deadlineServer(t, 7, slowThenFast))); !ok || sec < 5 || sec > 7 {
		t.Fatalf("agent_call_deadline_sec=7 announced %d s (ok %v), want about 7", sec, ok)
	}
}

// seenDispatch is what one dispatch to a fleet node carried.
type seenDispatch struct {
	priority any
	tenant   string
}

// captureNode is a fleet node that records what each dispatch carried and finishes
// every job at once.
type captureNode struct {
	mu   sync.Mutex
	seen []seenDispatch
}

func (n *captureNode) dispatches() []seenDispatch {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]seenDispatch(nil), n.seen...)
}

func newCaptureNode(t *testing.T) (*captureNode, string) {
	t.Helper()
	n := &captureNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-a", "queue_depth": 0, "agent_seat": "remote-seat", "agent_ctx_tokens": 32768,
			"agent_seat_resident": true, "agent_enabled": true, "jobs_queued": 0, "jobs_running": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		n.mu.Lock()
		n.seen = append(n.seen, seenDispatch{priority: env["priority"], tenant: r.Header.Get(core.TenantHeader)})
		n.mu.Unlock()
		id, _ := env["job_id"].(string)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": id, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		wire, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-a", Seat: "remote-seat",
			Output: "done on the node", Structured: json.RawMessage(`{"answer":"ok"}`), StopReason: "done"})
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(wire)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return n, srv.URL
}

// remoteDelegateArgs is one agent_delegate subtask pinned to the given node.
func remoteDelegateArgs(url string, extra map[string]any) string {
	m := map[string]any{
		"subtasks": []any{map[string]any{"goal": "answer it", "output_schema": map[string]any{"properties": map[string]any{"answer": map[string]any{"type": "string"}}}}},
		"route":    "remote", "remotes": []string{url},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// noLocalRun is a local seat that must never be used.
func noLocalRun(t *testing.T) func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the local seat ran")
		return core.AgentWireResult{}, nil
	}
}

// TestAgentDelegatePassesPriorityAndTenantToTheDispatch: the scheduling band the caller
// asked for and the per-session tenant reach the node in the dispatch (the envelope's
// `priority`, the X-Offload-Tenant header). The door rebuilt its RunOptions for the
// call deadline, which is exactly where a field is silently dropped: the band would be
// ignored (a sheddable job queued like an urgent one) and the tenant never named.
func TestAgentDelegatePassesPriorityAndTenantToTheDispatch(t *testing.T) {
	node, url := newCaptureNode(t)
	s := delegateTestServer(t, noLocalRun(t))
	if _, err := s.handleAgentDelegate(context.Background(), callReq(remoteDelegateArgs(url, map[string]any{"priority": 1}))); err != nil {
		t.Fatal(err)
	}
	got := node.dispatches()
	if len(got) != 1 {
		t.Fatalf("%d dispatches, want 1", len(got))
	}
	if got[0].priority != float64(1) {
		t.Errorf("the dispatch carried priority %v: the door dropped the caller's scheduling band", got[0].priority)
	}
	if got[0].tenant == "" || got[0].tenant != s.tenant {
		t.Errorf("the dispatch carried tenant %q, want the server's %q", got[0].tenant, s.tenant)
	}
}

// TestAgentDelegateHonoursTheServerQuarantine: a node the server has struck twice for
// answering about the wrong document must not receive delegation work through
// agent_delegate — the server-lifetime quarantine rides every delegation, not only
// research (silent-failure review, 2026-09-02).
func TestAgentDelegateHonoursTheServerQuarantine(t *testing.T) {
	node, url := newCaptureNode(t)
	s := delegateTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "local", StopReason: "done"}, nil
	})
	s.quarantine.Strike(url)
	s.quarantine.Strike(url)
	if !s.quarantine.Blocked(url) {
		t.Fatal("setup: the node is not quarantined")
	}
	if _, err := s.handleAgentDelegate(context.Background(), callReq(remoteDelegateArgs(url, nil))); err != nil {
		t.Fatal(err)
	}
	if n := len(node.dispatches()); n != 0 {
		t.Errorf("agent_delegate dispatched %d job(s) to a quarantined node", n)
	}
}

// TestResearchHonoursTheServerQuarantine: the same rule at the research door.
func TestResearchHonoursTheServerQuarantine(t *testing.T) {
	node, url := newCaptureNode(t)
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = []string{url}
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "local",
			Structured: json.RawMessage(`{"summary":"x"}`), StopReason: "done"}, nil
	}
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	s.quarantine.Strike(url)
	s.quarantine.Strike(url)
	args, _ := json.Marshal(map[string]any{"goal": "digest the page", "urls": []string{"https://docs.example/a"}, "route": "remote",
		"output_schema": map[string]any{"properties": map[string]any{"summary": map[string]any{"type": "string"}}}})
	if _, err := s.handleResearch(context.Background(), callReq(string(args))); err != nil {
		t.Fatal(err)
	}
	if n := len(node.dispatches()); n != 0 {
		t.Errorf("offload_research dispatched %d job(s) to a quarantined node", n)
	}
}

// callKeepOpen is callOverMCP without the closing quiesce and without closing the
// session: the caller keeps the client open to see whether the server keeps talking, or
// keeps a goroutine, after it answered.
func callKeepOpen(t *testing.T, s *Server, tool string, args map[string]any, token any) (*mcp.CallToolResult, *progressLog, func()) {
	t.Helper()
	log := &progressLog{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := s.buildServer("test").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) { log.add(req.Params) },
	})
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	if token != nil {
		params.SetProgressToken(token)
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	return res, log, func() { cs.Close(); ss.Close(); cancel() }
}

// reporterGoroutines is how many progress-reporter goroutines are running now.
func reporterGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*progressReporter).run")
}

// waitNoReporter waits (bounded) for every progress-reporter goroutine to end.
func waitNoReporter(t *testing.T, what string) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if reporterGoroutines() == 0 {
			return
		}
	}
	t.Fatalf("a progress reporter goroutine is still running after %s returned: the handler never stopped it, so it keeps its ticker (and its heartbeats) for the life of the server", what)
}

// TestAgentDelegateStopsItsProgressReporterWithTheCall: the reporter is owned by the
// call. One that outlives it keeps sending heartbeats for a request that has already
// been answered.
func TestAgentDelegateStopsItsProgressReporterWithTheCall(t *testing.T) {
	waitNoReporter(t, "an earlier test") // start from a clean slate
	s := deadlineServer(t, 30, slowThenFast)
	_, _, closeAll := callKeepOpen(t, s, "agent_delegate", map[string]any{
		"subtasks": []any{map[string]any{"goal": "fast one"}}, "route": "local"}, "gr-1")
	defer closeAll()
	waitNoReporter(t, "agent_delegate")
}

// TestResearchStopsItsProgressReporterWithTheCall: the same at the research door.
func TestResearchStopsItsProgressReporterWithTheCall(t *testing.T) {
	waitNoReporter(t, "an earlier test")
	s := researchServer(t, nil)
	_, _, closeAll := callKeepOpen(t, s, "offload_research", map[string]any{
		"goal": "digest the page", "urls": []any{"https://docs.example/a"}, "route": "local",
		"output_schema": map[string]any{"properties": map[string]any{"summary": map[string]any{"type": "string"}}}}, "gr-2")
	defer closeAll()
	waitNoReporter(t, "offload_research")
}

// TestProgressStopDoesNotDelayTheAnswer: the handler answers when the subtasks are done,
// not after the reporter's two-second stop allowance. stop() waits for the reporter to
// wind down, which it does at once; a stop that always waited out the allowance would
// hold every answer of every progress-aware call for two seconds.
func TestProgressStopDoesNotDelayTheAnswer(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	start := time.Now()
	_, _, closeAll := callKeepOpen(t, s, "agent_delegate", map[string]any{
		"subtasks": []any{map[string]any{"goal": "fast one"}}, "route": "local"}, "lat-1")
	defer closeAll()
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Fatalf("a one-subtask call with a progress token answered after %s: stop() holds the response for its full allowance", elapsed)
	}
}

// TestProgressMessagesNumberSubtasksFromOne: a client counts "subtask 1 of 2", not
// "subtask 0 of 2" — the engine's indexes are zero-based, the words are not.
func TestProgressMessagesNumberSubtasksFromOne(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	_, log := callOverMCP(t, s, "agent_delegate", twoSubtasks, "num-1")
	all := strings.Join(progressMessages(log.all()), " | ")
	for _, want := range []string{"subtask 1 of 2 started", "subtask 2 of 2 started", "subtask 1 of 2 finished", "subtask 2 of 2 finished"} {
		if !strings.Contains(all, want) {
			t.Errorf("no notification says %q: %s", want, all)
		}
	}
	if strings.Contains(all, "subtask 0 of") {
		t.Errorf("a notification numbers a subtask from zero: %s", all)
	}
}

// TestProgressHeartbeatReportsWhatHasFinished: while a slow subtask runs on, the heartbeat
// says how many subtasks are done — the count the last finish event carried — not a stale
// zero.
func TestProgressHeartbeatReportsWhatHasFinished(t *testing.T) {
	old := progressHeartbeat
	progressHeartbeat = 40 * time.Millisecond
	t.Cleanup(func() { progressHeartbeat = old })
	s := deadlineServer(t, 30, slowThenFast) // "fast" finishes at once, "slow" after 300 ms
	_, log := callOverMCP(t, s, "agent_delegate", twoSubtasks, "hb-1")
	for _, m := range progressMessages(log.all()) {
		if strings.Contains(m, "still working: 1 of 2 subtasks done") {
			return
		}
	}
	t.Fatalf("no heartbeat said 1 of 2 subtasks were done while the slow one ran: %v", progressMessages(log.all()))
}

// TestAStoppedReporterSendsNoHeartbeat: once the handler has stopped the reporter, a
// heartbeat tick that was already due must not write — the request has been answered and
// the session may be closing. The reporter here has no session at all, so a write would
// crash instead of merely failing; the guard is what keeps it from being reached.
func TestAStoppedReporterSendsNoHeartbeat(t *testing.T) {
	p := &progressReporter{}
	p.closed.Store(true)
	p.send(t.Context(), "still working")
}
