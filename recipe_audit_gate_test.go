package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/recipe"
)

// The half of the verb that runs the A/B and decides: the seat is the fake of recipe_audit_cmd_test.go.

// report runs the audit to its end and returns what it printed.
func (e *raEnv) report(t *testing.T, extra ...string) recipeAuditReport {
	t.Helper()
	out, errs, err := e.run(e.args(extra...))
	if err != nil {
		t.Fatalf("the audit failed: %v\n%s", err, errs)
	}
	var rep recipeAuditReport
	if err := decodeJSONStrict([]byte(out), &rep); err != nil {
		t.Fatalf("the report is not JSON of the declared shape: %v\n%s", err, out)
	}
	return rep
}

// raScript gives each arm a pass or a fail, with steps.
func raScript(incPass, candPass bool, incSteps, candSteps int) func(string, core.AgentContract) (core.AgentWireResult, error) {
	return func(arm string, _ core.AgentContract) (core.AgentWireResult, error) {
		if arm == recipeArmCandidate {
			return raOutcome(candPass, candSteps), nil
		}
		return raOutcome(incPass, incSteps), nil
	}
}

// Publish on a gate ADOPT with steps no worse.
func TestRecipeAuditPublishesOnAnAdoptWithNonWorseSteps(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.fake.script = raScript(false, true, 5, 3) // the recipe fixes every task and takes fewer steps
	rep := e.report(t)
	if rep.Gate.Verdict != eval.VerdictAdopt {
		t.Fatalf("gate verdict %s (%s), want ADOPT", rep.Gate.Verdict, rep.Gate.Reason)
	}
	if rep.Status != recipe.StatusPublished || rep.StatusBefore != recipe.StatusProposed {
		t.Fatalf("status %s (was %s), want published (was proposed)", rep.Status, rep.StatusBefore)
	}
	if !rep.Steps.NonWorse || rep.Steps.Incumbent != 5 || rep.Steps.Candidate != 3 || rep.Steps.Pairs != 16 {
		t.Fatalf("steps %+v, want 5 against 3 over 16 pairs", rep.Steps)
	}
	got := e.saved(t)
	if got.Status != recipe.StatusPublished || got.Audit == nil {
		t.Fatalf("the store holds %+v", got)
	}
	if a := got.Audit; a.Verdict != eval.VerdictAdopt || a.BaselineHash != rep.Gate.BaselineHash || a.EraMarker != rep.Gate.EraMarker || a.Seat != "seat-a" || a.StepsIncumbent != 5 || a.StepsCandidate != 3 {
		t.Fatalf("the audit record %+v does not match the gate result", a)
	}
	if e.fake.count(recipeArmIncumbent) != 16 || e.fake.count(recipeArmCandidate) != 16 {
		t.Fatalf("the arms ran %d and %d times, want 16 each", e.fake.count(recipeArmIncumbent), e.fake.count(recipeArmCandidate))
	}
	if e.seatAsked != "seat-a" || !reflect.DeepEqual(e.candidateRules, got.Rules) {
		t.Fatalf("the candidate arm ran seat %q with %+v, want the recipe's own rule set on seat-a", e.seatAsked, e.candidateRules)
	}
}

// A better pass rate bought with more steps is not a publish.
func TestRecipeAuditDemotesWhenTheStepsGotWorse(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.fake.script = raScript(false, true, 3, 5)
	rep := e.report(t)
	if rep.Gate.Verdict != eval.VerdictAdopt {
		t.Fatalf("the precondition is a gate ADOPT, got %s (%s)", rep.Gate.Verdict, rep.Gate.Reason)
	}
	if rep.Status != recipe.StatusDemoted || rep.Steps.NonWorse {
		t.Fatalf("status %s, non_worse %v: want demoted on worse steps", rep.Status, rep.Steps.NonWorse)
	}
	if got := e.saved(t); got.Status != recipe.StatusDemoted || got.Audit == nil || got.Audit.Verdict != eval.VerdictAdopt || got.Audit.StepsNoWorse() {
		t.Fatalf("the store holds %+v", got)
	}
}

// Anything but ADOPT demotes: a block, and a tie (the incumbent keeps every tie).
func TestRecipeAuditDemotesOnABlockAndOnAnInconclusive(t *testing.T) {
	for name, c := range map[string]struct {
		inc, cand bool
		verdict   string
	}{
		"block":        {true, false, eval.VerdictBlock},
		"inconclusive": {true, true, eval.VerdictInconclusive},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRAEnv(t)
			e.use(t, nil)
			e.fake.script = raScript(c.inc, c.cand, 3, 3)
			rep := e.report(t)
			if rep.Gate.Verdict != c.verdict {
				t.Fatalf("gate verdict %s (%s), want %s", rep.Gate.Verdict, rep.Gate.Reason, c.verdict)
			}
			if rep.Status != recipe.StatusDemoted {
				t.Fatalf("status %s, want demoted", rep.Status)
			}
			if got := e.saved(t); got.Status != recipe.StatusDemoted || got.Audit == nil || got.Audit.Verdict != c.verdict {
				t.Fatalf("the store holds %+v", got)
			}
		})
	}
}

