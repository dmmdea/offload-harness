package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The cut reported as "tool_calls" (0.140.2). Measured on a warm vLLM seat: a
// streamed tool call hit the 8,192-token completion cap in the middle of its
// argument and the engine reported finish_reason "tool_calls", NOT "length" —
// vLLM rewrites the finish reason whenever a tool call was streamed. The
// length-only classifier let the fragment into the transcript, and the next
// request died on HTTP 400 "Unterminated string" from the engine's own parse
// of the argument it was handed back.

// cutFragment is an argument cut mid-string, the shape the cap leaves behind.
const cutFragment = `{"path":"release-notes.md","content":"# Release`

// invalidArgsIn reports a tool call in msgs whose non-empty arguments are not
// valid JSON — exactly what the engine refuses with a 400.
func invalidArgsIn(msgs []Msg) (string, bool) {
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if strings.TrimSpace(c.Args) != "" && !json.Valid([]byte(c.Args)) {
				return c.Args, true
			}
		}
	}
	return "", false
}

// finish "tool_calls", the server's completion count AT the cap, the argument
// a fragment → a cut: re-issued at the final budget, never executed, never
// appended, never sent back to the engine.
func TestCutToolCallReportedAsToolCallsAtTheCapIsReissued(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", cutFragment)}},
			FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 1024}},
		{Msg: Msg{Role: "assistant", Content: "wrote release-notes.md"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || res.Output != "wrote release-notes.md" {
		t.Fatalf("stop_reason/output = %q/%q, want done / the answer (stop_note %q)", res.StopReason, res.Output, res.StopNote)
	}
	if len(client.seenMax) != 2 {
		t.Fatalf("Chat calls = %d (budgets %v), want 2: the cut step and its re-issue", len(client.seenMax), client.seenMax)
	}
	if want := FinalBudgetFor(1024); client.seenMax[1] < want {
		t.Errorf("re-issue ran at %d tok, want >= the final budget %d", client.seenMax[1], want)
	}
	if execs != 0 {
		t.Errorf("the tool ran %d times; a half-written argument must never be executed", execs)
	}
	if a, bad := invalidArgsIn(res.Transcript); bad {
		t.Errorf("the cut tool call was appended to the transcript: %q", a)
	}
	for i, sent := range client.seen {
		if a, bad := invalidArgsIn(sent); bad {
			t.Errorf("request %d carried invalid tool-call arguments back to the engine: %q", i, a)
		}
	}
}

// The same cut when the engine reports no usage: the argument is UNTERMINATED
// (the JSON ends mid-value), which is the cut's own signature.
func TestUnterminatedToolCallArgsWithToolCallsFinishIsACut(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", cutFragment)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "wrote release-notes.md"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || len(client.seenMax) != 2 || execs != 0 {
		t.Fatalf("stop=%q calls=%d execs=%d, want done / 2 (cut + re-issue) / 0", res.StopReason, len(client.seenMax), execs)
	}
	if a, bad := invalidArgsIn(res.Transcript); bad {
		t.Errorf("the cut tool call was appended to the transcript: %q", a)
	}
}

// Cut twice with "tool_calls" → the named StopToolCallCut, exactly as "length".
func TestCutToolCallReportedAsToolCallsTwiceStops(t *testing.T) {
	execs := 0
	cut := Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", cutFragment)}},
		FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 1024}}
	client := &fakeClient{script: []Completion{cut, cut}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopToolCallCut {
		t.Fatalf("stop_reason = %q, want %q", res.StopReason, StopToolCallCut)
	}
	if execs != 0 {
		t.Errorf("the tool ran %d times", execs)
	}
	if a, bad := invalidArgsIn(res.Transcript); bad {
		t.Errorf("the cut tool call was appended to the transcript: %q", a)
	}
}

