package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// --- dedupe rung -------------------------------------------------------------

func dupTranscript(body string) []Msg {
	return []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Args: `{"p":"a"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: body},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "read_file", Args: `{"p":"a2"}`}}},
		{Role: "tool", ToolCallID: "c2", Content: body}, // identical bytes, later copy
		{Role: "assistant", Content: "thinking"},
		{Role: "user", Content: "go on"},
	}
}

// TestDedupeCollapsesOlderDuplicate: the OLDER of two byte-identical tool
// results collapses to a reference naming the LATER call; the newest copy
// stays authoritative and byte-intact.
func TestDedupeCollapsesOlderDuplicate(t *testing.T) {
	body := strings.Repeat("identical content line\n", 40)
	msgs := dupTranscript(body)
	budget := estimateTokens(msgs) - len(body)/bytesPerToken/2 // pressure that one dedupe relieves
	out := compact(msgs, budget, 1, 2, compactOpts{})
	if estimateTokens(out) > budget {
		t.Fatalf("dedupe alone should have fit the budget")
	}
	if !isDeduped(out[3].Content) || !strings.Contains(out[3].Content, "c2") {
		t.Fatalf("older duplicate not collapsed to a reference naming the later call: %q", out[3].Content)
	}
	if out[5].Content != body {
		t.Fatalf("the LATER copy must stay byte-intact, got %q", out[5].Content[:40])
	}
	// Idempotent + deterministic: re-compacting lands on identical bytes.
	again := compact(out, budget, 1, 2, compactOpts{})
	if !reflect.DeepEqual(out, again) {
		t.Fatal("re-compaction of a deduped transcript changed bytes")
	}
}

// Tiny duplicates are not worth a reference marker: no dedupe reference may
// appear anywhere in the output, whatever the other rungs do.
func TestDedupeSkipsTinyBodies(t *testing.T) {
	msgs := dupTranscript("short")
	out := compact(msgs, 1, 1, 2, compactOpts{}) // absurd budget → every rung runs
	for i, m := range out {
		if isDeduped(m.Content) {
			t.Fatalf("turn %d: a %d-char body must never be deduped: %q", i, len("short"), m.Content)
		}
	}
}

// --- H8 pinning --------------------------------------------------------------

// TestPinnedResultSurvivesLossyRungs: a pinned tool body is exempt from
// dedupe/skeleton/elide, and its unit from drop — while unpinned peers are
// compacted around it.
func TestPinnedResultSurvivesLossyRungs(t *testing.T) {
	body := strings.Repeat("some verbose result content here\n", 60)
	msgs := []Msg{
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Content: body},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c2", Content: body + "x"}, // not identical (no dedupe)
		{Role: "assistant", Content: "final"},
	}
	opts := compactOpts{Skeleton: true, Pinned: map[string]bool{"c1": true}}
	out := compact(msgs, 60, 1, 1, opts) // brutal budget: every rung engages
	if out[2].Content != body {
		t.Fatalf("pinned body was modified: %q", out[2].Content[:40])
	}
	// The pinned unit must also survive the drop rung.
	foundPinned := false
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			foundPinned = true
		}
	}
	if !foundPinned {
		t.Fatal("pinned unit was dropped")
	}
}

// TestLoopExactRepeatPinsOriginalResult: loop-level H8 — the model repeats an
// exact call, the breaker refuses, and from then on the ORIGINAL result is
// pinned against lossy compaction.
func TestLoopExactRepeatPinsOriginalResult(t *testing.T) {
	body := strings.Repeat("precious content the model keeps re-reading\n", 80)
	filler := strings.Repeat("unimportant other output\n", 80)
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "other_tool", `{"p":"b"}`)}}, FinishReason: "tool_calls"},
		// The exact repeat of c1's (name,args) — refused by the breaker, pins c1.
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "read_file", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []Tool{
		{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) { return body, nil }},
		{ToolSpec: ToolSpec{Name: "other_tool"}, Exec: func(_ context.Context, _ string) (string, error) { return filler, nil }},
	}
	// A window tight enough that compaction MUST elide something on the last
	// step, but generous enough to hold the pinned body + markers.
	loop := NewLoop(client, tools, 10).WithContextTokens(4096).WithMaxTokens(1024).WithSkeletonPrune(true)
	res, err := loop.Run(context.Background(), "objective: keep reading the same file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var pinnedBody string
	for _, m := range res.Transcript {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			pinnedBody = m.Content
		}
	}
	if pinnedBody != body {
		t.Fatalf("the re-requested result was compacted (len %d vs %d) — H8 pin not applied", len(pinnedBody), len(body))
	}
}

// --- FORCE_PRESERVE ----------------------------------------------------------

// TestElideKeepsSignalResidue: eliding a body that carries error lines keeps a
// bounded residue of them under the marker; a signal-free body gets the bare
// marker exactly as before.
func TestElideKeepsSignalResidue(t *testing.T) {
	noise := strings.Repeat("plain log line without anything special\n", 80)
	sig := "ERROR: connection refused id=42\nFATAL: disk full on /var/data\n"
	msgs := []Msg{
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: noise + sig + noise},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2"}}},
		{Role: "tool", ToolCallID: "c2", Content: noise},
		{Role: "assistant", Content: "final"},
	}
	out := compact(msgs, 120, 1, 1, compactOpts{})
	if !isElided(out[2].Content) {
		t.Fatalf("signal body not elided: %q", out[2].Content[:40])
	}
	if !strings.Contains(out[2].Content, "connection refused") || !strings.Contains(out[2].Content, "disk full") {
		t.Fatalf("signal lines lost by elision: %q", out[2].Content)
	}
	if !isElided(out[4].Content) || strings.Contains(out[4].Content, "\n") {
		t.Fatalf("signal-free body should be a bare one-line marker: %q", out[4].Content)
	}
	// The residue is bounded.
	if len(out[2].Content) > len(elisionMarker(1<<20))+elideSignalMaxChars+elideSignalMaxLines+16 {
		t.Fatalf("residue unbounded: %d chars", len(out[2].Content))
	}
}

// TestDropRefusesSignalResidue: the drop rung never removes a unit whose tool
// body carries preserved signal — the ladder exhausts honestly instead.
func TestDropRefusesSignalResidue(t *testing.T) {
	noise := strings.Repeat("plain filler line here\n", 40)
	msgs := []Msg{
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: noise + "ERROR: the one line that matters\n" + noise},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2"}}},
		{Role: "tool", ToolCallID: "c2", Content: noise},
		{Role: "assistant", Content: "final"},
	}
	// A budget below what even full elision can reach forces the drop rung.
	out := compact(msgs, 1, 1, 1, compactOpts{})
	sawSignal := false
	for _, m := range out {
		if m.Role == "tool" && strings.Contains(m.Content, "the one line that matters") {
			sawSignal = true
		}
	}
	if !sawSignal {
		t.Fatal("the drop rung destroyed preserved signal — FORCE_PRESERVE guard not holding")
	}
	// The signal-free unit WAS droppable and must be gone (the guard is
	// selective, not a blanket drop disable).
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "c2" {
			t.Fatal("signal-free unit survived a budget that requires dropping it")
		}
	}
}

// --- fit telemetry -----------------------------------------------------------

// TestLoopCountsExhaustedCompactions: a window so small the ladder cannot fit
// the preamble+recent floor must surface fit=false telemetry on the Result —
// never a silent over-budget request.
func TestLoopCountsExhaustedCompactions(t *testing.T) {
	long := strings.Repeat("tool output line with routine content\n", 200)
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) { return long, nil }}}
	// ctx floor (256-token input budget) far below one tool body + keepRecent.
	loop := NewLoop(client, tools, 5).WithSystem(strings.Repeat("big system prompt ", 100)).WithContextTokens(300).WithMaxTokens(64).WithToolResultCap(len(long) + 1)
	res, err := loop.Run(context.Background(), "objective")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CompactionsExhausted == 0 {
		t.Fatal("ladder demonstrably could not fit the budget, but CompactionsExhausted = 0 — fit=false telemetry missing")
	}
}

// --- floor-mode monotonicity -------------------------------------------------

// TestCompactionMonotonicity: (1) compaction is idempotent at a fixed budget
// (identical bytes on re-compaction — the KV-prefix invariant); (2) a
// transcript compacted under HARD pressure re-compacted under a GENTLER budget
// stays exactly as compacted — a turn only ever moves down the ladder
// (no-op → dedupe/GCF/skeleton → marker → drop), never back up.
func TestCompactionMonotonicity(t *testing.T) {
	body := strings.Repeat("verbose content line for the ladder to work on\n", 60)
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: body},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2"}}},
		{Role: "tool", ToolCallID: "c2", Content: body + "tail"},
		{Role: "assistant", Content: "final"},
	}
	opts := compactOpts{Skeleton: true}
	hard := compact(msgs, 100, 1, 2, opts)
	if reflect.DeepEqual(hard, msgs) {
		t.Fatal("fixture invalid: hard compaction did nothing")
	}
	if again := compact(hard, 100, 1, 2, opts); !reflect.DeepEqual(hard, again) {
		t.Fatal("re-compaction at the same budget changed bytes (idempotence broken)")
	}
	if gentle := compact(hard, 1<<20, 1, 2, opts); !reflect.DeepEqual(hard, gentle) {
		t.Fatal("a gentler budget REGRESSED a compacted transcript up the ladder (monotonicity broken)")
	}
}