// The gate input the verb builds is the one gate verdict reads: written out with --input-out and judged
// by gateMain, the result is the one the audit printed, byte for byte.
func TestRecipeAuditGateInputIsWhatGateVerdictReads(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.freeze(t, func(p *eval.PreReg) {
		p.Efficiency = []eval.PreRegEfficiency{{Name: "forwarded_tokens", Class: eval.EffTokens}}
	})
	e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
		w := raOutcome(arm == recipeArmCandidate, 3)
		w.PrefillTokens = 900
		if arm == recipeArmCandidate {
			w.PrefillTokens = 400
		}
		return w, nil
	}
	rep := e.report(t)
	var gout, gerr bytes.Buffer
	if err := gateMain([]string{"verdict", "--baseline", e.baseline, "--input", e.inputOut}, &gout, &gerr); err != nil {
		t.Fatalf("gate verdict refused the input the audit wrote: %v\n%s", err, gerr.String())
	}
	var viaGate eval.GateResult
	if err := decodeJSONStrict(gout.Bytes(), &viaGate); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(rep.Gate)
	b, _ := json.Marshal(viaGate)
	if !bytes.Equal(a, b) {
		t.Fatalf("the audit's gate result differs from gate verdict's:\n audit %s\n gate  %s", a, b)
	}
	if len(rep.Gate.Efficiency) != 1 || rep.Gate.Efficiency[0].N != 16 {
		t.Fatalf("the declared tokens metric was not carried into the input: %+v", rep.Gate.Efficiency)
	}
	if rep.Gate.GateBuildSHA256 == "" {
		t.Fatal("the result does not name the build that ran the gate")
	}
}

// How a wire result becomes a gate attempt: the acceptance DSL judges a finished run, and a defer is
// told apart by who is at fault.
func TestRecipeAuditMapsWireResultsToGateOutcomes(t *testing.T) {
	c := core.AgentContract{SchemaVersion: core.AgentWireSchemaVersion, Goal: "g", Acceptance: []string{"contains:DONE"}}
	deferred := func(class, reason, stop string) core.AgentWireResult {
		return core.AgentWireResult{Deferred: true, DeferClass: class, Reason: reason, StopReason: stop}
	}
	cases := []struct {
		name    string
		wire    core.AgentWireResult
		err     error
		outcome eval.AttemptOutcome
		score   float64 // -1: none
	}{
		{"acceptance passes", raOutcome(true, 2), nil, eval.OutcomePass, 1},
		{"acceptance fails", raOutcome(false, 2), nil, eval.OutcomeTaskFail, 0},
		{"the runner failed", core.AgentWireResult{}, errors.New("boom"), eval.OutcomeInfraError, -1},
		{"infrastructure defer", deferred(core.DeferClassInfrastructure, "endpoint down", "error"), nil, eval.OutcomeInfraError, -1},
		{"config defer", deferred(core.DeferClassConfig, "no seat", ""), nil, eval.OutcomeInfraError, -1},
		{"capacity defer", deferred(core.DeferClassCapacity, "full", ""), nil, eval.OutcomeInfraError, -1},
		{"step budget spent", deferred(core.DeferClassBudget, "step budget exhausted (12 steps)", "budget"), nil, eval.OutcomeTaskFail, 0},
		{"wall spent", deferred(core.DeferClassBudget, "context deadline exceeded", "error"), nil, eval.OutcomeTimeout, -1},
		// The seat spent its completion budget and did not finish: a verdict about the task, as a spent
		// step budget is, so a recipe that fixes exactly these gets credit for it.
		{"tool call cut at the completion budget", deferred(core.DeferClassBudget, "tool-call argument cut at the completion budget twice", agent.StopToolCallCut), nil, eval.OutcomeTaskFail, 0},
		{"reasoning starved the final answer", deferred(core.DeferClassBudget, "empty final answer after 4 steps and 900 completion tokens: reasoning_starved", agent.StopReasoningStarved), nil, eval.OutcomeTaskFail, 0},
		// A clock or a caller ended the run: nothing was learned about the task.
		{"wall ceiling", deferred(core.DeferClassBudget, "wall timeout after 600s (the caller's deadline, not this node's ceiling)", ""), nil, eval.OutcomeTimeout, -1},
		{"caller gave up", deferred(core.DeferClassBudget, "agent loop: canceled (the parent context ended, the caller gave up, or this box is draining)", ""), nil, eval.OutcomeTimeout, -1},
		{"re-pack skipped for the clock", deferred(core.DeferClassBudget, "structured re-pack skipped: the wall's remaining time could not buy the answer", "final"), nil, eval.OutcomeTimeout, -1},
		{"abstention", deferred(core.DeferClassAbstention, "output failed schema", "final"), nil, eval.OutcomeTaskFail, 0},
		{"defer with no class", deferred("", "unknown", ""), nil, eval.OutcomeTaskFail, 0},
	}
	for _, tc := range cases {
		outcome, score := recipeJudge(c, tc.wire, tc.err)
		if outcome != tc.outcome {
			t.Errorf("%s: outcome %s, want %s", tc.name, outcome, tc.outcome)
		}
		switch {
		case tc.score < 0 && score != nil:
			t.Errorf("%s: score %v, want none", tc.name, *score)
		case tc.score >= 0 && (score == nil || *score != tc.score):
			t.Errorf("%s: score %v, want %v", tc.name, score, tc.score)
		}
	}
}

