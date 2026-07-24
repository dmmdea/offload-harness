package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gcf"
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
	// Give a budget far above the estimate so nothing is touched. Preamble =
	// system + objective = 2.
	out := compact(msgs, 100000, 2, 2, compactOpts{})
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
	out := compact(msgs, budget, 2, 2, compactOpts{}) // preamble = system + objective = 2

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
	out := compact(msgs, budget, 1, 2, compactOpts{}) // preamble = system + objective = 2

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

// I-1: with a profile active, the preamble is system → exemplars → recall →
// AGENT.md → objective, so the objective is NOT the "first user message" —
// exemplar #1 is. When a long transcript forces Step-4 turn-dropping, the
// objective (a bare older user message) must NOT be dropped. RED before the fix:
// protectedPrefixLen protects only exemplar #1, leaving the objective droppable.
func TestCompactKeepsObjectiveWhenProfileExemplarsPrecedeIt(t *testing.T) {
	const objective = "THE-REAL-OBJECTIVE-TEXT-MUST-SURVIVE"

	// A profile with leading exemplars (user/assistant tool-cycle pairs), exactly
	// like the shipped profiles now inject before recall/AGENT.md/objective.
	prof := Profile{
		Name:   "test",
		System: "profile system prompt",
		Exemplars: []Msg{
			{Role: "user", Content: "example question one"},
			{Role: "assistant", ToolCalls: []ToolCall{tc("ex1", "search", `{"q":"a"}`)}},
			{Role: "tool", ToolCallID: "ex1", Content: "example result one"},
			{Role: "user", Content: "example question two"},
			{Role: "assistant", ToolCalls: []ToolCall{tc("ex2", "search", `{"q":"b"}`)}},
			{Role: "tool", ToolCallID: "ex2", Content: "example result two"},
		},
	}

	// Script many tool-calling turns with big results so the transcript grows past
	// the input budget and forces Step-4 whole-turn dropping (elision alone won't
	// fit it), then a final answer.
	bigArgs := `{"blob":"` + strings.Repeat("A", 3000) + `"}`
	script := []Completion{}
	for i := 0; i < 8; i++ {
		script = append(script, Completion{
			Msg:          Msg{Role: "assistant", ToolCalls: []ToolCall{tc("call", "bloat", bigArgs)}},
			FinishReason: "tool_calls",
		})
	}
	script = append(script, Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"})
	client := &fakeClient{script: script}

	// A tool that returns a large result each call, and no same-tool cap so it can
	// be called repeatedly to build up the transcript.
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "bloat", Description: "bloat", Schema: []byte(`{"type":"object"}`)},
		Exec:     func(_ context.Context, _ string) (string, error) { return strings.Repeat("R", 3000), nil },
	}}

	// Small served window so compaction must drop older turns. AGENT.md via
	// WithWorktree(dir) would need a file; the exemplars + recall path is enough to
	// push the objective off the "first user message" position, which is the bug.
	loop := NewLoop(client, tools, 20).
		WithContextTokens(2048).
		WithMaxTokens(512).
		WithMaxSameTool(0).
		WithProfile(prof)

	if _, err := loop.Run(context.Background(), objective); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) == 0 {
		t.Fatal("client never called")
	}
	// Inspect the LAST (most compacted) transcript the client saw: the objective
	// text must still be present.
	last := client.seen[len(client.seen)-1]
	var sawObjective bool
	for _, m := range last {
		if strings.Contains(m.Content, objective) {
			sawObjective = true
			break
		}
	}
	if !sawObjective {
		t.Errorf("objective %q was dropped by compaction (profile exemplars precede it); it must always survive", objective)
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

// --- emergency shrink (reactive-overflow last resort) ---

// TestEmergencyShrinkNewestHugeTurn pins the exact live failure (flip-decision
// report 2026-07-24, F3): [system, objective, assistant, tool(HUGE)] — the
// harder-compaction retry no-ops because keepRecent protects the huge newest
// body, and the run dies. emergencyShrink must fit the budget WITHOUT touching
// the preamble and without dropping any turn.
func TestEmergencyShrinkNewestHugeTurn(t *testing.T) {
	huge := strings.Repeat("all work and no play makes a very long tool result line\n", 800)
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "objective: read the readme"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Content: huge},
	}
	budget := 1000
	// Precondition: this is the case compact() cannot help (everything is
	// protected by preamble + keepRecent) — pin it so the test keeps meaning.
	if compacted := compact(msgs, budget, 2, 2, compactOpts{}); estimateTokens(compacted) <= budget {
		t.Fatal("fixture invalid: compact() alone fit the budget, the emergency case never arises")
	}
	out := emergencyShrink(msgs, budget, 2, compactOpts{})
	if got := estimateTokens(out); got > budget {
		t.Fatalf("emergencyShrink left estimate %d > budget %d", got, budget)
	}
	if len(out) != len(msgs) {
		t.Fatalf("turn count changed: %d -> %d (must shrink bodies, never drop turns)", len(msgs), len(out))
	}
	if out[0].Content != "sys" || out[1].Content != "objective: read the readme" {
		t.Fatal("preamble was modified")
	}
	if out[3].Content == huge {
		t.Fatal("the huge newest body was not shrunk")
	}
	// Determinism: same input, same bytes.
	out2 := emergencyShrink(msgs, budget, 2, compactOpts{})
	for i := range out {
		if out[i].Content != out2[i].Content {
			t.Fatalf("nondeterministic shrink at turn %d", i)
		}
	}
	// The input slice itself must be untouched (shrink works on a copy).
	if msgs[3].Content != huge {
		t.Fatal("emergencyShrink mutated its input")
	}
}

