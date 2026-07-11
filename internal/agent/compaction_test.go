package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- helpers for building transcripts the compactor operates on ---

// asstCall is an assistant turn that requested one tool call with the given id.
func asstCall(id string) Msg {
	return Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "search", `{"q":"x"}`)}}
}

// toolResult is the matching tool result for a call id, with a body of n bytes.
func toolResult(id string, n int) Msg {
	return Msg{Role: "tool", ToolCallID: id, Content: strings.Repeat("R", n)}
}

// pairing verifies every tool-role message has a matching earlier assistant
// ToolCall id, and every assistant ToolCall id has a matching tool result —
// i.e. compaction never orphaned one side of a pair.
func pairing(t *testing.T, msgs []Msg) {
	t.Helper()
	callIDs := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			callIDs[c.ID] = true
		}
	}
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "tool" {
			if !callIDs[m.ToolCallID] {
				t.Errorf("orphan tool result: tool_call_id %q has no assistant ToolCall in the transcript", m.ToolCallID)
			}
			resultIDs[m.ToolCallID] = true
		}
	}
	for id := range callIDs {
		if !resultIDs[id] {
			t.Errorf("orphan assistant ToolCall: id %q has no matching tool result", id)
		}
	}
}

func TestEstimateTokensRoughlyLenOverFour(t *testing.T) {
	// A single user message of 400 chars should estimate ~100 tokens plus a
	// small per-message overhead — order-of-magnitude, not exact.
	msgs := []Msg{{Role: "user", Content: strings.Repeat("a", 400)}}
	got := estimateTokens(msgs)
	if got < 100 || got > 130 {
		t.Errorf("estimateTokens = %d, want ~100-130 (len/4 + small overhead)", got)
	}
}

// TestCompactUnderBudgetIsNoOp: when the transcript already fits, compact must
// return it byte-for-byte unchanged (preserves prefix stability / KV cache).
func TestCompactUnderBudgetIsNoOp(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "objective"},
		asstCall("c1"),
		toolResult("c1", 50),
		{Role: "assistant", Content: "answer"},
	}
	// Give a budget far above the estimate so nothing is touched.
	out := compact(msgs, 100000, 2)
	if len(out) != len(msgs) {
		t.Fatalf("no-op compaction changed message count: %d -> %d", len(msgs), len(out))
	}
	for i := range msgs {
		if out[i].Role != msgs[i].Role || out[i].Content != msgs[i].Content || out[i].ToolCallID != msgs[i].ToolCallID {
			t.Errorf("no-op compaction mutated msg %d: %+v -> %+v", i, msgs[i], out[i])
		}
	}
}

// TestCompactElidesOldestToolBodyKeepsSystemObjectiveRecent: over budget, the
// compactor must first elide the OLDEST tool-result body to a marker while
// keeping system + objective + the recent window full, and pairing intact.
func TestCompactElidesOldestToolBodyKeepsSystemObjectiveRecent(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "SYSTEM-PROMPT"},
		{Role: "user", Content: "OBJECTIVE-TEXT"},
		asstCall("c1"),
		toolResult("c1", 4000), // oldest, big — the elision target
		asstCall("c2"),
		toolResult("c2", 4000), // recent — must stay full
		{Role: "assistant", Content: "thinking"},
	}
	// Budget that the full transcript exceeds but that fits once the oldest
	// tool body is elided. Keep the most recent 2 turns full.
	budget := estimateTokens(msgs) - 800
	out := compact(msgs, budget, 2)

	// system + objective preserved verbatim.
	if out[0].Role != "system" || out[0].Content != "SYSTEM-PROMPT" {
		t.Errorf("system message not preserved: %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "OBJECTIVE-TEXT" {
		t.Errorf("objective not preserved: %+v", out[1])
	}
	// oldest tool result (c1) elided to a marker; recent (c2) kept full.
	var c1, c2 Msg
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			c1 = m
		}
		if m.Role == "tool" && m.ToolCallID == "c2" {
			c2 = m
		}
	}
	if c1.ToolCallID != "c1" {
		t.Fatalf("oldest tool result c1 was dropped entirely; want its body elided to a marker")
	}
	if !strings.Contains(c1.Content, "elided") || len(c1.Content) >= 4000 {
		t.Errorf("oldest tool body not elided to a compact marker: %q (len %d)", c1.Content, len(c1.Content))
	}
	if len(c2.Content) != 4000 {
		t.Errorf("recent tool result must stay full; got len %d", len(c2.Content))
	}
	pairing(t, out)
	if estimateTokens(out) > budget {
		t.Errorf("compacted transcript still over budget: %d > %d", estimateTokens(out), budget)
	}
}

