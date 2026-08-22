package agent

import (
	"context"
	"fmt"
	"testing"
)

// TestLoopDefaultSameToolCapAllowsMultiFileRecon pins the 0.79.0 default: eight
// distinct-argument calls to one tool execute, the ninth is refused. At the old
// default of 3 a six-file reconnaissance lost read_file on its fourth path —
// the cap, not the planner, was what starved the task.
func TestLoopDefaultSameToolCapAllowsMultiFileRecon(t *testing.T) {
	mk := func(p string) Completion {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c", "read_file", `{"path":"`+p+`"}`)}}, FinishReason: "tool_calls"}
	}
	var script []Completion
	for i := 1; i <= 9; i++ { // nine DISTINCT paths — none is an exact repeat
		script = append(script, mk(fmt.Sprintf("f%d.md", i)))
	}
	script = append(script, Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"})
	client := &fakeClient{script: script}
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		execs++
		return "contents", nil
	}}}
	res, err := NewLoop(client, tools, 12).Run(context.Background(), "map the repo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Pinned to the literal 8, not to the constant: comparing against the constant
	// would pass for ANY value of it, including the 3 this test exists to keep out.
	if execs != 8 {
		t.Fatalf("read_file executed %d times on nine distinct paths, want exactly 8 (the default cap) — a six-file recon needs at least that", execs)
	}
	if res.StopReason != "done" {
		t.Errorf("stop_reason = %q, want done", res.StopReason)
	}
}