// Oldest-first: with several oversized tool bodies, the OLDER ones are
// destroyed before the newest is touched.
func TestEmergencyShrinkPrefersOldest(t *testing.T) {
	body := strings.Repeat("filler line with some text in it\n", 120)
	msgs := []Msg{
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: body},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2"}}},
		{Role: "tool", ToolCallID: "c2", Content: body},
	}
	// Budget that fits once the OLDER body is reduced but does not require
	// touching the newest one beyond pass 1.
	budget := estimateTokens(msgs) - len(body)/bytesPerToken/2
	out := emergencyShrink(msgs, budget, 1, compactOpts{})
	if estimateTokens(out) > budget {
		t.Fatalf("did not fit budget")
	}
	if out[2].Content == body && out[4].Content != body {
		t.Fatal("shrink touched the newest body while the older one survived intact")
	}
}

// TestLoopReactiveRetryHugeNewestBody: the loop-level version of the live
// failure — the first Chat overflows while the transcript's newest tool body
// is inside keepRecent. Without emergencyShrink the retry re-sends the same
// bytes and the run dies; with it, the retry fits and the run completes.
func TestLoopReactiveRetryHugeNewestBody(t *testing.T) {
	huge := strings.Repeat("readme content line that goes on and on\n", 700)
	client := &errThenScriptClient{
		errOnCall: 1, // the Chat AFTER the huge tool result lands
		errText:   "chat 400: request (9187 tokens) exceeds the available context size (8192 tokens), try increasing it",
	}
	client.script = []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"README.md"}`}}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "summarized after recovery"}, FinishReason: "stop"},
	}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		return huge, nil
	}}}
	// The explicit over-large cap reproduces the pre-fix reality: the boundary
	// cap does not save us (live it was mis-derived from an assumed window).
	loop := NewLoop(client, tools, 5).WithSystem("sys").WithContextTokens(2048).WithMaxTokens(512).WithToolResultCap(len(huge) + 1)
	res, err := loop.Run(context.Background(), "objective: read the readme")
	if err != nil {
		t.Fatalf("run died on the huge-newest-body overflow (the pre-fix behavior): %v", err)
	}
	if res.Output != "summarized after recovery" {
		t.Errorf("output = %q", res.Output)
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

// --- skeleton rung of the compaction ladder ---

// skTranscript builds a transcript whose two older tool results are verbose
// multi-line bodies with buried signal lines, plus a recent full turn.
func skTranscript() []Msg {
	old1 := buildToolOutput(120, "ERROR: connection refused by upstream")
	old2 := buildToolOutput(120, "--- FAIL: TestPairing (0.01s)")
	return []Msg{
		{Role: "system", Content: "SYSTEM-PROMPT"},
		{Role: "user", Content: "OBJECTIVE-TEXT"},
		asstCall("c1"),
		{Role: "tool", ToolCallID: "c1", Content: old1},
		asstCall("c2"),
		{Role: "tool", ToolCallID: "c2", Content: old2},
		asstCall("c3"),
		toolResult("c3", 800), // recent — must stay full
		{Role: "assistant", Content: "thinking"},
	}
}

// TestCompactSkeletonizesBeforeMarkerElision: with the skeleton rung enabled
// and a budget that skeletons alone can satisfy, older tool bodies become
// signal-preserving skeletons — NOT bare size markers — and the recent turn
// stays full, pairing intact.
func TestCompactSkeletonizesBeforeMarkerElision(t *testing.T) {
	msgs := skTranscript()
	budget := estimateTokens(msgs) - 1200 // fits once the two old bodies shrink to skeletons
	out := compact(msgs, budget, 2, 2, compactOpts{Skeleton: true})

	var c1, c3 Msg
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			c1 = m
		}
		if m.Role == "tool" && m.ToolCallID == "c3" {
			c3 = m
		}
	}
	if c1.ToolCallID != "c1" {
		t.Fatal("older tool result dropped; want it skeletonized in place")
	}
	if !isSkeletonized(c1.Content) {
		t.Errorf("older tool body not skeletonized: %q", firstLine(c1.Content))
	}
	if isElided(c1.Content) {
		t.Errorf("older tool body went straight to a bare marker; the skeleton rung was skipped")
	}
	if !strings.Contains(c1.Content, "ERROR: connection refused by upstream") {
		t.Errorf("skeleton lost the buried error signal line")
	}
	if len(c3.Content) != 800 {
		t.Errorf("recent tool result must stay full; got len %d", len(c3.Content))
	}
	pairing(t, out)
	if estimateTokens(out) > budget {
		t.Errorf("still over budget after skeleton rung: %d > %d", estimateTokens(out), budget)
	}
}

// TestCompactSkeletonFallsThroughToMarkers: when skeletons alone cannot reach
// the budget, the ladder must continue exactly as before — bare markers, then
// whole-turn drops — and still land under budget with pairing intact.
func TestCompactSkeletonFallsThroughToMarkers(t *testing.T) {
	msgs := skTranscript()
	// A budget far below what skeletons can reach: forces marker elision (and
	// possibly drops) after the skeleton rung.
	out := compact(msgs, 400, 1, 2, compactOpts{Skeleton: true})
	if estimateTokens(out) > 400 {
		t.Errorf("ladder failed to reach a tight budget: %d > 400", estimateTokens(out))
	}
	pairing(t, out)
	if out[0].Content != "SYSTEM-PROMPT" || out[1].Content != "OBJECTIVE-TEXT" {
		t.Errorf("protected preamble lost under fall-through pressure")
	}
}

// TestCompactSkeletonOffIsByteIdenticalToOldLadder: skeleton=false must
// reproduce the pre-feature ladder exactly. Pinned message-by-message: every
// message either survives verbatim or carries the EXACT bare-marker string the
// old ladder produced — any off-path drift (marker text, order, extra drops)
// fails here.
func TestCompactSkeletonOffIsByteIdenticalToOldLadder(t *testing.T) {
	msgs := skTranscript()
	budget := estimateTokens(msgs) - 1200
	out := compact(msgs, budget, 2, 2, compactOpts{})

	if len(out) != len(msgs) {
		t.Fatalf("off-path dropped/added messages: %d -> %d (this budget only needs body elision)", len(msgs), len(out))
	}
	for i := range out {
		if out[i].Role != msgs[i].Role || out[i].ToolCallID != msgs[i].ToolCallID {
			t.Fatalf("off-path reordered msg %d: %s/%s -> %s/%s", i, msgs[i].Role, msgs[i].ToolCallID, out[i].Role, out[i].ToolCallID)
		}
		want := msgs[i].Content
		// The old ladder's ONLY edit at this budget: the OLDEST tool body (c1)
		// becomes an exact bare marker — its elision alone (~1900 est. tokens
		// saved) satisfies the 1200-token deficit, so the oldest-first loop
		// stops there and c2 stays full.
		if out[i].Role == "tool" && out[i].ToolCallID == "c1" {
			want = elisionMarker(len(msgs[i].Content))
		}
		if out[i].Content != want {
			t.Errorf("off-path msg %d (%s/%s) drifted:\n got %q\nwant %q", i, out[i].Role, out[i].ToolCallID, firstLine(out[i].Content), firstLine(want))
		}
	}
}

// TestCompactSkeletonFallThroughMarkerReportsOriginalSize: when harder
// pressure elides a skeleton onward to a bare marker, the marker must disclose
// the ORIGINAL body size (parsed from the skeleton prefix), not the skeleton's
// own much smaller length.
func TestCompactSkeletonFallThroughMarkerReportsOriginalSize(t *testing.T) {
	msgs := skTranscript()
	origLen := len(msgs[3].Content) // c1's verbose body
	out := compact(msgs, 400, 1, 2, compactOpts{Skeleton: true})
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" && isElided(m.Content) {
			if m.Content != elisionMarker(origLen) {
				t.Errorf("fall-through marker misreports size: got %q, want %q", m.Content, elisionMarker(origLen))
			}
			return
		}
	}
	// c1 dropped entirely at this budget is also legal (step 4); nothing to assert then.
}

// jsonToolBody builds an eligible GCF payload: a JSON array of n flat objects.
func jsonToolBody(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"path":"internal/pkg/file_%d.go","line":%d,"match":"func Fn%d() error","kind":"function"}`, i, i*13, i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestCompactGCFRungRunsBeforeSkeleton: with both gentler rungs enabled and a
// budget the lossless rung alone can satisfy, an older JSON-array tool body
// must become a GCF block — NOT a skeleton, NOT a marker — and the recent
// window stays full.
func TestCompactGCFRungRunsBeforeSkeleton(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "SYS"},
		{Role: "user", Content: "OBJ"},
		asstCall("c1"),
		{Role: "tool", ToolCallID: "c1", Content: jsonToolBody(30)},
		asstCall("c2"),
		toolResult("c2", 800), // recent — full
		{Role: "assistant", Content: "thinking"},
	}
	budget := estimateTokens(msgs) - 200 // GCF's ~30%+ on the JSON body more than covers this
	out := compact(msgs, budget, 2, 2, compactOpts{GCF: true, Skeleton: true})

	var c1, c2 Msg
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			c1 = m
		}
		if m.Role == "tool" && m.ToolCallID == "c2" {
			c2 = m
		}
	}
	if !gcf.IsCompacted(c1.Content) {
		t.Fatalf("older JSON tool body not GCF-compacted: %q", firstLine(c1.Content))
	}
	if isSkeletonized(c1.Content) || isElided(c1.Content) {
		t.Errorf("lossless rung skipped: body went to a lossy rung")
	}
	if len(c2.Content) != 800 {
		t.Errorf("recent tool result must stay full; got len %d", len(c2.Content))
	}
	pairing(t, out)
	if estimateTokens(out) > budget {
		t.Errorf("still over budget after GCF rung: %d > %d", estimateTokens(out), budget)
	}
}

