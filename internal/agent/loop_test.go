package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeClient returns a scripted sequence of completions (one per loop step),
// recording the messages it was called with so tests can assert the loop fed
// tool results back correctly.
type fakeClient struct {
	script []Completion
	calls  int
	seen   [][]Msg
}

func (f *fakeClient) Chat(_ context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
	f.seen = append(f.seen, append([]Msg(nil), msgs...))
	if f.calls >= len(f.script) {
		return Completion{}, errors.New("fakeClient: script exhausted")
	}
	c := f.script[f.calls]
	f.calls++
	return c, nil
}

func tc(id, name, args string) ToolCall { return ToolCall{ID: id, Name: name, Args: args} }

func TestLoopExecutesToolThenStops(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"hi"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done: hi"}, FinishReason: "stop"},
	}}
	var gotArgs string
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, args string) (string, error) {
			gotArgs = args
			var in struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			return "echoed:" + in.Text, nil
		},
	}}
	res, err := NewLoop(client, tools, 10).Run(context.Background(), "say hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" {
		t.Errorf("stop_reason = %q, want done", res.StopReason)
	}
	if res.Output != "done: hi" {
		t.Errorf("output = %q, want %q", res.Output, "done: hi")
	}
	if res.Steps != 2 {
		t.Errorf("steps = %d, want 2", res.Steps)
	}
	if gotArgs != `{"text":"hi"}` {
		t.Errorf("tool got args %q", gotArgs)
	}
	// the 2nd model call must have seen the tool result fed back as a role:tool message
	if client.calls != 2 {
		t.Fatalf("client called %d times, want 2", client.calls)
	}
	last := client.seen[1]
	var sawToolResult bool
	for _, m := range last {
		if m.Role == "tool" && m.ToolCallID == "c1" && strings.Contains(m.Content, "echoed:hi") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("2nd model call did not receive the tool result fed back: %+v", last)
	}
}

func TestLoopExecutesToolCallsRegardlessOfFinishReason(t *testing.T) {
	// REGRESSION (caught by the P0 e2e gate): llama.cpp/llama-swap sometimes returns
	// a tool call with finish_reason "stop" instead of "tool_calls". The loop MUST
	// key on the PRESENCE of tool calls, not finish_reason — otherwise it stops
	// without executing the tool and returns an empty answer.
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{}`)}}, FinishReason: "stop"},
		{Msg: Msg{Role: "assistant", Content: "final"}, FinishReason: "stop"},
	}}
	var called bool
	tools := []Tool{{ToolSpec: ToolSpec{Name: "echo"}, Exec: func(_ context.Context, _ string) (string, error) { called = true; return "ok", nil }}}
	res, err := NewLoop(client, tools, 10).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("tool present with finish_reason=stop was NOT executed (loop wrongly keyed on finish_reason)")
	}
	if res.Output != "final" {
		t.Errorf("output = %q, want final", res.Output)
	}
}

type fakeMem struct {
	recalled  []Recalled
	recallQ   string
	persisted string
}

func (f *fakeMem) Recall(_ context.Context, q string, _ int) ([]Recalled, error) {
	f.recallQ = q
	return f.recalled, nil
}
func (f *fakeMem) Persist(_ context.Context, text string, _ map[string]string) (string, error) {
	f.persisted = text
	return "mem-id", nil
}

func TestLoopRecallsIntoContextAndPersistsOutcome(t *testing.T) {
	mem := &fakeMem{recalled: []Recalled{{Text: "PAST-FACT-42", Score: 0.9}}}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	_, err := NewLoop(client, nil, 5).WithSystem("base sys").WithMemory(mem).Run(context.Background(), "do X")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mem.recallQ != "do X" {
		t.Errorf("recall query = %q, want the objective", mem.recallQ)
	}
	// recalled memory must be injected into the system message the model saw
	first := client.seen[0]
	var sawRecallUser, sawSystem, recallInSystem bool
	for _, m := range first {
		if m.Role == "user" && strings.Contains(m.Content, "PAST-FACT-42") && strings.Contains(m.Content, "RECALL") {
			sawRecallUser = true
		}
		if m.Role == "system" {
			sawSystem = strings.Contains(m.Content, "base sys")
			if strings.Contains(m.Content, "PAST-FACT-42") {
				recallInSystem = true
			}
		}
	}
	if !sawRecallUser {
		t.Errorf("recalled memory not injected as a fenced USER message: %+v", first)
	}
	if recallInSystem {
		t.Errorf("recalled (untrusted) memory must NOT be in the system role: %+v", first)
	}
	if !sawSystem {
		t.Errorf("system prompt missing the base instructions: %+v", first)
	}
	// the run outcome must be persisted
	if !strings.Contains(mem.persisted, "the answer") || !strings.Contains(mem.persisted, "do X") {
		t.Errorf("outcome not persisted: %q", mem.persisted)
	}
}

func TestLoopMemoryNilSafe(t *testing.T) {
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}
	res, err := NewLoop(client, nil, 5).Run(context.Background(), "x") // no WithMemory
	if err != nil || res.Output != "ok" {
		t.Fatalf("nil memory must be safe: %v / %q", err, res.Output)
	}
}

func TestLoopRespectsBudget(t *testing.T) {
	// model that always asks for another tool call -> must stop at maxSteps, not loop forever.
	always := Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c", "noop", `{}`)}}, FinishReason: "tool_calls"}
	client := &fakeClient{script: []Completion{always, always, always, always, always}}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "noop"}, Exec: func(_ context.Context, _ string) (string, error) { return "ok", nil }}}
	res, err := NewLoop(client, tools, 3).Run(context.Background(), "loop")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "budget" {
		t.Errorf("stop_reason = %q, want budget", res.StopReason)
	}
	if res.Steps != 3 {
		t.Errorf("steps = %d, want 3 (maxSteps)", res.Steps)
	}
}

func TestLoopDefersNotCrashesOnToolError(t *testing.T) {
	// a tool that errors must be fed back as an is_error tool result, and the loop continues.
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "boom", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "recovered"}, FinishReason: "stop"},
	}}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "boom"}, Exec: func(_ context.Context, _ string) (string, error) {
		return "", errors.New("kaboom")
	}}}
	res, err := NewLoop(client, tools, 10).Run(context.Background(), "trigger error")
	if err != nil {
		t.Fatalf("Run must not error on a tool failure: %v", err)
	}
	if res.Output != "recovered" {
		t.Errorf("output = %q, want recovered", res.Output)
	}
	last := client.seen[1]
	var sawErr bool
	for _, m := range last {
		if m.Role == "tool" && m.ToolCallID == "c1" && m.IsError && strings.Contains(m.Content, "kaboom") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("tool error not fed back as is_error tool result: %+v", last)
	}
}

func TestLoopUnknownToolDefersNotCrashes(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "ghost", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
	}}
	_, err := NewLoop(client, nil, 10).Run(context.Background(), "call a missing tool")
	if err != nil {
		t.Fatalf("Run must not error on unknown tool: %v", err)
	}
	last := client.seen[1]
	var sawErr bool
	for _, m := range last {
		if m.Role == "tool" && m.ToolCallID == "c1" && m.IsError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("unknown tool not fed back as is_error tool result: %+v", last)
	}
}
