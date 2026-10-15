package main

import (
	"errors"
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/recipe"
)

// The era of an audit is the seat it ran on. The harness pins the served seat (weights, quant, engine
// build, window, template) and puts the hash on every served run's wire result, so the verb takes the
// seat half of both arms' stamps from the runs and not from the alias it was asked to use: a model
// swapped behind the alias moves the stamps and the gate's era marker, and one swapped mid-audit ends
// the audit. The two arms differ by the env-rule table on top of that.

// raKindSeatPin is the refusal kind for a seat that cannot be pinned or whose pin moved.
const raKindSeatPin = "seat_pin"

// raAuditUnderPin runs a whole audit on a seat whose runs all carry pin, and returns the report and
// the gate input the verb wrote.
func raAuditUnderPin(t *testing.T, pin string) (recipeAuditReport, eval.GateInput) {
	t.Helper()
	e := newRAEnv(t)
	e.use(t, nil)
	base := raScript(false, true, 5, 3)
	e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
		w, err := base(arm, c)
		w.SeatConfigSHA256 = pin
		return w, err
	}
	rep := e.report(t)
	raw, err := os.ReadFile(e.inputOut)
	if err != nil {
		t.Fatal(err)
	}
	var in eval.GateInput
	if err := decodeJSONStrict(raw, &in); err != nil {
		t.Fatal(err)
	}
	if in.Incumbent == nil {
		t.Fatal("the gate input has no incumbent arm")
	}
	return rep, in
}

// The two arms ran under different env-rule tables, and their stamps say so.
func TestRecipeAuditArmsCarryDifferentStamps(t *testing.T) {
	rep, in := raAuditUnderPin(t, "pin-a")
	inc, cand := in.Incumbent.Stamps, in.Candidate.Stamps
	if inc.SeatConfigSHA256 == "" || cand.SeatConfigSHA256 == "" {
		t.Fatalf("an arm has no seat stamp: %+v and %+v", inc, cand)
	}
	if inc.SeatConfigSHA256 == cand.SeatConfigSHA256 {
		t.Fatalf("both arms carry the seat stamp %s: the stamps must differ by the env-rule table each ran under", inc.SeatConfigSHA256)
	}
	if rep.Gate.Stamps.Incumbent != inc || rep.Gate.Stamps.Candidate != cand {
		t.Fatalf("the gate result stamps %+v differ from the input's %+v and %+v", rep.Gate.Stamps, inc, cand)
	}
}

// The seat's pin is in both arms' stamps and so in the era: another pin is another era, the same pin
// is the same one.
func TestRecipeAuditPinMovesTheStampsAndTheEra(t *testing.T) {
	repA, inA := raAuditUnderPin(t, "pin-a")
	repA2, _ := raAuditUnderPin(t, "pin-a")
	repB, inB := raAuditUnderPin(t, "pin-b")
	if repA.Gate.EraMarker != repA2.Gate.EraMarker {
		t.Fatalf("the same seat gave two eras: %s and %s", repA.Gate.EraMarker, repA2.Gate.EraMarker)
	}
	if repA.Gate.EraMarker == repB.Gate.EraMarker {
		t.Fatalf("a different seat pin gave the same era marker %s: a model swapped behind the alias would not move the era", repA.Gate.EraMarker)
	}
	if inA.Incumbent.Stamps.SeatConfigSHA256 == inB.Incumbent.Stamps.SeatConfigSHA256 {
		t.Error("the incumbent's seat stamp ignores the seat's pin")
	}
	if inA.Candidate.Stamps.SeatConfigSHA256 == inB.Candidate.Stamps.SeatConfigSHA256 {
		t.Error("the candidate's seat stamp ignores the seat's pin")
	}
}

// wantSeatPinRefusal asserts a seat_pin refusal that ended the audit after calls seat calls and
// recorded nothing.
func (e *raEnv) wantSeatPinRefusal(t *testing.T, err error, calls int) {
	t.Helper()
	var ref *recipeRefusal
	if !errors.As(err, &ref) || ref.Kind != raKindSeatPin {
		t.Fatalf("want a %s refusal, got %v", raKindSeatPin, err)
	}
	if code, _ := exitStatus(err); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if n := len(e.fake.calls); n != calls {
		t.Fatalf("the seat was asked %d times, want %d", n, calls)
	}
	if got := e.saved(t); got.Status != recipe.StatusProposed || got.Audit != nil {
		t.Fatalf("a refused audit changed the recipe: %+v", got)
	}
}