// CONTROL: valid arguments with "tool_calls" AT the cap are a legitimate call
// that happened to fill the budget — executed and appended unchanged.
func TestValidToolCallArgsWithToolCallsFinishAreUnchanged(t *testing.T) {
	execs := 0
	args := `{"path":"release-notes.md","content":"# Release 1.0"}`
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", args)}},
			FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 1024}},
		{Msg: Msg{Role: "assistant", Content: "wrote release-notes.md"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || execs != 1 || len(client.seenMax) != 2 {
		t.Fatalf("stop=%q execs=%d calls=%d, want done / 1 / 2 (no re-issue)", res.StopReason, execs, len(client.seenMax))
	}
	found := false
	for _, m := range res.Transcript {
		for _, c := range m.ToolCalls {
			if c.ID == "c1" && c.Args == args {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the valid tool call is missing from the transcript or was rewritten: %+v", res.Transcript)
	}
}

// CONTROL: a MALFORMED but complete argument below the cap (the seat wrote bad
// JSON; nothing was cut) is not a budget cut — no re-issue at a raised budget,
// the tool is dispatched and reports its own error to the seat.
func TestMalformedToolCallArgsBelowTheCapAreNotACut(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path": release-notes.md}`)}},
			FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 40}},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || len(client.seenMax) != 2 {
		t.Fatalf("stop=%q calls=%d (budgets %v), want done / 2", res.StopReason, len(client.seenMax), client.seenMax)
	}
	if client.seenMax[1] != 1024 {
		t.Errorf("second call ran at %d tok, want the step budget 1024 (a cut re-issue would raise it)", client.seenMax[1])
	}
}

// The engine boundary: whatever a transcript holds, the request the client
// sends never carries tool-call arguments the engine cannot parse. A malformed
// argument goes out as "{}"; valid and empty arguments go out byte-for-byte,
// and the caller's transcript is not mutated.
func TestChatNeverSendsInvalidToolCallArguments(t *testing.T) {
	var body struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "m", "", 5*time.Second)
	msgs := []Msg{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Name: "write_file", Args: cutFragment},
			{ID: "b", Name: "write_file", Args: `{"path":"x"}`},
			{ID: "c", Name: "noop", Args: ``},
		}},
		{Role: "tool", ToolCallID: "a", Content: "error"},
		{Role: "tool", ToolCallID: "b", Content: "ok"},
		{Role: "tool", ToolCallID: "c", Content: "ok"},
	}
	if _, err := c.Chat(context.Background(), msgs, nil, 64); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(body.Messages) != 5 || len(body.Messages[1].ToolCalls) != 3 {
		t.Fatalf("request shape = %+v", body.Messages)
	}
	got := body.Messages[1].ToolCalls
	if a := got[0].Function.Arguments; !json.Valid([]byte(a)) {
		t.Errorf("invalid arguments sent to the engine: %q", a)
	}
	if a := got[1].Function.Arguments; a != `{"path":"x"}` {
		t.Errorf("valid arguments rewritten on the wire: %q", a)
	}
	if a := got[2].Function.Arguments; a != `` {
		t.Errorf("empty arguments rewritten on the wire: %q", a)
	}
	if msgs[1].ToolCalls[0].Args != cutFragment {
		t.Errorf("Chat mutated the caller's transcript: %q", msgs[1].ToolCalls[0].Args)
	}
}

// The cap signal on its own: an argument that is malformed mid-value (not an
// open prefix) but arrived with the completion count AT the step budget is
// still a cut. vLLM's streaming tool parser can close a half-emitted value
// badly, so "the cap was hit and the JSON does not parse" is the cut, whatever
// the fragment's shape.
func TestInvalidToolCallArgsAtTheCapAreACutWhateverTheirShape(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path": release-notes.md}`)}},
			FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 1024}},
		{Msg: Msg{Role: "assistant", Content: "wrote release-notes.md"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "done" || len(client.seenMax) != 2 || execs != 0 {
		t.Fatalf("stop=%q calls=%d execs=%d, want done / 2 (cut + re-issue) / 0", res.StopReason, len(client.seenMax), execs)
	}
	if want := FinalBudgetFor(1024); client.seenMax[1] < want {
		t.Errorf("re-issue ran at %d tok, want >= the final budget %d", client.seenMax[1], want)
	}
	if a, bad := invalidArgsIn(res.Transcript); bad {
		t.Errorf("the cut tool call was appended to the transcript: %q", a)
	}
}
