package pipeline

import (
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// InstallFinalFit fits a run's final answer to its wall (register D-95): the ONE place
// the two agent doors, the delegation door's node and the local agent_run door, install
// the fit on their loop, so the arithmetic they publish and run cannot drift apart
// (register D-134, the cure D-102 left open; internal/mcpserver's
// TestAgentRunSizesTheRunStartFinalLikeTheNodeDoes compares what each door publishes).
//
// It does two things. It installs on loop the fit the loop applies at the forced final
// step, off the live clock: the largest budget the wall that is LEFT can decode at the
// seat's measured rate, never above the configured final, floored at
// seatrate.FinalBudgetFloor (see seatrate.FitFinalBudget). The cold load, the think block
// and the tool steps are already spent by then, so the loop's fit charges none of them.
// And it returns the same fit as it stands when the wall starts, from est (the run's
// seatrate.Estimate: its rate and the non-final terms it still owes, Estimate.OtherSec)
// and the whole wall, for the caller to publish as final_budget_fit / budget_note and to
// size anything else off.
//
// configuredFinal is the budget the ONE rule asks for (seatrate.FinalBudgets) and the cap
// of both fits. schema says the same wall also has to hold the structured re-pack of the
// answer, which halves what the final may take. A seat with no rate (est.TokS 0) has
// nothing to fit: the loop is left as it was and the returned fit is the configured budget
// with no note.
//
// Whether the loop's fit narrows anything depends on the deadline the loop is run under.
// The agent_run door runs it under a context deadline that IS the wall, so a run that
// reaches its forced final with less wall than the configured final needs is held to what
// the wall can decode, instead of being cut at the deadline mid-answer. The node runs its
// loop under the liveness ceiling (ADR 0055: its wall is an expectation, not a deadline),
// which is at least twice the wall, so there the loop's fit has room until a run nears the
// ceiling and the returned run-start fit is a published prediction.
func InstallFinalFit(loop *agent.Loop, est seatrate.Estimate, configuredFinal, wallSec int, schema bool) seatrate.FinalFitResult {
	loop.WithFinalBudgetFit(func(configured int, remaining time.Duration) (int, string) {
		// At the final turn the cold load, the think block and the tool steps
		// are already spent, so only the answer (and its re-pack) still has to
		// fit — OtherSec is 0 here on purpose.
		f := seatrate.FitFinalBudget(seatrate.FinalFit{
			ConfiguredFinal: configured,
			RemainingSec:    remaining.Seconds(),
			TokS:            est.TokS,
			Schema:          schema,
		})
		return f.Budget, f.Note
	})
	return seatrate.FitFinalBudget(seatrate.FinalFit{
		ConfiguredFinal: configuredFinal,
		RemainingSec:    float64(wallSec),
		OtherSec:        float64(est.OtherSec),
		TokS:            est.TokS,
		Schema:          schema,
	})
}
