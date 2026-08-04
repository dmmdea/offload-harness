package tasks

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The output budget must SCALE with the number of bullets the caller asked for.
// A flat budget was the largest single source of truncation defers (ledger audit
// 2026-08-03): the request grows, the budget does not, the JSON is cut mid-structure
// and the whole call defers to cloud.
func TestSummarizeBudgetScalesWithPoints(t *testing.T) {
	build := func(points any) Built {
		p := map[string]any{}
		if points != nil {
			p["max_points"] = points
		}
		b, err := Build(core.Request{Task: core.TaskSummarize, Input: "some text", Params: p})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return b
	}

	def := build(nil)      // default max_points = 5
	few := build(3)
	many := build(20)

	if !(many.MaxTokens > def.MaxTokens && def.MaxTokens > few.MaxTokens) {
		t.Fatalf("budget must grow with max_points: few=%d default=%d many=%d",
			few.MaxTokens, def.MaxTokens, many.MaxTokens)
	}
	// The regression this guards: the pre-fix flat 512 could not summarize a
	// 20-point request without truncating.
	if many.MaxTokens <= 512 {
		t.Fatalf("a 20-point summary must budget well past the old flat 512, got %d", many.MaxTokens)
	}
	if def.MaxTokens <= 512 {
		t.Fatalf("even the DEFAULT request must exceed the old flat 512, got %d", def.MaxTokens)
	}
}

// Triage returns a decision (enum) plus a free-text reason. Truncating the reason
// throws away a decision the model already made, so the budget must leave real room.
func TestTriageBudgetLeavesRoomForReason(t *testing.T) {
	b, err := Build(core.Request{
		Task:   core.TaskTriage,
		Input:  "some text",
		Params: map[string]any{"question": "is this urgent?"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.MaxTokens <= 256 {
		t.Fatalf("triage budget must exceed the 256 that was measured truncating, got %d", b.MaxTokens)
	}
	if !strings.Contains(b.Grammar, "decision") {
		t.Fatalf("triage grammar should still bind a decision field: %s", b.Grammar)
	}
}