// A recipe that fixes the runs on which the incumbent spent its completion budget without finishing
// earns the credit: those runs are the incumbent's failures, not pairs the gate drops.
func TestRecipeAuditCreditsARecipeThatFixesASpentCompletionBudget(t *testing.T) {
	for name, stop := range map[string]string{"tool_call_cut": agent.StopToolCallCut, "reasoning_starved": agent.StopReasoningStarved} {
		t.Run(name, func(t *testing.T) {
			e := newRAEnv(t)
			e.use(t, nil)
			e.fake.script = func(arm string, _ core.AgentContract) (core.AgentWireResult, error) {
				if arm == recipeArmCandidate {
					return raOutcome(true, 3), nil
				}
				w := raOutcome(false, 5)
				w.Output, w.Deferred, w.DeferClass, w.Reason, w.StopReason = "", true, core.DeferClassBudget, "the completion budget was spent", stop
				return w, nil
			}
			rep := e.report(t)
			if rep.Gate.Verdict != eval.VerdictAdopt || rep.Status != recipe.StatusPublished {
				t.Fatalf("gate %s (%s), status %s: a candidate that passes 16 of 16 tasks the incumbent could not finish should be published", rep.Gate.Verdict, rep.Gate.Reason, rep.Status)
			}
		})
	}
}

// A tie in steps is not worse: the policy is "no worse than the incumbent", so a candidate that wins
// the gate on the same mean steps is published.
func TestRecipeAuditPublishesWhenTheStepsTie(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.fake.script = raScript(false, true, 3, 3)
	rep := e.report(t)
	if rep.Gate.Verdict != eval.VerdictAdopt {
		t.Fatalf("the precondition is a gate ADOPT, got %s (%s)", rep.Gate.Verdict, rep.Gate.Reason)
	}
	if rep.Status != recipe.StatusPublished || !rep.Steps.NonWorse || rep.Steps.Incumbent != 3 || rep.Steps.Candidate != 3 {
		t.Fatalf("status %s, steps %+v: want published on 3 against 3", rep.Status, rep.Steps)
	}
	if got := e.saved(t); got.Status != recipe.StatusPublished {
		t.Fatalf("the store holds %+v", got)
	}
}

// Steps are compared over the tasks both arms finished, and only a pass or a task_fail attempt is a
// finished one: a run that ended in an infrastructure defer says nothing about how many steps the
// task takes, however many steps it reports.
func TestRecipeAuditComparesStepsOnlyOverFinishedAttempts(t *testing.T) {
	infra := func() core.AgentWireResult {
		w := raOutcome(false, 40)
		w.Output, w.Deferred, w.DeferClass, w.Reason = "", true, core.DeferClassInfrastructure, "endpoint down"
		return w
	}
	t.Run("a task one arm never finished is not a pair", func(t *testing.T) {
		e := newRAEnv(t)
		e.use(t, nil)
		e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
			if arm == recipeArmCandidate {
				if c.Goal == "task-05" {
					return infra(), nil
				}
				return raOutcome(true, 3), nil
			}
			return raOutcome(false, 5), nil
		}
		rep := e.report(t)
		if rep.Steps.Pairs != 15 || rep.Steps.Incumbent != 5 || rep.Steps.Candidate != 3 {
			t.Fatalf("steps %+v: want 5 against 3 over the 15 tasks both arms finished", rep.Steps)
		}
	})
	t.Run("an attempt that did not finish does not count toward its task", func(t *testing.T) {
		e := newRAEnv(t)
		e.use(t, nil)
		seen := map[string]int{}
		e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
			if arm == recipeArmIncumbent {
				return raOutcome(false, 5), nil
			}
			seen[c.Goal]++
			if seen[c.Goal] == 1 { // the first attempt of every task is an infrastructure defer
				return infra(), nil
			}
			return raOutcome(true, 3), nil
		}
		rep := e.report(t, "--attempts", "2")
		if rep.Steps.Pairs != 16 || rep.Steps.Incumbent != 5 || rep.Steps.Candidate != 3 {
			t.Fatalf("steps %+v: want 5 against 3 over 16 tasks, the infrastructure defers not counted", rep.Steps)
		}
	})
}

