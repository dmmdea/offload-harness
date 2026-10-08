// The whole-call deadline of the MCP delegation doors (ADR 0065, register C-67).
//
// The MCP client aborts a tool call at its own limit (1,800 s in the reference
// setup) and DROPS the response with it. agent_delegate and offload_research had
// no deadline of their own: the request context carried none and the results came
// back only when every subtask ended, so one slow subtask took the finished ones
// down with the call (2026-09-27: a call ran 2,103 s and lost a finished 423 s
// answer). These tests drive the real handlers with a compressed deadline.

package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
)

// mcpClientAbort is the limit the reference MCP client aborts a tool call at.
const mcpClientAbort = 1800 * time.Second

// deadlineServer is delegateTestServer with the whole-call deadline compressed to
// sec seconds (config.AgentCallDeadlineSec is whole seconds; a one-second
// deadline keeps each test to about a second).
func deadlineServer(t *testing.T, sec int, local func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error), roster ...string) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	cfg.AgentCallDeadlineSec = sec
	cfg.DelegateRemotes = roster // the configured fleet: a call's own remotes list may only narrow it
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

// callWithin runs a handler in a goroutine and FAILS the test when it has not
// returned inside limit — the shape of the bug is "returns only when every
// subtask ends", which must fail fast rather than hang the suite. Blocked seats
// are released through the context and the goroutine is always waited for.
func callWithin(t *testing.T, limit time.Duration, handler func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error), args string) (*mcp.CallToolResult, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		res *mcp.CallToolResult
		err error
	}
	ch := make(chan outcome, 1)
	start := time.Now()
	go func() {
		r, e := handler(ctx, callReq(args))
		ch <- outcome{r, e}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("handler: %v", o.err)
		}
		return o.res, time.Since(start)
	case <-time.After(limit):
		cancel()
		<-ch
		t.Fatalf("the handler had not returned after %s: it blocks until every subtask ends instead of returning at the call deadline", limit)
		return nil, 0
	}
}

// slowSeat finishes "fast" subtasks at once and blocks every other one until its
// context ends, the way the real agent loop does; cancelled counts the seats
// that were told to stop.
func slowSeat(cancelled *atomic.Int64) func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		if strings.Contains(c.Goal, "fast") {
			return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
				Output: "done on the seat", StopReason: "done"}, nil
		}
		<-ctx.Done()
		cancelled.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Deferred: true,
			DeferClass: core.DeferClassBudget, Reason: "agent loop: canceled (the parent context ended)"}, nil
	}
}

// TestHandleAgentDelegateReturnsFinishedResultsAtTheCallDeadline is PR-6's
// acceptance test: one subtask finishes, one blocks past the deadline. The call
// returns AT the deadline with the finished subtask's result and a budget-class
// defer "call deadline reached; N unfinished" for the other — as a successful
// tool call, because the call delivered.
func TestHandleAgentDelegateReturnsFinishedResultsAtTheCallDeadline(t *testing.T) {
	var cancelled atomic.Int64
	s := deadlineServer(t, 1, slowSeat(&cancelled))

	res, elapsed := callWithin(t, 6*time.Second, s.handleAgentDelegate,
		`{"subtasks":[{"goal":"fast one"},{"goal":"slow one"}],"route":"local"}`)

	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("the call returned after %s, want about the 1s deadline", elapsed)
	}
	if res.IsError {
		t.Fatal("IsError = true: a call that returned a finished result and one call-deadline defer delivered — the defer is a result shape, not a failure")
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["succeeded"] != float64(1) || summary["deferred"] != float64(1) || summary["failed"] != float64(0) {
		t.Fatalf("summary = %v, want one success and one defer", summary)
	}
	results, _ := m["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want both subtasks in submission order", m["results"])
	}
	fast, _ := results[0].(map[string]any)
	if fast["output"] != "done on the seat" || fast["deferred"] == true {
		t.Fatalf("the finished subtask = %v, want its own result intact", fast)
	}
	slow, _ := results[1].(map[string]any)
	reason, _ := slow["reason"].(string)
	if slow["deferred"] != true || slow["defer_class"] != core.DeferClassBudget || !strings.HasPrefix(reason, "call deadline reached; 1 unfinished") {
		t.Fatalf("the unfinished subtask = %v, want a budget defer reading %q", slow, "call deadline reached; 1 unfinished")
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the blocked seat saw the cancellation %d time(s), want 1", cancelled.Load())
	}
}

