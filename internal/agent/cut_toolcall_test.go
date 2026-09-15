package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Register D-114: a `write_file` call whose JSON argument is CUT at the step
// completion budget is a BUDGET defect, never a broken box. Measured on the
// Aorus 9B llama.cpp seat (step_tokens 1024, a ~3 KB single-call write):
// llama.cpp answers HTTP 500 "Failed to parse tool call arguments as JSON …
// invalid string: missing closing quote" at column 2847, the loop returned
// stop_reason "error", and the node filed defer_class "infrastructure" — the
// stack blamed for a budget the seat could not fit the argument into.

// llamaCppCutBody is the 500 body llama.cpp returns when the tool-call
// argument it must parse was cut mid-string by the completion budget (the
// nlohmann parse error, verbatim in shape; the client keeps the first 600
// bytes of it in StatusError.Body).
const llamaCppCutBody = `{"error":{"code":500,"message":"Failed to parse tool call arguments as JSON: ` +
	`[json.exception.parse_error.101] parse error at line 1, column 2847: syntax error while parsing value ` +
	`- invalid string: missing closing quote; last read: '{\"path\":\"release-notes.md\",\"content\":\"# Release'",` +
	`"type":"server_error"}}`

func cutErr() error { return &StatusError{Code: 500, Body: llamaCppCutBody} }

// scriptedErrClient errors on the call indexes in errs (0-based) and behaves
// like fakeClient otherwise. maxSeen records the completion budget of EVERY
// call, the errored ones included (fakeClient.seenMax never sees those).
type scriptedErrClient struct {
	fakeClient
	errs    map[int]error
	maxSeen []int
}

func (c *scriptedErrClient) Chat(ctx context.Context, msgs []Msg, specs []ToolSpec, mt int) (Completion, error) {
	c.maxSeen = append(c.maxSeen, mt)
	if err, ok := c.errs[len(c.maxSeen)-1]; ok {
		return Completion{}, err
	}
	return c.fakeClient.Chat(ctx, msgs, specs, mt)
}

// writeTool is a stand-in for write_file that counts executions: a cut call
// must never reach it — the engine refused the argument, so nothing ran.
func writeTool(execs *int) []Tool {
	return []Tool{{
		ToolSpec: ToolSpec{Name: "write_file", Description: "write a file", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) {
			*execs++
			return "written", nil
		},
	}}
}

// (i) The engine refuses the cut argument at the step budget → the SAME step is
// re-issued once at the wall-fitted final budget, the seat answers, and the run
// ends "done" with the cut attempt on the record.
func TestCutToolCallIsReissuedAtTheFinalBudget(t *testing.T) {
	execs := 0
	client := &scriptedErrClient{errs: map[int]error{0: cutErr()}}
	client.script = []Completion{
		{Msg: Msg{Role: "assistant", Content: "wrote release-notes.md"}, FinishReason: "stop"},
	}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: a cut tool call must be re-issued, not returned as an error: %v", err)
	}
	if res.StopReason != "done" || res.Output != "wrote release-notes.md" {
		t.Fatalf("stop_reason/output = %q/%q, want done / the answer (stop_note %q)", res.StopReason, res.Output, res.StopNote)
	}
	if len(client.maxSeen) != 2 {
		t.Fatalf("Chat calls = %d (budgets %v), want 2: the cut step and its ONE re-issue", len(client.maxSeen), client.maxSeen)
	}
	if client.maxSeen[0] != 1024 {
		t.Errorf("first call ran at %d tok, want the step budget 1024", client.maxSeen[0])
	}
	if want := FinalBudgetFor(1024); client.maxSeen[1] < want {
		t.Errorf("re-issue ran at %d tok, want >= the final budget %d", client.maxSeen[1], want)
	}
	if len(res.Calls) != 2 {
		t.Fatalf("calls = %d (%+v), want 2: the cut attempt must appear in results[].calls", len(res.Calls), res.Calls)
	}
	if res.Calls[0].FinishReason != StopToolCallCut || res.Calls[0].MaxTokens != 1024 {
		t.Errorf("calls[0] = %+v, want the cut attempt recorded at the step budget", res.Calls[0])
	}
	if execs != 0 {
		t.Errorf("the tool ran %d times; a refused argument executes nothing", execs)
	}
}