// TestCompactGCFIneligibleFallsToSkeleton: a verbose NON-JSON body cannot use
// the lossless rung; with both rungs on it must fall through to a skeleton.
func TestCompactGCFIneligibleFallsToSkeleton(t *testing.T) {
	msgs := skTranscript()
	budget := estimateTokens(msgs) - 1200
	out := compact(msgs, budget, 2, 2, compactOpts{GCF: true, Skeleton: true})
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			if !isSkeletonized(m.Content) {
				t.Errorf("prose body should have fallen through to the skeleton rung: %q", firstLine(m.Content))
			}
			if gcf.IsCompacted(m.Content) {
				t.Errorf("prose body wrongly GCF-marked")
			}
		}
	}
	pairing(t, out)
}

// TestLoopGCFCompactWiring: a loop built WithGCFCompact(true) and a window
// that forces compaction must produce GCF-compacted older tool results in the
// final transcript — proving the flag reaches compact() through ladderOpts.
func TestLoopGCFCompactWiring(t *testing.T) {
	// Sized so the transcript EXCEEDS the input budget (compaction must fire)
	// while GCF'd older bodies (~45% smaller on this shape) bring it back
	// under — a tighter window would fall through to bare markers and hide
	// the GCF block; a looser one never compacts at all (first version of
	// this test failed exactly that way).
	body := jsonToolBody(80)
	script := []Completion{}
	for i := 0; i < 5; i++ {
		script = append(script, Completion{
			Msg:          Msg{Role: "assistant", ToolCalls: []ToolCall{tc(fmt.Sprintf("g%d", i), "bloat", fmt.Sprintf(`{"i":%d}`, i))}},
			FinishReason: "tool_calls",
		})
	}
	script = append(script, Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"})
	client := &fakeClient{script: script}
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "bloat", Description: "bloat", Schema: []byte(`{"type":"object"}`)},
		Exec:     func(_ context.Context, _ string) (string, error) { return body, nil },
	}}
	loop := NewLoop(client, tools, 20).
		WithContextTokens(8192).
		WithMaxTokens(256).
		WithMaxSameTool(0).
		WithGCFCompact(true)
	res, err := loop.Run(context.Background(), "objective")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Transcript {
		if m.Role == "tool" && gcf.IsCompacted(m.Content) {
			found = true
		}
	}
	if !found {
		t.Errorf("no GCF-compacted tool result in the final transcript — WithGCFCompact not wired into the loop's compact() calls")
	}
}

