package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// singleShotClient returns one scripted completion (a final assistant message
// with no tool calls, so the loop stops after one step) and records the
// transcript it was called with so tests can assert what the loop fed it.
type singleShotClient struct {
	output string
	err    error
	seen   [][]Msg
}

func (c *singleShotClient) Chat(_ context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
	c.seen = append(c.seen, append([]Msg(nil), msgs...))
	if c.err != nil {
		return Completion{}, c.err
	}
	return Completion{Msg: Msg{Role: "assistant", Content: c.output}, FinishReason: "stop"}, nil
}

// loopWith builds a minimal Loop around a fake client for orchestration tests.
func loopWith(c Client) *Loop { return NewLoop(c, nil, 4) }

// userMsgs returns the Content of every user-role message the client saw on its
// first (and here only) call.
func userMsgs(c *singleShotClient) []string {
	var out []string
	if len(c.seen) == 0 {
		return out
	}
	for _, m := range c.seen[0] {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestRunTwoTierHandoff: the architect's plan becomes the editor's SOLE user
// message; the original objective and any architect-side history do NOT appear
// in the editor's transcript (aider one-shot move_back_cur_messages semantics).
func TestRunTwoTierHandoff(t *testing.T) {
	const objective = "refactor the parser to support comments"
	const plan = "PLAN: 1. edit internal/parse/lexer.go to skip # lines. 2. add a test in lexer_test.go."

	archClient := &singleShotClient{output: plan}
	editClient := &singleShotClient{output: "done: applied the plan"}
	architect := loopWith(archClient)
	editor := loopWith(editClient)

	res, err := RunTwoTier(context.Background(), objective, architect, editor)
	if err != nil {
		t.Fatalf("RunTwoTier: %v", err)
	}

	// The editor's Output is what's returned (happy path).
	if res.Output != "done: applied the plan" {
		t.Errorf("output = %q, want the editor's output", res.Output)
	}

	// The plan must be the editor's SOLE user message.
	eu := userMsgs(editClient)
	if len(eu) != 1 {
		t.Fatalf("editor saw %d user messages, want exactly 1 (the plan): %#v", len(eu), eu)
	}
	if eu[0] != plan {
		t.Errorf("editor's user message = %q, want the architect's plan %q", eu[0], plan)
	}

	// The original objective must NOT appear anywhere in the editor's transcript.
	for _, m := range editClient.seen[0] {
		if strings.Contains(m.Content, objective) {
			t.Errorf("editor transcript leaked the original objective: %q", m.Content)
		}
	}

	// The architect saw the original objective (it plans from it).
	au := userMsgs(archClient)
	if len(au) == 0 || au[len(au)-1] != objective {
		t.Errorf("architect did not receive the original objective as its user message: %#v", au)
	}
}

// TestRunTwoTierFallbackDegeneratePlan: an empty/degenerate architect plan must
// fall back to a single-model run of the ORIGINAL objective on the editor loop.
func TestRunTwoTierFallbackDegeneratePlan(t *testing.T) {
	const objective = "add a --verbose flag to the CLI"

	archClient := &singleShotClient{output: "  no  "} // degenerate: too short after trim
	editClient := &singleShotClient{output: "done: added the flag"}
	architect := loopWith(archClient)
	editor := loopWith(editClient)

	res, err := RunTwoTier(context.Background(), objective, architect, editor)
	if err != nil {
		t.Fatalf("RunTwoTier: %v", err)
	}
	if res.Output != "done: added the flag" {
		t.Errorf("output = %q, want the editor's fallback output", res.Output)
	}
	// Fallback: the editor must have run the ORIGINAL objective.
	eu := userMsgs(editClient)
	if len(eu) != 1 || eu[0] != objective {
		t.Fatalf("editor did not run the original objective on fallback: %#v", eu)
	}
	if res.Fallback != FallbackDegeneratePlan {
		t.Errorf("Fallback = %q, want %q", res.Fallback, FallbackDegeneratePlan)
	}
}

// TestRunTwoTierFallbackArchitectError: if architect.Run errors, fall back to a
// single-model run of the ORIGINAL objective on the editor — no panic.
func TestRunTwoTierFallbackArchitectError(t *testing.T) {
	const objective = "fix the off-by-one in the pager"

	archClient := &singleShotClient{err: errors.New("boom: planner unreachable")}
	editClient := &singleShotClient{output: "done: fixed the pager"}
	architect := loopWith(archClient)
	editor := loopWith(editClient)

	res, err := RunTwoTier(context.Background(), objective, architect, editor)
	if err != nil {
		t.Fatalf("RunTwoTier should not surface the architect error (it falls back): %v", err)
	}
	if res.Output != "done: fixed the pager" {
		t.Errorf("output = %q, want the editor's fallback output", res.Output)
	}
	eu := userMsgs(editClient)
	if len(eu) != 1 || eu[0] != objective {
		t.Fatalf("editor did not run the original objective on architect-error fallback: %#v", eu)
	}
	if res.Fallback != FallbackArchitectError {
		t.Errorf("Fallback = %q, want %q", res.Fallback, FallbackArchitectError)
	}
}

// TestRunTwoTierHappyPathNote: the two-tier (non-fallback) path records that the
// plan-then-execute path ran, so the caller can log it.
func TestRunTwoTierHappyPathNote(t *testing.T) {
	archClient := &singleShotClient{output: "PLAN: do the thing in file foo.go by adding a function bar()."}
	editClient := &singleShotClient{output: "done"}
	res, err := RunTwoTier(context.Background(), "objective", loopWith(archClient), loopWith(editClient))
	if err != nil {
		t.Fatalf("RunTwoTier: %v", err)
	}
	if res.Fallback != FallbackNone {
		t.Errorf("Fallback = %q, want %q (two-tier path ran)", res.Fallback, FallbackNone)
	}
}
