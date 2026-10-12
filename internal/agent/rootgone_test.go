package agent

// Register C-79: a run whose workspace root is gone must end, typed, instead of
// feeding "workspace root unavailable" back to the seat and letting it write a
// final answer from nothing. These tests pin the three halves: the tools report
// the loss as a *RootGoneError, the loop ends the run on it (and only on it), and
// the report is not mistaken for a file that is merely missing.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatFunc is a Client written as a function, so a test can act BETWEEN model
// turns the way a sweep or a cleaner does.
type chatFunc func(ctx context.Context, msgs []Msg, specs []ToolSpec, maxTokens int) (Completion, error)

func (f chatFunc) Chat(ctx context.Context, msgs []Msg, specs []ToolSpec, maxTokens int) (Completion, error) {
	return f(ctx, msgs, specs, maxTokens)
}

// rootWithNotes is a workspace root holding one context document, like the job
// directory's context/ a delegated run reads.
func rootWithNotes(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "context")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("the answer is 42"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func vanish(t *testing.T, root string) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("removing the root: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the root is still there after the removal: %v", err)
	}
}

func toolTurn(calls ...ToolCall) Completion {
	return Completion{Msg: Msg{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls"}
}

func answerTurn(text string) Completion {
	return Completion{Msg: Msg{Role: "assistant", Content: text}, FinishReason: "stop"}
}

// A tool that cannot open the root says so with a type, keeps the words the seat
// has always seen, and still unwraps to the operating system's own error.
func TestScopeOpenReportsAnUnopenableRootAsRootGone(t *testing.T) {
	root := rootWithNotes(t)
	vanish(t, root)
	s := &scope{root: root}
	_, _, err := s.open("notes.md")
	var gone *RootGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("open on a vanished root returned %v (%T), want a *RootGoneError", err, err)
	}
	if gone.Root != root {
		t.Errorf("Root = %q, want %q", gone.Root, root)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the error does not unwrap to the operating system's not-exist error: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "workspace root unavailable: ") || !strings.Contains(err.Error(), root) {
		t.Errorf("Error() = %q: the seat's observation must keep its opening words and name the root", err.Error())
	}
	if strings.Contains(gone.Cause(), root) || gone.Cause() == "" {
		t.Errorf("Cause() = %q, want the operating system's words without the path", gone.Cause())
	}
}

// Every tool that opens the workspace goes through the same door, so each reports
// the loss the same way: the read tools, the offload digest, the write door's
// tools, and the plan scratchpad.
func TestEveryWorkspaceToolReportsAVanishedRootAsRootGone(t *testing.T) {
	root := rootWithNotes(t)
	offload := func(_ context.Context, _, _ string, _ map[string]any) (string, error) { return "a summary", nil }
	read, err := ReadOnlyTools(root, offload, nil)
	if err != nil {
		t.Fatal(err)
	}
	write, err := WriteTools(root, NewPolicy(true, nil).WithWritePosture(true, true))
	if err != nil {
		t.Fatal(err)
	}
	tools := append(read, write...)
	vanish(t, root)
	for _, tc := range []struct{ tool, args string }{
		{"list_dir", `{"path":"."}`},
		{"read_file", `{"path":"notes.md"}`},
		{"search_files", `{"pattern":"answer"}`},
		{"summarize_file", `{"path":"notes.md"}`},
		{"write_file", `{"path":"out.txt","content":"c"}`},
		{"edit_file", `{"path":"notes.md","old_string":"42","new_string":"43"}`},
		{"delete_file", `{"path":"notes.md"}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			tool := findTool(tools, tc.tool)
			if tool == nil {
				t.Fatalf("%s is not registered", tc.tool)
			}
			_, err := tool.Exec(context.Background(), tc.args)
			var gone *RootGoneError
			if !errors.As(err, &gone) {
				t.Fatalf("%s on a vanished root returned %v (%T), want a *RootGoneError", tc.tool, err, err)
			}
			if gone.Root != root {
				t.Errorf("Root = %q, want %q", gone.Root, root)
			}
		})
	}
	t.Run("update_plan", func(t *testing.T) {
		_, err := updatePlanTool(root).Exec(context.Background(), `{"plan":"1. read notes"}`)
		var gone *RootGoneError
		if !errors.As(err, &gone) {
			t.Fatalf("update_plan on a vanished worktree returned %v (%T), want a *RootGoneError", err, err)
		}
	})
}

// A file that is missing inside a root that opens is the model's mistake, not the
// box's: it is not a RootGoneError, from any tool.
func TestAMissingFileInsideAnOpenRootIsNotRootGone(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tool, args string }{
		{"read_file", `{"path":"nosuch.md"}`},
		{"list_dir", `{"path":"nosuch"}`},
	} {
		_, err := findTool(tools, tc.tool).Exec(context.Background(), tc.args)
		if err == nil {
			t.Fatalf("%s on a missing path did not fail", tc.tool)
		}
		var gone *RootGoneError
		if errors.As(err, &gone) {
			t.Errorf("%s on a missing path inside an open root was reported as a root gone: %v", tc.tool, err)
		}
	}
}

// The run ends on the first tool report, before the seat is asked again: the seat
// is never given the chance to write a final answer from nothing. What happened
// stays on the record: one failed effect, the steps taken, the typed error.
func TestLoopEndsTheRunWhenAToolReportsTheRootGone(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		if calls == 1 {
			vanish(t, root) // the sweep takes the job directory while the seat decides what to read
			return toolTurn(tc("c1", "read_file", `{"path":"notes.md"}`)), nil
		}
		return answerTurn("I could not read the file."), nil
	})
	res, err := NewLoop(client, tools, 5).Run(context.Background(), "what is the answer?")
	var gone *RootGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("Run returned err = %v; the seat was asked %d time(s) and answered %q — a run whose root is gone must end with a *RootGoneError", err, calls, res.Output)
	}
	if gone.Root != root {
		t.Errorf("Root = %q, want %q", gone.Root, root)
	}
	if calls != 1 {
		t.Errorf("the seat was asked %d times, want 1: the run must not give the seat another turn after its tool reported the root gone", calls)
	}
	if res.StopReason != "error" || res.Steps != 1 || res.Output != "" {
		t.Errorf("stop_reason=%q steps=%d output=%q, want an error stop after one step with no answer", res.StopReason, res.Steps, res.Output)
	}
	if len(res.Effects) != 1 || res.Effects[0].Status != EffectFailed || res.Effects[0].Tool != "read_file" ||
		!strings.Contains(res.Effects[0].Note, "workspace root unavailable") {
		t.Errorf("effects = %+v, want the one failed read_file with the tool's own words", res.Effects)
	}
}

// The same holds when tool capping is switched off (WithToolTimeout(0)), which runs
// the tool inline instead of behind the cap's goroutine: both paths report it.
func TestLoopEndsTheRunWhenAnUncappedToolReportsTheRootGone(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		if calls == 1 {
			vanish(t, root)
			return toolTurn(tc("c1", "read_file", `{"path":"notes.md"}`)), nil
		}
		return answerTurn("I could not read the file."), nil
	})
	_, err = NewLoop(client, tools, 5).WithToolTimeout(0).Run(context.Background(), "what is the answer?")
	var gone *RootGoneError
	if !errors.As(err, &gone) || calls != 1 {
		t.Fatalf("Run returned err = %v after %d seat call(s), want a *RootGoneError after 1", err, calls)
	}
}

// Several calls in one turn each get their record before the run ends: the effect
// ledger keeps "one record per requested call", and the transcript the caller
// reads keeps every result.
func TestLoopRecordsEveryCallOfTheTurnThatHitTheGoneRoot(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		vanish(t, root)
		return toolTurn(tc("c1", "list_dir", `{"path":"."}`), tc("c2", "read_file", `{"path":"notes.md"}`)), nil
	})
	res, err := NewLoop(client, tools, 5).Run(context.Background(), "what is the answer?")
	var gone *RootGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("Run returned err = %v, want a *RootGoneError", err)
	}
	if calls != 1 {
		t.Errorf("the seat was asked %d times, want 1", calls)
	}
	if len(res.Effects) != 2 || res.Effects[0].Status != EffectFailed || res.Effects[1].Status != EffectFailed {
		t.Errorf("effects = %+v, want both calls of the turn recorded as failed", res.Effects)
	}
	results := 0
	for _, m := range res.Transcript {
		if m.Role == "tool" {
			results++
		}
	}
	if results != 2 {
		t.Errorf("the transcript holds %d tool results, want 2", results)
	}
}

// Only the tool's report ends the run. A tool that fails on a file inside a root
// that opens is an observation, the seat reacts, and the run finishes.
func TestLoopKeepsGoingWhenAToolFailsInsideAnOpenRoot(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		if calls == 1 {
			return toolTurn(tc("c1", "read_file", `{"path":"nosuch.md"}`)), nil
		}
		return answerTurn("there is no such file"), nil
	})
	res, err := NewLoop(client, tools, 5).Run(context.Background(), "what is the answer?")
	if err != nil {
		t.Fatalf("Run: %v — a missing file inside an open root must not end the run", err)
	}
	if res.StopReason != "done" || calls != 2 || res.Output != "there is no such file" {
		t.Errorf("stop_reason=%q calls=%d output=%q, want the seat to react to the failed read and finish", res.StopReason, calls, res.Output)
	}
}

// The signal is the tool's report and nothing else: a root that vanishes after the
// seat has read what it needed, and that no tool touches again, does not turn a
// finished answer into a defer.
func TestLoopDoesNotEndARunWhoseRootVanishedAfterItsLastRead(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		switch calls {
		case 1:
			return toolTurn(tc("c1", "read_file", `{"path":"notes.md"}`)), nil
		case 2:
			vanish(t, root) // after the read, before the answer; no tool is called again
			return answerTurn("the answer is 42"), nil
		}
		return Completion{}, errors.New("unexpected extra call")
	})
	res, err := NewLoop(client, tools, 5).Run(context.Background(), "what is the answer?")
	if err != nil {
		t.Fatalf("Run: %v — no tool reported the loss, so the finished answer stands", err)
	}
	if res.StopReason != "done" || res.Output != "the answer is 42" {
		t.Errorf("stop_reason=%q output=%q, want the finished answer", res.StopReason, res.Output)
	}
}

// A setup action (a context read replayed before the first turn) that finds the
// root gone ends the run before the seat is asked at all: otherwise the seat
// would answer from a transcript whose only reads failed, with no tool call of
// its own for the loop to end on.
func TestSetupReplayEndsTheRunWhenTheRootIsGone(t *testing.T) {
	root := rootWithNotes(t)
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	vanish(t, root)
	calls := 0
	client := chatFunc(func(context.Context, []Msg, []ToolSpec, int) (Completion, error) {
		calls++
		return answerTurn("I could not read the file."), nil
	})
	res, err := NewLoop(client, tools, 5).WithSetupActions(setupActs(`read_file {"path":"notes.md"}`)).
		Run(context.Background(), "what is the answer?")
	var gone *RootGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("Run returned err = %v; the seat was asked %d time(s) and answered %q — a setup read that finds the root gone must end the run", err, calls, res.Output)
	}
	if calls != 0 || res.Steps != 0 || res.StopReason != "error" {
		t.Errorf("calls=%d steps=%d stop_reason=%q, want the run ended before any model turn", calls, res.Steps, res.StopReason)
	}
	if len(res.Effects) != 1 || !res.Effects[0].Setup || res.Effects[0].Status != EffectFailed {
		t.Errorf("effects = %+v, want the one failed setup read", res.Effects)
	}
}

// The pipeline asks the tools' own question where no tool call is there to ask it: the
// write door opening on a tree lost before the run's first call, and closing on one lost
// after its last. The answer is the tools' answer, byte for byte, and the handle it opens
// to ask is closed again: a handle left open would hold the directory against its removal
// on Windows, which the removal below would show.
func TestCheckWorkspaceRootAnswersWhatTheToolsAnswer(t *testing.T) {
	root := rootWithNotes(t)
	if gone := CheckWorkspaceRoot(root); gone != nil {
		t.Fatalf("a root that opens was reported gone: %v", gone)
	}
	vanish(t, root) // fails on Windows when the check above left its handle open
	gone := CheckWorkspaceRoot(root)
	if gone == nil {
		t.Fatal("a vanished root was not reported")
	}
	if gone.Root != root {
		t.Errorf("Root = %q, want %q", gone.Root, root)
	}
	if !errors.Is(gone, fs.ErrNotExist) {
		t.Errorf("the report does not unwrap to the operating system's not-exist error: %v", gone)
	}
	_, _, err := (&scope{root: root}).open("notes.md")
	if err == nil || gone.Error() != err.Error() {
		t.Errorf("CheckWorkspaceRoot says %q, a tool says %v: one question, one answer", gone.Error(), err)
	}
}
