// lint.go — warn-at-intake acceptance lint (delegator-side, both surfaces).
//
// Acceptance is the lane's verifiability gate: it decides failed_verification
// vs success AND whether the cross-seat retry fires. Three authoring
// pathologies, every one MEASURED in the standing corpus before this existed,
// let that gate pass garbage while reading as "verified":
//
//   - shape-only     — nonempty:/min_items: with no content check. Measured:
//     "The docs directory does not exist" passed nonempty:summary; a seat once
//     passed nonempty:q1 by echoing the questions back verbatim.
//   - parrot-passable — every content check is ALSO satisfied by the goal
//     text itself, so a model that repeats the question back passes the whole
//     contract (and the retry that would have fixed the answer never fires,
//     because nothing failed). Measured: 5/5 of the first organic contracts.
//   - ungrounded     — a contains:/regex: whose pattern appears nowhere in
//     the contract's own context docs cannot distinguish a wrong answer from
//     a defective contract. Measured: contains:OptiPlex failed BOTH seats on
//     a task both did right.
//
// WARN-ONLY by decision: the lint never blocks, never changes placement or
// evaluation. It rides the response per-subtask (acceptance_lint) so the
// CALLER — usually a model mid-session — sees it beside the results and can
// fix the acceptance and resubmit. Blocking was rejected because a linted
// contract still produces a usable (if weakly verified) result, and because
// grounding is heuristic: an answer-side synonym for doc content is legal.
//
// Kept in LOCKSTEP with the Python analyzer's A3 classifier
// (memory-frontier bench/seat-quality-pilot.py); change both or neither.
package delegate

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// LintAcceptance returns human-readable warnings for the measured acceptance
// pathologies. Empty slice = nothing to warn about. Unparseable checks are
// skipped here — Validate already rejects them upstream with a hard error,
// and double-reporting would drown the lint's signal.
func LintAcceptance(c core.AgentContract) []string {
	var docs strings.Builder
	for _, d := range c.Context {
		docs.WriteString(d.Text)
		docs.WriteByte('\n')
	}
	docsText := docs.String()

	var warns []string
	content := 0          // parsed content-anchored checks (contains/not_contains/regex)
	parrotPassable := 0   // of those, how many the goal text alone satisfies
	for _, raw := range c.Acceptance {
		chk, err := core.ParseAcceptanceCheck(raw)
		if err != nil {
			continue
		}
		switch chk.Kind {
		case core.AccContains:
			content++
			if !strings.Contains(docsText, chk.Arg) {
				warns = append(warns, fmt.Sprintf(
					"acceptance %q is UNGROUNDED: the substring appears nowhere in this contract's context docs, so it cannot distinguish a wrong answer from a defective contract (measured: contains:OptiPlex failed both seats on a task both did right) — anchor it to text that actually appears in the docs", raw))
			}
			if strings.Contains(c.Goal, chk.Arg) {
				parrotPassable++
			}
		case core.AccRegex:
			// Parse-time compilation, via the accessor — no recompile, no
			// can-never-fail error branch. The nil guard sits BEFORE content++
			// on purpose: a check that cannot be analyzed must not enter the
			// parrot denominator either, or it would silently suppress a
			// legitimate all-echoable warning (review finding, 0.88.0).
			re := chk.Pattern()
			if re == nil {
				continue
			}
			content++
			if !re.MatchString(docsText) {
				warns = append(warns, fmt.Sprintf(
					"acceptance %q is UNGROUNDED: the pattern matches nothing in this contract's context docs, so it cannot distinguish a wrong answer from a defective contract — anchor it to content that actually appears in the docs", raw))
			}
			if re.MatchString(c.Goal) {
				parrotPassable++
			}
		case core.AccNotContains:
			content++
			// A parrot's output IS the goal text, so not_contains:<s> passes
			// a parrot exactly when the goal does not contain <s>.
			if !strings.Contains(c.Goal, chk.Arg) {
				parrotPassable++
			}
		}
	}

	// The aggregate verdict is PREPENDED so it is genuinely the first line —
	// the caller reads warns[0] as the headline, and the per-check UNGROUNDED
	// lines (appended during the loop) are its supporting detail. Shape-only
	// subsumes parrot-passable (no content checks means trivially
	// parrot-passable — warn once, not twice).
	if content == 0 {
		warns = append([]string{"acceptance is SHAPE-ONLY (nonempty:/min_items: verify that fields exist, not that they are true): a wrong or evasive answer passes (measured: \"the docs directory does not exist\" passed nonempty:summary) — add at least one contains:/regex:/not_contains: check anchored to doc content"}, warns...)
	} else if parrotPassable == content {
		warns = append([]string{"acceptance is PARROT-PASSABLE: every content check is also satisfied by the goal text itself, so a model that echoes the question back passes as verified AND the cross-seat retry never fires (measured on 5/5 of the first organic contracts) — anchor at least one check to content that appears only in the docs/answer, not in the goal"}, warns...)
	}
	return warns
}
