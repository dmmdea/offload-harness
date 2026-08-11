package agent

import (
	"strings"
	"testing"
)

// A judge prompt that lists only trouble patterns saturates toward "this was
// warranted" — the exact mirror of the saturate-to-"low" failure measured in
// the model's own security_risk annotation on 2026-08-11 (54/54 "low", 0%
// park-gate recall). The fix is a three-way partition with the NEGATIVE space
// spelled out, so the judge has somewhere to put ordinary work.
//
// These tests pin that structure. They are deliberately about the prompt's
// SHAPE, not its wording: a future edit may reword freely, but silently
// deleting the expected-friction category would restore the saturation bug with
// no other signal.

func TestJudgePromptCarriesAllThreeVerdicts(t *testing.T) {
	for _, verdict := range []string{"WARRANTED", "EXPECTED FRICTION", "BLOCKER"} {
		if !strings.Contains(judgeSystemPrompt, verdict) {
			t.Errorf("judge prompt lost the %q verdict — without a place to put "+
				"ordinary work, the judge saturates toward warranted", verdict)
		}
	}
}

func TestJudgePromptEnumeratesExpectedFriction(t *testing.T) {
	// The negative space must be CONCRETE. "Use judgement" does not stop
	// saturation; named patterns do.
	required := []string{
		"corrected on a later turn",
		"expose a defect",
		"dead-end",
		"adaptation, not looping",
		"did not exist yet",
		"proceeded without it",
		"CLEAN outcome",
	}
	for _, pat := range required {
		if !strings.Contains(judgeSystemPrompt, pat) {
			t.Errorf("expected-friction pattern %q missing from the judge prompt", pat)
		}
	}
}

func TestJudgePromptAllowsAnAllClearVerdict(t *testing.T) {
	// If the judge has no sanctioned way to say "nothing here", it will invent
	// something. This is the single most important line for a quiet run.
	if !strings.Contains(judgeSystemPrompt, "manufacturing concern") {
		t.Error("judge prompt does not sanction an all-clear report")
	}
}

func TestJudgePromptDoesNotAskWhetherItWasFlagged(t *testing.T) {
	// Every record the judge sees is already flagged, so asking "was the flag
	// warranted" as the framing question is uninformative and biases toward yes.
	if strings.Contains(judgeSystemPrompt, "was the flag warranted") {
		t.Error("judge prompt still frames the task as confirming the flag")
	}
}

// The self-assessment clause in flaggedForJudge is measured-inert: it selects
// committed calls only when the model self-flags high, and the model never
// does. It must still fail SAFE if one ever appears.
func TestFlaggedForJudgeStillCatchesASelfFlaggedCommittedCall(t *testing.T) {
	got := flaggedForJudge([]EffectRecord{
		{Tool: "write_file", Status: EffectCommitted, Risk: "low"},
		{Tool: "delete_file", Status: EffectCommitted, Risk: "high"},
	})
	if len(got) != 1 || got[0].Tool != "delete_file" {
		t.Fatalf("expected only the self-flagged high call, got %+v", got)
	}
}

func TestFlaggedForJudgeIgnoresOrdinaryCommittedCalls(t *testing.T) {
	// The measured reality: every committed call carries "low", so none of them
	// should reach the judge on the strength of the annotation alone.
	got := flaggedForJudge([]EffectRecord{
		{Tool: "write_file", Status: EffectCommitted, Risk: "low"},
		{Tool: "delete_file", Status: EffectCommitted, Risk: "low"},
	})
	if len(got) != 0 {
		t.Fatalf("committed low-risk calls reached the judge: %+v", got)
	}
}