// TestCompactDropsMatchedOlderTurnsAsUnits: when eliding all older tool bodies
// still isn't enough, the compactor drops whole OLDER turns oldest-first — an
// assistant-with-ToolCalls plus its matching tool results dropped together, so
// pairing is never broken — while never dropping system or the objective.
func TestCompactDropsMatchedOlderTurnsAsUnits(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "SYS"},
		{Role: "user", Content: "OBJ"},
		asstCall("c1"),
		toolResult("c1", 3000),
		asstCall("c2"),
		toolResult("c2", 3000),
		asstCall("c3"),
		toolResult("c3", 3000),
		{Role: "assistant", Content: "latest"},
	}
	// Aggressively small budget: even after eliding all older tool bodies (which
	// alone brings the estimate to ~85 tokens), the estimate must still exceed
	// this so the assistant framing of older turns is dropped to fit. keepRecent=1.
	budget := 40
	out := compact(msgs, budget, 1)

	// system + objective always survive.
	var sawSys, sawObj bool
	for _, m := range out {
		if m.Role == "system" && m.Content == "SYS" {
			sawSys = true
		}
		if m.Role == "user" && m.Content == "OBJ" {
			sawObj = true
		}
	}
	if !sawSys || !sawObj {
		t.Fatalf("system/objective must never be dropped: sys=%v obj=%v out=%+v", sawSys, sawObj, out)
	}
	// pairing must survive whole-turn drops.
	pairing(t, out)
	// the most recent assistant content must survive (keepRecent=1).
	var sawLatest bool
	for _, m := range out {
		if m.Role == "assistant" && m.Content == "latest" {
			sawLatest = true
		}
	}
	if !sawLatest {
		t.Errorf("most recent turn must be kept; 'latest' missing from %+v", out)
	}
	// and it must have actually shrunk from the original.
	if len(out) >= len(msgs) {
		t.Errorf("expected whole older turns dropped; message count %d did not shrink from %d", len(out), len(msgs))
	}
}

// --- reactive retry (belt-and-suspenders) ---

// errThenScriptClient errors the FIRST time Chat is called (simulating a
// context-overflow HTTP error), then behaves like fakeClient. It also records
// the message count of each call so the test can confirm the retry compacted.
type errThenScriptClient struct {
	fakeClient
	errOnCall int    // 0-based index of the call that should error once
	errText   string // the error surfaced on that call
	errored   bool
	msgCounts []int
}

func (c *errThenScriptClient) Chat(ctx context.Context, msgs []Msg, specs []ToolSpec, mt int) (Completion, error) {
	c.msgCounts = append(c.msgCounts, len(msgs))
	if !c.errored && len(c.msgCounts)-1 == c.errOnCall {
		c.errored = true
		return Completion{}, errors.New(c.errText)
	}
	return c.fakeClient.Chat(ctx, msgs, specs, mt)
}

// TestLoopReactiveRetryOnContextOverflow: a Chat error that looks like a
// context overflow must trigger a HARDER compaction and ONE retry of the same
// step; the run then completes instead of aborting.
func TestLoopReactiveRetryOnContextOverflow(t *testing.T) {
	client := &errThenScriptClient{
		errOnCall: 0,
		errText:   "chat 400: the request exceeds the available context size, try increasing the context",
	}
	client.script = []Completion{
		{Msg: Msg{Role: "assistant", Content: "done after retry"}, FinishReason: "stop"},
	}
	// A system prompt + objective + a low context budget so the retry has
	// something it can compact; the run must still succeed.
	loop := NewLoop(client, nil, 5).WithSystem("sys").WithContextTokens(2048).WithMaxTokens(1024)
	res, err := loop.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Run should recover from a context-overflow via reactive retry, got: %v", err)
	}
	if res.Output != "done after retry" {
		t.Errorf("output = %q, want 'done after retry'", res.Output)
	}
	if !client.errored {
		t.Errorf("the overflow error path was never exercised")
	}
	// Chat must have been called at least twice (initial error + retry).
	if client.calls < 1 || len(client.msgCounts) < 2 {
		t.Errorf("expected an initial erroring call plus a retry; msgCounts=%v calls=%d", client.msgCounts, client.calls)
	}
}

// TestLoopReactiveRetryGivesUpAfterOne: if the overflow persists after the one
// harder-compaction retry, Run returns the error as it does today (no infinite
// loop).
func TestLoopReactiveRetryGivesUpAfterOne(t *testing.T) {
	client := &alwaysErrClient{errText: "chat 413: context length exceeded"}
	loop := NewLoop(client, nil, 5).WithSystem("sys").WithContextTokens(2048).WithMaxTokens(1024)
	res, err := loop.Run(context.Background(), "do the thing")
	if err == nil {
		t.Fatalf("persistent overflow must surface as an error, got nil (res=%+v)", res)
	}
	if res.StopReason != "error" {
		t.Errorf("stop_reason = %q, want error", res.StopReason)
	}
	// exactly the initial call + one retry, then give up — not an unbounded loop.
	if client.calls != 2 {
		t.Errorf("expected exactly 2 Chat calls (initial + one retry), got %d", client.calls)
	}
}

// alwaysErrClient always returns the same context-overflow error.
type alwaysErrClient struct {
	errText string
	calls   int
}

func (c *alwaysErrClient) Chat(_ context.Context, _ []Msg, _ []ToolSpec, _ int) (Completion, error) {
	c.calls++
	return Completion{}, errors.New(c.errText)
}