// A seat that changes behind its alias mid-audit ends the audit at the first run that shows it, and
// records nothing: the two arms were not measured on one seat.
func TestRecipeAuditRefusesASeatThatChangesMidAudit(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	base := raScript(false, true, 5, 3)
	n := 0
	e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
		n++
		w, err := base(arm, c)
		if n > 20 {
			w.SeatConfigSHA256 = "pin-b"
		}
		return w, err
	}
	_, _, err := e.run(e.args())
	e.wantSeatPinRefusal(t, err, 21)
}

// A run that served and carries no pin cannot be shown to be on the same seat as the others: the
// harness's own rule is that an unpinned row does not enter a paired experiment. The harness stamps the
// pin after the loop and before every terminal branch, so a run that finished and a run that spent its
// budget or abstained are both served runs and both carry one; only a run that ended before that point
// (a ceiling, a canceled parent, an infrastructure defer) legitimately carries none.
func TestRecipeAuditRefusesAServedRunWithNoPin(t *testing.T) {
	cases := map[string]func(w core.AgentWireResult) core.AgentWireResult{
		"a finished run": func(w core.AgentWireResult) core.AgentWireResult {
			w.SeatConfigSHA256 = ""
			return w
		},
		"a spent step budget": func(w core.AgentWireResult) core.AgentWireResult {
			return core.AgentWireResult{Steps: 12, StopReason: "budget", Deferred: true, DeferClass: core.DeferClassBudget, Reason: "step budget exhausted (12 steps)"}
		},
		"an abstention": func(w core.AgentWireResult) core.AgentWireResult {
			return core.AgentWireResult{Steps: 3, StopReason: "final", Deferred: true, DeferClass: core.DeferClassAbstention, Reason: "output failed schema"}
		},
	}
	for name, unpin := range cases {
		t.Run(name, func(t *testing.T) {
			e := newRAEnv(t)
			e.use(t, nil)
			base := raScript(false, true, 5, 3)
			n := 0
			e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
				n++
				w, err := base(arm, c)
				if n == 7 {
					w = unpin(w)
				}
				return w, err
			}
			_, _, err := e.run(e.args())
			e.wantSeatPinRefusal(t, err, 7)
		})
	}
}

// A run that ended before the seat finished serving it carries no pin by design (the probe would
// cold-start a seat that is not resident), and that is not a reason to end the audit: it is not a
// verdict about the task either, so the gate counts it and the audit goes on.
func TestRecipeAuditToleratesRunsThatEndedBeforeTheSeatServedThem(t *testing.T) {
	for name, w := range map[string]core.AgentWireResult{
		"an infrastructure defer": {Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "endpoint down"},
		"a wall ceiling":          {Deferred: true, DeferClass: core.DeferClassBudget, Reason: "wall timeout after 600s (the caller's deadline, not this node's ceiling)"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRAEnv(t)
			e.use(t, nil)
			base := raScript(false, true, 5, 3)
			n := 0
			e.fake.script = func(arm string, c core.AgentContract) (core.AgentWireResult, error) {
				n++
				if n == 7 {
					return w, nil
				}
				return base(arm, c)
			}
			rep := e.report(t)
			if rep.Gate.Stamps.Incumbent.SeatConfigSHA256 == "" {
				t.Fatalf("the audit ended without a seat stamp: %+v", rep.Gate.Stamps)
			}
		})
	}
}

// An audit in which no run was served has no seat to name in the era: nothing was measured, and the
// recipe is not demoted for it.
func TestRecipeAuditRefusesWhenNoRunWasServed(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.fake.script = func(string, core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "endpoint down"}, nil
	}
	_, _, err := e.run(e.args())
	e.wantSeatPinRefusal(t, err, 32)
}
