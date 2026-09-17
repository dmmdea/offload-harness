package seatrate

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// defaultStepTokens is the tool-step completion budget a seat runs at when
// nothing names one (config agent_max_tokens unset, or a node that publishes
// no seat_budget). The same 1024 Compute falls back to — named here because
// three callers now reach for it.
const defaultStepTokens = 1024

// SeatPolicy is what ONE seat runs contracts at: its tool-step completion
// budget, its thinking policy, and the two measured numbers a wall is sized
// from (its decode rate and its cold load).
//
// It exists because two processes now size the SAME contract on the SAME seat
// (register D-116). The executing NODE builds a policy from its own config and
// its machine-local seat-rates store; the DELEGATOR builds one from what that
// node advertises on health (`seat_rate` / `seat_budget`). They differ only in
// where the numbers come from — the arithmetic below is one copy, so the
// node's wall and the delegator's poll clock cannot drift apart.
type SeatPolicy struct {
	Seat string
	// StepTokens is the planner's per-tool-step completion budget
	// (agent_max_tokens); 0 = defaultStepTokens.
	StepTokens int
	// Thinking is the BOX's thinking policy (agent_thinking); "" = "auto".
	// A contract's own `thinking` overrides it, exactly as the node's loop
	// resolves it (pipeline.thinkingFor).
	Thinking string
	// TokS is the seat's measured decode rate; 0 = no rate, no estimate.
	TokS        float64
	RateSamples int
	RateSource  string // "store" | "config agent_seat_tok_s" | "<node> health seat_rate"
	ColdLoadSec float64
}

// FinalBudgets is THE final-answer / re-pack budget rule for a contract with
// maxSteps steps on a seat whose tool-step budget is stepTokens: the final
// answer runs at FinalBudgetFor(stepTokens) — except on a ONE-step contract,
// whose single completion is a plain step — and a contract carrying an
// output_schema may pay one more completion of the same size for the
// structured re-pack (register D-46 follow-up), charged as an upper bound.
//
// One rule, four readers: the node's loop budget (pipeline.finalBudgetsFor),
// the wall estimate, the final-budget fit (D-95) and the delegator's poll
// bound (D-116).
func FinalBudgets(stepTokens, maxSteps int, hasSchema bool) (final, repack int) {
	if stepTokens <= 0 {
		stepTokens = defaultStepTokens
	}
	if maxSteps <= 0 {
		maxSteps = core.AgentMaxStepsDefault
	}
	final = FinalBudgetFor(stepTokens)
	if maxSteps == 1 {
		final = stepTokens
	}
	if hasSchema {
		repack = final
	}
	return final, repack
}

// InputFor builds the estimate input for contract c on seat policy p: the
// contract's step count, the budgets FinalBudgets implies, and the thinking
// the loop will actually run (the contract's own policy when it names one,
// the box's otherwise). The caller may still raise ColdLoadSec with a load it
// just observed, or set TimeoutSec to the wall the run is actually under.
func InputFor(p SeatPolicy, c core.AgentContract) Input {
	steps := c.MaxSteps
	if steps <= 0 {
		steps = core.AgentMaxStepsDefault
	}
	step := p.StepTokens
	if step <= 0 {
		step = defaultStepTokens
	}
	final, repack := FinalBudgets(step, steps, len(c.OutputSchema) > 0)
	in := Input{
		Seat:         p.Seat,
		TokS:         p.TokS,
		RateSamples:  p.RateSamples,
		RateSource:   p.RateSource,
		ColdLoadSec:  p.ColdLoadSec,
		MaxSteps:     steps,
		StepBudget:   step,
		FinalBudget:  final,
		RepackBudget: repack,
		TimeoutSec:   c.TimeoutSec,
	}
	think := strings.TrimSpace(c.Thinking)
	if think == "" {
		think = strings.TrimSpace(p.Thinking)
	}
	switch strings.ToLower(think) {
	case "", "auto":
		in.ThinkingAuto = true
	case "on":
		in.ThinkingOn = true
	}
	return in
}

// AutoWallFor sizes the wall of a timeout_auto contract on policy p (register
// D-03): the estimate, clamped to the wire bounds
// [core.AgentTimeoutSecDefault, core.AgentTimeoutSecCap]. It returns 0 and the
// estimate (whose Note says why) when the seat has no rate — the node then
// runs the wire default and the delegator then polls to the cap.
//
// The estimate is taken against the wire DEFAULT as the nominal wall, so the
// note reads "wall 300 s vs estimate N s" — what the contract would have run
// under had nobody sized it.
func AutoWallFor(p SeatPolicy, c core.AgentContract) (int, Estimate) {
	in := InputFor(p, c)
	in.TimeoutSec = core.AgentTimeoutSecDefault
	est := Compute(in)
	if est.TotalSec <= 0 {
		return 0, est
	}
	wall := est.TotalSec
	switch {
	case wall < core.AgentTimeoutSecDefault:
		wall = core.AgentTimeoutSecDefault
	case wall > core.AgentTimeoutSecCap:
		wall = core.AgentTimeoutSecCap
	}
	return wall, est
}
