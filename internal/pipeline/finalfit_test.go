package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// budgetClient answers a loop with one tool call and then a final answer, recording the
// completion budget each turn was asked for.
type budgetClient struct{ maxes []int }

func (c *budgetClient) Chat(_ context.Context, _ []agent.Msg, _ []agent.ToolSpec, maxTokens int) (agent.Completion, error) {
	c.maxes = append(c.maxes, maxTokens)
	if len(c.maxes) == 1 {
		return agent.Completion{Msg: agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "list_dir", Args: `{}`}}}, FinishReason: "tool_calls"}, nil
	}
	return agent.Completion{Msg: agent.Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"}, nil
}

func budgetTools() []agent.Tool {
	return []agent.Tool{{
		ToolSpec: agent.ToolSpec{Name: "list_dir", Description: "list", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return "ok", nil },
	}}
}

// TestInstallFinalFitNarrowsTheForcedFinalToTheWallThatIsLeft: the installer is what both
// agent doors call, so a loop it is installed on opens its forced final at the budget the
// REMAINING wall can decode at the seat's rate: a 420 s wall at 17.2 tok/s holds
// (420 - 42) x 17.2 = 6,501 tokens of a configured 8,192, and the run publishes the budget
// and the arithmetic. With no rate there is nothing to fit and the run is untouched.
func TestInstallFinalFitNarrowsTheForcedFinalToTheWallThatIsLeft(t *testing.T) {
	// fit is what the wall buys with remainingSec of it left. The loop reads the live clock, so
	// the assertions below take a window (ten seconds of a loaded machine's delay) and not a point.
	fit := func(remainingSec float64, schema bool) int {
		return seatrate.FitFinalBudget(seatrate.FinalFit{ConfiguredFinal: 8192, RemainingSec: remainingSec, TokS: 17.2, Schema: schema}).Budget
	}
	run := func(tokS float64, schema bool) (*budgetClient, agent.Result) {
		t.Helper()
		client := &budgetClient{}
		loop := agent.NewLoop(client, budgetTools(), 2).WithMaxTokens(2048)
		// 451 s of other terms (the cold load, the tool steps) would leave a 420 s wall nothing if
		// the loop charged them: at the forced final they are spent, and it must not
		InstallFinalFit(loop, seatrate.Estimate{TokS: tokS, OtherSec: 451}, 8192, 420, schema)
		ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
		defer cancel()
		res, err := loop.Run(ctx, "digest")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(client.maxes) != 2 {
			t.Fatalf("want a tool step and a forced final, got budgets %v", client.maxes)
		}
		return client, res
	}

	client, res := run(17.2, false)
	if client.maxes[0] != 2048 {
		t.Errorf("the tool step ran at %d tokens, want the step budget 2048: the fit narrows the final only", client.maxes[0])
	}
	// the clock has run a few milliseconds, so the fit is at most the whole wall's 6,501
	lo, hi := fit(410, false), fit(420, false)
	if got := client.maxes[1]; got < lo || got > hi {
		t.Fatalf("the forced final opened at %d tokens, want %d..%d (the wall that is left, less a tenth, at 17.2 tok/s)", got, lo, hi)
	}
	if res.FinalBudgetFit != client.maxes[1] || !strings.HasPrefix(res.BudgetNote, "final 8192 → ") || !strings.HasSuffix(res.BudgetNote, " s at 17.2 tok/s") {
		t.Errorf("the run must publish the budget it opened at and the arithmetic: %d %q", res.FinalBudgetFit, res.BudgetNote)
	}

	// an output_schema owes a re-pack of the answer inside the same wall: the final may take half
	client, res = run(17.2, true)
	lo, hi = fit(410, true), fit(420, true)
	if got := client.maxes[1]; got < lo || got > hi {
		t.Errorf("with an output_schema the forced final opened at %d tokens, want %d..%d (half of the above)", got, lo, hi)
	}
	if !strings.Contains(res.BudgetNote, "(split with the output_schema re-pack)") {
		t.Errorf("the note must say the wall is split with the re-pack: %q", res.BudgetNote)
	}

	client, res = run(0, false)
	if got := client.maxes[1]; got != 8192 {
		t.Errorf("with no rate the forced final opened at %d tokens, want the configured 8192", got)
	}
	if res.FinalBudgetFit != 0 || res.BudgetNote != "" {
		t.Errorf("with no rate nothing is published: %d %q", res.FinalBudgetFit, res.BudgetNote)
	}
}

// TestInstallFinalFitReturnsTheRunStartFit: the value the installer returns is the fit at the
// instant the wall starts, and it charges everything the run still owes besides its final (the
// cold load, the think block, the tool steps: Estimate.OtherSec) against the wall before it
// converts what is left into tokens. At the forced final those terms are spent, so the loop's
// own fit (above) charges none. 451 s of other terms against a 420 s wall leaves nothing: the
// floor, and a note that says the wall will be the stop.
func TestInstallFinalFitReturnsTheRunStartFit(t *testing.T) {
	loop := agent.NewLoop(&budgetClient{}, budgetTools(), 2)
	est := seatrate.Estimate{TokS: 17.2, OtherSec: 451}

	got := InstallFinalFit(loop, est, 8192, 420, false)
	if got.Budget != seatrate.FinalBudgetFloor || !got.Floored || !strings.Contains(got.Note, "the wall will be the stop") {
		t.Fatalf("run-start fit = %+v, want the floor and the wall-will-be-the-stop note", got)
	}
	// the same wall with room for the other terms: 420 - 100 - 42 = 278 s at 17.2 tok/s
	got = InstallFinalFit(loop, seatrate.Estimate{TokS: 17.2, OtherSec: 100}, 8192, 420, false)
	if got.Budget != 4781 || got.Floored {
		t.Errorf("run-start fit = %+v, want 4781 tokens (278 s at 17.2 tok/s)", got)
	}
	// an output_schema splits the same wall between the final and its re-pack
	got = InstallFinalFit(loop, seatrate.Estimate{TokS: 17.2, OtherSec: 100}, 8192, 420, true)
	if got.Budget != 2390 || !strings.Contains(got.Note, "(split with the output_schema re-pack)") {
		t.Errorf("run-start fit = %+v, want 2390 tokens split with the re-pack", got)
	}
	// no rate, no opinion: the configured budget, no note
	got = InstallFinalFit(loop, seatrate.Estimate{}, 8192, 420, false)
	if got.Budget != 8192 || got.Note != "" {
		t.Errorf("run-start fit with no rate = %+v, want the configured budget and no note", got)
	}
}