// TestHandleAgentDelegateEveryUnfinishedSubtaskIsStillASuccessfulCall: nothing
// finished before the deadline. The result is still not a tool error — the flag
// is for a call that failed, and a deadline defer is a budget-class RESULT shape
// (the same as any wall the seat ran out of).
func TestHandleAgentDelegateEveryUnfinishedSubtaskIsStillASuccessfulCall(t *testing.T) {
	var cancelled atomic.Int64
	s := deadlineServer(t, 1, slowSeat(&cancelled))
	res, _ := callWithin(t, 6*time.Second, s.handleAgentDelegate,
		`{"subtasks":[{"goal":"slow one"},{"goal":"slow two"}],"route":"local"}`)
	if res.IsError {
		t.Fatal("IsError = true on a call whose every subtask hit the call deadline: budget defers are result shapes")
	}
	summary, _ := decodeResult(t, res)["summary"].(map[string]any)
	if summary["deferred"] != float64(2) || summary["failed"] != float64(0) {
		t.Fatalf("summary = %v, want two call-deadline defers and no failure", summary)
	}
}

// TestHandleAgentDelegateDeadlineCanBeSwitchedOff: a negative agent_call_deadline_sec
// means no whole-call deadline (a client that never aborts). The same 2 s subtask
// is cut by a 1 s deadline and completes without one.
func TestHandleAgentDelegateDeadlineCanBeSwitchedOff(t *testing.T) {
	takes12 := func(ctx context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		select {
		case <-time.After(2 * time.Second):
			return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "done", StopReason: "done"}, nil
		case <-ctx.Done():
			return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, DeferClass: core.DeferClassBudget,
				Reason: "agent loop: canceled (the parent context ended)"}, nil
		}
	}
	for _, tc := range []struct {
		name          string
		sec           int
		wantSucceeded float64
	}{
		{"a 1s deadline cuts it", 1, 0},
		{"switched off, it completes", -1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := deadlineServer(t, tc.sec, takes12)
			res, _ := callWithin(t, 8*time.Second, s.handleAgentDelegate, `{"subtasks":[{"goal":"take a while"}],"route":"local"}`)
			summary, _ := decodeResult(t, res)["summary"].(map[string]any)
			if summary["succeeded"] != tc.wantSucceeded {
				t.Fatalf("agent_call_deadline_sec=%d: summary = %v, want succeeded=%v", tc.sec, summary, tc.wantSucceeded)
			}
		})
	}
}