// --attempts is honoured: every task runs that many times on each arm.
func TestRecipeAuditRunsEveryAttemptOnBothArms(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.fake.script = raScript(false, true, 5, 3)
	e.report(t, "--attempts", "2")
	if e.fake.count(recipeArmIncumbent) != 32 || e.fake.count(recipeArmCandidate) != 32 {
		t.Fatalf("the arms ran %d and %d times, want 32 each (16 tasks, 2 attempts)", e.fake.count(recipeArmIncumbent), e.fake.count(recipeArmCandidate))
	}
	perTask := map[string]int{}
	for _, c := range e.fake.calls {
		perTask[c]++
	}
	for _, id := range raTaskIDs() {
		for _, arm := range []string{recipeArmIncumbent, recipeArmCandidate} {
			if n := perTask[arm+":"+id]; n != 2 {
				t.Errorf("%s ran %s %d times, want 2", arm, id, n)
			}
		}
	}
}

// Which arm goes first alternates by task and by attempt, so neither arm always has the seat warm.
func TestRecipeAuditAlternatesWhichArmGoesFirst(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.report(t, "--attempts", "2")
	want := []string{
		"incumbent:task-01", "candidate:task-01", "candidate:task-01", "incumbent:task-01", // task 1: attempt 0, then attempt 1
		"candidate:task-02", "incumbent:task-02", "incumbent:task-02", "candidate:task-02", // task 2 starts with the other arm
	}
	if len(e.fake.calls) < len(want) {
		t.Fatalf("only %d seat calls: %v", len(e.fake.calls), e.fake.calls)
	}
	if got := e.fake.calls[:len(want)]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the first calls ran in the order %v, want %v", got, want)
	}
}

// Only a tokens metric can be read off a run, from the forwarded prefill tokens; the rest stay
// unresolved, and an unmeasured run is never a zero.
func TestRecipeAuditMetricsAreTokensOrUnresolved(t *testing.T) {
	effs := []eval.EfficiencyMetric{
		{Name: "forwarded_tokens", Class: eval.EffTokens, LowerIsBetter: true},
		{Name: "spend", Class: eval.EffMoney, LowerIsBetter: true},
	}
	got := recipeMetrics(effs, core.AgentWireResult{PrefillTokens: 1200})
	if v := got["forwarded_tokens"]; v == nil || *v != 1200 {
		t.Errorf("forwarded_tokens = %v, want 1200", v)
	}
	if v, ok := got["spend"]; !ok || v != nil {
		t.Errorf("spend = %v (present %v), want an unresolved null", v, ok)
	}
	if v := recipeMetrics(effs, core.AgentWireResult{})["forwarded_tokens"]; v != nil {
		t.Errorf("a run with no prefill accounting read as %v tokens, want unresolved", *v)
	}
}

// The two arms differ by the env-rule table alone: the incumbent runs the box's own, the candidate the
// recipe's in its place, and the box's config is not changed by building either.
func TestRecipeAuditArmsDifferByTheEnvRuleTableAlone(t *testing.T) {
	box := &core.AgentEnvRules{MaxObservationTokens: 100}
	base := config.Default()
	base.AgentEnvRules = box
	recipeRules := core.AgentEnvRules{DenyTools: []string{"web_fetch"}}

	inc := recipeArmConfig(base, recipeArmIncumbent, recipeRules)
	if inc.AgentEnvRules != box {
		t.Fatalf("the incumbent arm runs %+v, want the box's own table", inc.AgentEnvRules)
	}
	cand := recipeArmConfig(base, recipeArmCandidate, recipeRules)
	if cand.AgentEnvRules == nil || !reflect.DeepEqual(*cand.AgentEnvRules, recipeRules) {
		t.Fatalf("the candidate arm runs %+v, want the recipe's rule set", cand.AgentEnvRules)
	}
	if base.AgentEnvRules != box || box.MaxObservationTokens != 100 {
		t.Fatal("building the arms changed the box's config")
	}
	want := cand
	want.AgentEnvRules = box
	if !reflect.DeepEqual(want, inc) {
		t.Fatal("the two arms differ by more than agent_env_rules")
	}
}