// (ii) Cut again at the final budget → the run stops on the NEW stop reason
// with both budgets and the partial argument size in the note. Never "error".
func TestCutToolCallTwiceStopsWithBothBudgetsNamed(t *testing.T) {
	execs := 0
	client := &scriptedErrClient{errs: map[int]error{0: cutErr(), 1: cutErr()}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err != nil {
		t.Fatalf("Run: a twice-cut tool call is a budget stop, not an error: %v", err)
	}
	if res.StopReason != StopToolCallCut {
		t.Fatalf("stop_reason = %q, want %q", res.StopReason, StopToolCallCut)
	}
	if len(client.maxSeen) != 2 {
		t.Fatalf("Chat calls = %d (budgets %v), want exactly 2 — one re-issue, never a loop", len(client.maxSeen), client.maxSeen)
	}
	for _, want := range []string{"1024", strconv.Itoa(FinalBudgetFor(1024)), "2847"} {
		if !strings.Contains(res.StopNote, want) {
			t.Errorf("stop_note %q does not name %q (both budgets and the partial argument size)", res.StopNote, want)
		}
	}
	if len(res.Calls) != 2 || res.Calls[1].FinishReason != StopToolCallCut {
		t.Errorf("calls = %+v, want both cut attempts recorded", res.Calls)
	}
	if res.Output != "" {
		t.Errorf("output = %q, want empty: nothing was answered", res.Output)
	}
}

// (ii-b) vLLM's shape of the same defect: the completion ARRIVES, cut at the
// budget (finish_reason "length"), carrying a tool call whose arguments are a
// JSON fragment. Same re-issue, and the fragment never executes.
func TestCutToolCallInLengthCutCompletionIsReissued(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path":"release-notes.md","content":"# Release`)}}, FinishReason: "length"},
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
		t.Fatalf("Chat calls = %d (budgets %v), want 2", len(client.seenMax), client.seenMax)
	}
	if want := FinalBudgetFor(1024); client.seenMax[1] < want {
		t.Errorf("re-issue ran at %d tok, want >= the final budget %d", client.seenMax[1], want)
	}
	if execs != 0 {
		t.Errorf("the tool ran %d times; a half-written argument must never be executed", execs)
	}
	// The cut turn must not reach the transcript: a fragment tool call in the
	// history is what the re-issue has to be free of.
	for _, m := range res.Transcript {
		for _, c := range m.ToolCalls {
			if strings.Contains(c.Args, "# Release") && !json.Valid([]byte(c.Args)) {
				t.Errorf("the cut tool call was appended to the transcript: %q", c.Args)
			}
		}
	}
}

// (iii) CONTROL: an unrelated 500 is still an infrastructure-class error. The
// cut classifier must key on the tool-call parse failure, not on the status.
func TestUnrelatedServerErrorStillStopsOnError(t *testing.T) {
	execs := 0
	client := &scriptedErrClient{errs: map[int]error{0: &StatusError{Code: 500, Body: "internal server error"}}}
	res, err := NewLoop(client, writeTool(&execs), 5).WithMaxTokens(1024).Run(context.Background(), "write release-notes.md")
	if err == nil {
		t.Fatalf("Run: an unrelated 500 must surface as an error (res %+v)", res)
	}
	if res.StopReason != "error" {
		t.Errorf("stop_reason = %q, want error", res.StopReason)
	}
	if len(client.maxSeen) != 1 {
		t.Errorf("Chat calls = %d, want 1: an unrelated 500 earns no re-issue", len(client.maxSeen))
	}
}
