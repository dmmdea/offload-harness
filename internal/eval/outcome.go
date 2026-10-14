package eval

import "fmt"

// AttemptOutcome is the closed taxonomy of what happened to ONE attempt at ONE task in a gate
// run (GATE-2). The gate scores only the attempts that reached a verdict on the task itself
// (pass, task_fail, cheat); every other class is infrastructure or judge noise that is counted
// and reported but never scored, so a flaky box cannot move a score in either direction.
//
// Why not `Outcome`: this package already exports `Outcome`, the per-case result struct of the
// cascade eval (Run, Aggregate, the confhead A/B). The gate's enum is the string type below;
// the exported helpers keep the names the gate spec gave them (AllOutcomes, OutcomeFromCaliper).
type AttemptOutcome string

const (
	// OutcomePass: the attempt finished and the scorer gave it a score (any value in the
	// scale; a "pass" is not necessarily 1).
	OutcomePass AttemptOutcome = "pass"
	// OutcomeTaskFail: the attempt finished and the scorer gave it a score (typically low).
	OutcomeTaskFail AttemptOutcome = "task_fail"
	// OutcomeCheat: the attempt tried to game the scorer. It is scored 0 whatever score it
	// carries, and counted separately per arm.
	OutcomeCheat AttemptOutcome = "cheat"
	// OutcomeInfraError: the run broke (engine crash, out of memory, transport). Not scored.
	OutcomeInfraError AttemptOutcome = "infra_error"
	// OutcomeTimeout: the attempt hit its budget. Not scored: a timeout is a property of the
	// run, and the gate carries no wall-time term (INV-5).
	OutcomeTimeout AttemptOutcome = "timeout"
	// OutcomeJudgeError: the judge could not score the attempt. Not scored.
	OutcomeJudgeError AttemptOutcome = "judge_error"
	// OutcomeNotRun: the attempt never ran (also: Caliper's `not_checked`). Not scored.
	OutcomeNotRun AttemptOutcome = "not_run"
)

// allOutcomes is the fixed order every report, schema and test uses.
var allOutcomes = [...]AttemptOutcome{
	OutcomePass, OutcomeTaskFail, OutcomeCheat, OutcomeInfraError, OutcomeTimeout, OutcomeJudgeError, OutcomeNotRun,
}

// AllOutcomes returns the seven classes in their fixed order (a fresh slice each call).
func AllOutcomes() []AttemptOutcome {
	out := make([]AttemptOutcome, len(allOutcomes))
	copy(out, allOutcomes[:])
	return out
}

// Valid reports whether o is one of the seven classes.
func (o AttemptOutcome) Valid() bool {
	for _, v := range allOutcomes {
		if o == v {
			return true
		}
	}
	return false
}

// Scored reports whether an attempt of this class enters a task's score: pass, task_fail and
// cheat. (A cheat enters as 0.)
func (o AttemptOutcome) Scored() bool {
	return o == OutcomePass || o == OutcomeTaskFail || o == OutcomeCheat
}

// OutcomeFromCaliper maps the outcome word of a Caliper record to this taxonomy. The six
// classes it shares with the gate map to themselves; its `not_checked` (the check never ran)
// is the gate's `not_run`. Any other word is unknown (false): a record the gate cannot place
// must be refused by the caller, never guessed into a class.
func OutcomeFromCaliper(s string) (AttemptOutcome, bool) {
	switch s {
	case "pass", "task_fail", "cheat", "infra_error", "timeout", "judge_error":
		return AttemptOutcome(s), true
	case "not_checked":
		return OutcomeNotRun, true
	}
	return "", false
}

// Refusal is the error type of every refusal in the gate: a freeze that would write a
// pre-registration the gate cannot honour, a baseline that is not the frozen bytes, an input
// the gate will not judge. A refusal never carries a verdict. The gate verb maps it to exit 2.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refusef(format string, a ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, a...)}
}