// TestLoopSkeletonPruneWiring: a loop built WithSkeletonPrune(true) and a tiny
// window must produce skeletonized (not bare-marker) older tool results in the
// transcript it sends — proving the flag actually reaches compact().
func TestLoopSkeletonPruneWiring(t *testing.T) {
	verbose := buildToolOutput(150, "ERROR: wired through")
	script := []Completion{}
	for i := 0; i < 5; i++ {
		// Unique args per call — byte-identical repeats are refused by
		// dispatchOrThrottle and would replace the verbose bodies under test
		// with small refusal messages.
		script = append(script, Completion{
			Msg:          Msg{Role: "assistant", ToolCalls: []ToolCall{tc(fmt.Sprintf("call-%d", i), "bloat", fmt.Sprintf(`{"i":%d}`, i))}},
			FinishReason: "tool_calls",
		})
	}
	script = append(script, Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"})
	client := &fakeClient{script: script}
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "bloat", Description: "bloat", Schema: []byte(`{"type":"object"}`)},
		Exec:     func(_ context.Context, _ string) (string, error) { return verbose, nil },
	}}
	// Window sized so the run must compact but skeletons ALONE satisfy the
	// budget — a tighter window would legitimately elide the skeletons onward
	// to bare markers (the fall-through rung), hiding the wiring under test.
	loop := NewLoop(client, tools, 20).
		WithContextTokens(8192).
		WithMaxTokens(512).
		WithMaxSameTool(0).
		WithSkeletonPrune(true)
	res, err := loop.Run(context.Background(), "objective")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range res.Transcript {
		if m.Role == "tool" && isSkeletonized(m.Content) {
			found = true
		}
	}
	if !found {
		t.Errorf("no skeletonized tool result in the final transcript — WithSkeletonPrune not wired into the loop's compact() calls")
	}
}
