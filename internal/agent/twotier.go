package agent

import (
	"context"
	"strings"
)

// FallbackReason records which path RunTwoTier actually took, so the caller can
// log it. FallbackNone means the two-tier plan-then-execute path ran; the others
// mean the architect produced nothing usable and RunTwoTier fell back to a
// single-model run of the ORIGINAL objective on the editor loop.
type FallbackReason string

const (
	// FallbackNone: the architect produced a usable plan and the editor executed it.
	FallbackNone FallbackReason = ""
	// FallbackDegeneratePlan: the architect returned an empty/degenerate plan
	// (nothing to hand off), so the editor ran the original objective directly.
	FallbackDegeneratePlan FallbackReason = "degenerate-plan"
	// FallbackArchitectError: the architect run errored, so the editor ran the
	// original objective directly.
	FallbackArchitectError FallbackReason = "architect-error"
)

// Fallback is set on the Result RunTwoTier returns to record which path ran.
// It is a distinct field (not part of the loop-level Result semantics) so the
// caller can decide how to surface it.
func (r *Result) withFallback(f FallbackReason) Result {
	r.Fallback = f
	return *r
}

// minPlanChars is the smallest architect reply we treat as a usable plan. A
// one-word refusal ("no", "cannot", "n/a") trimmed is shorter than this, so it
// is caught as degenerate and we fall back rather than hand the editor an
// unrecoverable underspecified plan (the editor has NO access to the original
// request, so an empty plan is fatal — aider's small-model pitfall guard).
const minPlanChars = 16

// ArchitectPrompt is the planning-model system prompt for two-tier mode. It has
// read/search tools only (NO write) and its whole job is to emit ONE complete,
// unambiguous plan a separate edit model will execute WITHOUT ever seeing the
// original request — so the plan must stand alone. Phrasing follows aider's
// architect prompt (provide direction to your editor engineer; make it
// unambiguous and complete; do NOT reproduce whole files).
const ArchitectPrompt = `You are the ARCHITECT. You investigate the workspace with your read and search tools, then hand a separate EDITOR engineer a single, complete plan to execute.

The editor will see ONLY your plan — it will NOT see this request, the conversation, or anything you looked at. So your plan MUST stand entirely on its own.

Your plan must:
- Name every concrete file to change (exact paths you verified with your tools).
- For each file, describe the change precisely and unambiguously — enough that the editor can apply it with no further context or questions.
- Be complete: cover every step needed to satisfy the request. Do not leave anything implied.
- DO NOT reproduce whole files. Describe the edits (what to add/change/remove and where); the editor writes the actual code.

First use your tools to read the relevant files so the plan references real, current code. Then output the plan as your final message — prose, well-structured, with the file changes clearly laid out. Output ONLY the plan.`

// EditorPrompt is the edit-model system prompt for two-tier mode. Its sole user
// message is the architect's plan (no history, not the original request), and it
// has the write tools the user's --allow-* flags granted.
const EditorPrompt = `You are the EDITOR. The message you receive is a complete implementation plan written for you by an architect. It is your ONLY instruction — treat it as the full task.

Execute the plan exactly using your tools: make the file edits it specifies. Do not second-guess the plan or ask for the original request — the plan is self-contained and is what you must implement. When every step is done, briefly state what you changed.`

// RunTwoTier runs aider's verified architect/editor one-shot handoff (the
// best-evidenced small-model decomposition: architect/editor 26.2% vs 20.9%
// solo, edit-format compliance 67.6%->100%).
//
// Flow (exactly aider's semantics):
//  1. The architect loop runs the ORIGINAL objective and produces a full prose
//     plan (Result.Output).
//  2. If the plan is non-degenerate, the editor loop runs with that plan as its
//     SOLE user message — Loop.Run always starts a fresh transcript (system +
//     optional recall/AGENT.md + the objective it is given), so passing the plan
//     as the editor's objective naturally gives the editor a fresh context with
//     NO conversation history and NOT the original request. The editor's Result
//     is returned.
//
// It is one-shot: the architect is NOT re-invoked after the editor runs. On a
// single GPU this is exactly ONE cold model swap (architect model -> editor
// model), cheap because it is plan-once-then-execute, not per-step alternation.
//
// Fallback (never leave the user with nothing): if the architect errored OR
// produced no usable plan, RunTwoTier runs the ORIGINAL objective directly on
// the editor loop as a single-model run and records the reason in
// Result.Fallback.
func RunTwoTier(ctx context.Context, objective string, architect, editor *Loop) (Result, error) {
	archRes, err := architect.Run(ctx, objective)
	if err != nil {
		// Architect unreachable/failed — fall back to a single-model editor run of
		// the ORIGINAL objective so the user still gets an attempt.
		res, eerr := editor.Run(ctx, objective)
		return res.withFallback(FallbackArchitectError), eerr
	}

	plan := strings.TrimSpace(archRes.Output)
	if len(plan) < minPlanChars {
		// Degenerate plan (empty or a one-word refusal). Handing this to the editor
		// is unrecoverable (it never sees the original request), so fall back to a
		// single-model editor run of the ORIGINAL objective.
		res, eerr := editor.Run(ctx, objective)
		return res.withFallback(FallbackDegeneratePlan), eerr
	}

	// One-shot handoff: the architect's full plan is the editor's sole user message.
	res, eerr := editor.Run(ctx, plan)
	return res.withFallback(FallbackNone), eerr
}