// TestHandleResearchReturnsFinishedDigestsAtTheCallDeadline: nine pages run as
// two chunks (8 + 1). The last page of chunk one blocks; at the deadline the seven
// digests come back, that page and the page of the second chunk (which never
// started) are call-deadline defers counting the whole call, and the body keeps
// its C-75 shape (digests before sources, not an error).
func TestHandleResearchReturnsFinishedDigestsAtTheCallDeadline(t *testing.T) {
	var cancelled atomic.Int64
	var ran atomic.Int64
	s := deadlineServer(t, 1, func(ctx context.Context, c core.AgentContract, o delegate.LocalOptions) (core.AgentWireResult, error) {
		ran.Add(1)
		name := ""
		if len(c.Context) > 0 {
			name = c.Context[0].Name
		}
		if strings.HasPrefix(name, "08-") {
			c.Goal = "slow page"
		} else {
			c.Goal = "fast page"
		}
		return slowSeat(&cancelled)(ctx, c, o)
	})
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	urls := make([]string, 9)
	for i := range urls {
		urls[i] = "https://docs.example/p" + string(rune('a'+i))
	}
	args, _ := json.Marshal(map[string]any{"goal": "digest the page", "urls": urls, "route": "local",
		"output_schema": json.RawMessage(`{"properties":{"summary":{"type":"string"}}}`)})

	res, elapsed := callWithin(t, 8*time.Second, s.handleResearch, string(args))

	if elapsed > 3*time.Second {
		t.Fatalf("the call returned after %s, want about the 1s deadline", elapsed)
	}
	if res.IsError {
		t.Fatal("IsError = true on a research call that returned seven digests")
	}
	raw := res.Content[0].(*mcp.TextContent).Text
	if i, j := strings.Index(raw, `"results"`), strings.Index(raw, `"sources"`); i < 0 || j < 0 || i > j {
		t.Fatalf("results at %d, sources at %d: the digests must come first", i, j)
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["succeeded"] != float64(7) || summary["deferred"] != float64(2) || summary["skipped"] != nil {
		t.Fatalf("summary = %v, want 7 digests and 2 call-deadline defers (deferred, not skipped)", summary)
	}
	results, _ := m["results"].([]any)
	if len(results) != 9 {
		t.Fatalf("results = %d entries, want one per page (9)", len(results))
	}
	for _, i := range []int{7, 8} {
		r, _ := results[i].(map[string]any)
		reason, _ := r["reason"].(string)
		if r["deferred"] != true || r["defer_class"] != core.DeferClassBudget || !strings.HasPrefix(reason, "call deadline reached; 2 unfinished") {
			t.Fatalf("result %d = %v, want the call-deadline defer counting both chunks", i, r)
		}
	}
	if ran.Load() != 8 {
		t.Fatalf("the seat ran %d pages, want 8: the second chunk must not start after the deadline", ran.Load())
	}
}

// TestHandleResearchCallDeadlineCountsThePageFetch: the client's clock starts when
// it sends the request, so the deadline starts at handler entry — the page fetch
// spends from it. A deadline that began after the fetch would let a slow fetch push
// the whole call past the client's abort.
func TestHandleResearchCallDeadlineCountsThePageFetch(t *testing.T) {
	var cancelled atomic.Int64
	s := deadlineServer(t, 2, func(ctx context.Context, c core.AgentContract, o delegate.LocalOptions) (core.AgentWireResult, error) {
		c.Goal = "slow page"
		return slowSeat(&cancelled)(ctx, c, o)
	})
	s.researchFetch = func(ctx context.Context, urls []string, _ research.Options) []research.Fetched {
		select { // a slow fetch that honours its context
		case <-time.After(1400 * time.Millisecond):
		case <-ctx.Done():
		}
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	args := `{"goal":"digest the page","urls":["https://docs.example/a"],"route":"local","output_schema":{"properties":{"summary":{"type":"string"}}}}`
	_, elapsed := callWithin(t, 8*time.Second, s.handleResearch, args)
	// From handler entry the call ends at ~2s; counted from after the 1.4s fetch it
	// would end at ~3.4s. 3s splits the two with a margin either side.
	if elapsed > 3*time.Second {
		t.Fatalf("the call returned after %s: the deadline was counted from AFTER the 1.4s fetch (want ~2s from handler entry)", elapsed)
	}
}

// TestHandleResearchFetchIsBoundedByTheCallDeadline: a page fetch that would run
// for minutes must not hold the call past its deadline. The fetch honours its
// context, so the deadline ends it, its pages come back as failed sources, and the
// call answers "no usable source" at the deadline instead of after the fetch.
func TestHandleResearchFetchIsBoundedByTheCallDeadline(t *testing.T) {
	s := deadlineServer(t, 1, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the seat ran although every page fetch was cut off")
		return core.AgentWireResult{}, nil
	})
	s.researchFetch = func(ctx context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		select {
		case <-time.After(4 * time.Second): // a fetch that would run far past the deadline
		case <-ctx.Done():
			for i, u := range urls {
				out[i] = research.Fetched{URL: u, Err: "fetch: " + ctx.Err().Error()}
			}
			return out
		}
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	args := `{"goal":"digest the page","urls":["https://docs.example/a"],"route":"local"}`
	res, elapsed := callWithin(t, 8*time.Second, s.handleResearch, args)
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("the call returned after %s: the page fetch was not bounded by the 1s call deadline", elapsed)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || !strings.Contains(m["reason"].(string), "no usable source") {
		t.Fatalf("result = %v, want the no-usable-source deferral (every fetch was cut off)", m)
	}
}

// TestCallDeadlineDefaultIsBelowTheClientAbort: the default must leave a
// response room to be built and delivered before the client's own abort, and must
// still be longer than the longest single subtask that starts at once (a subtask
// that first waits queued on a node, or for capacity, can run longer and be cut:
// see config.DefaultCallDeadlineSec).
func TestCallDeadlineDefaultIsBelowTheClientAbort(t *testing.T) {
	d := config.Default().CallDeadline()
	if d <= 0 {
		t.Fatalf("the default call deadline = %s, want a positive built-in (the deadline is on by default)", d)
	}
	// The live acceptance is "call wall <= deadline + 30 s": the deadline itself
	// must leave that margin under the client's abort.
	if d+30*time.Second >= mcpClientAbort {
		t.Fatalf("the default call deadline %s + 30s is not below the client's %s abort", d, mcpClientAbort)
	}
	// One subtask can be polled for timeout_sec (cap) + the admission allowance +
	// the poll grace. The deadline exists to end a call, not to cut a healthy one.
	longest := time.Duration(core.AgentTimeoutSecCap+core.AgentAdmissionSecDefault+60) * time.Second
	if d <= longest {
		t.Fatalf("the default call deadline %s does not exceed the longest single subtask (%s)", d, longest)
	}
}

// TestDelegationToolDescriptionsStateTheResultRules keeps the MODEL-FACING text of
// the two delegation tools honest. A model reads these descriptions to decide what
// a result means, and they used to promise that a failed subtask "comes back
// flagged as an error" — the rule C-75 narrowed — and to list the research body in
// an order that is no longer the marshalled one. They now name the whole-call
// deadline and the partial-result rule, read here through a real MCP client (the
// same view Claude gets).
func TestDelegationToolDescriptionsStateTheResultRules(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	desc := map[string]string{}
	for _, tool := range listTools(t, cfg) {
		desc[tool.Name] = tool.Description
	}
	for name, wants := range map[string][]string{
		"agent_delegate": {
			"agent_call_deadline_sec", "call deadline reached; N unfinished",
			"when NOTHING succeeded the call comes back flagged as an error", "PARTIAL result",
		},
		"offload_research": {
			"the digests come before the sources", "PARTIAL result", "whole-call deadline", "`call deadline reached`",
		},
	} {
		d := desc[name]
		if d == "" {
			t.Fatalf("%s is not advertised with agent_delegation_enabled ON", name)
		}
		for _, want := range wants {
			if !strings.Contains(d, want) {
				t.Errorf("%s description does not say %q", name, want)
			}
		}
	}
	if stale := "and the call comes back flagged as an error, with this same JSON body intact"; strings.Contains(desc["agent_delegate"], stale) {
		t.Errorf("agent_delegate still promises %q: a partial result is not flagged since C-75", stale)
	}
	if i, j := strings.Index(desc["offload_research"], "results:[...agent_delegate result rows"), strings.Index(desc["offload_research"], "sources:[{index"); i < 0 || j < 0 || i > j {
		t.Errorf("offload_research lists sources (at %d) before results (at %d): the description must match the marshalled order", j, i)
	}
}
