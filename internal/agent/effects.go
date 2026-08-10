package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Effect accounting for tool execution (ADR-pending; pattern adopted 2026-08-10
// from turnstone's effect-record design, trimmed to what this loop can honestly
// represent).
//
// The loop's tool dispatch has three failure shapes that used to collapse into
// one error string: the tool ran and reported failure, the tool was ABANDONED
// mid-flight (its goroutine may still be mutating the world — see dispatch's
// hard-cap design note), and the call was never executed at all (unknown tool,
// circuit-breaker refusal). A caller deciding whether a run is safe to retry —
// or a judge deciding whether an approval was honoured — needs that
// distinction, not a prose transcript. This is the same class of honesty the
// eval program kept re-learning at other layers (unconditional .done markers,
// silent row truncation): a state you cannot distinguish is a state you will
// eventually misreport.
//
// Deliberately ABSENT: turnstone's PARTIAL and ROLLED_BACK. This loop has no
// rollback machinery and no sub-call progress reporting, so neither status is
// representable — claiming them would be decoration. If a tool ever gains
// transactional semantics, extend the enum then.

// EffectStatus classifies what one tool call did to the world.
type EffectStatus string

const (
	// EffectCommitted — the tool ran to completion and returned success. Its
	// effects (if any) are whatever the tool's contract says a success does.
	EffectCommitted EffectStatus = "committed"
	// EffectFailed — the tool ran to completion and returned an error. Effects
	// up to the failure point may exist; the tool got the chance to clean up.
	EffectFailed EffectStatus = "failed"
	// EffectUnknown — the call was STARTED but the loop stopped waiting
	// (per-tool budget exceeded, or the run was cancelled mid-call). The
	// goroutine may still be running; effects are genuinely unknown. Unknown,
	// never none: this is the one status it is dangerous to soften.
	EffectUnknown EffectStatus = "unknown"
	// EffectNone — the call was never executed: unknown tool name, or a
	// circuit-breaker refusal (exact repeat, name cap, disabled tool). The
	// world is untouched by this call.
	EffectNone EffectStatus = "none"
)

// notPerformedError is the sentinel a tool's Exec returns when it DECLINED to
// act: a policy-broker denial, an allowlist/path-form refusal, or the sandbox
// cage failing to start. The tool function ran, but the requested action was
// never performed and the world is untouched — which is EffectNone, not
// EffectCommitted. Without this sentinel every defer-not-crash refusal (text +
// nil error, the repo-wide convention) classified as committed, and a run whose
// writes were ALL denied was byte-identical on the ledger to one whose writes
// all landed — inverting the feature (review finding #1, 2026-08-10).
//
// dispatch converts it back into ordinary (non-error) content for the model, so
// the model-visible bytes are identical to the pre-ledger behaviour.
type notPerformedError struct{ msg string }

func (e *notPerformedError) Error() string { return e.msg }

// NotPerformed wraps a refusal message for Exec to return as its error. The
// message is what the model will see as the (non-error) tool result.
func NotPerformed(msg string) error { return &notPerformedError{msg: msg} }

// IsNotPerformed reports whether err is a NotPerformed sentinel.
func IsNotPerformed(err error) bool {
	var t *notPerformedError
	return errors.As(err, &t)
}

// EffectRecord is one tool call's execution accounting, in call order.
type EffectRecord struct {
	Step   int          `json:"step"`
	CallID string       `json:"call_id,omitempty"`
	Tool   string       `json:"tool"`
	Status EffectStatus `json:"status"`
	// Note carries the WHY for non-committed statuses (budget exceeded,
	// refusal class, unknown tool) so the record is auditable without the
	// transcript. Empty for committed.
	Note string `json:"note,omitempty"`
	// Risk is the model's own security_risk annotation on the call ("" when
	// not provided) — recorded for every fate so the flywheel can correlate
	// self-assessed risk with actual outcomes.
	Risk string `json:"risk,omitempty"`
}

// EffectCounts aggregates a run's records per status — the summary an MCP
// caller actually reads ("did anything end up in unknown?").
func EffectCounts(recs []EffectRecord) map[EffectStatus]int {
	if len(recs) == 0 {
		return nil
	}
	c := make(map[EffectStatus]int, 4)
	for _, r := range recs {
		c[r.Status]++
	}
	return c
}

// securityRisk extracts the model's own security_risk annotation from a tool
// call's raw JSON args. Absent (or wholly unparsable JSON) yields "" — no
// signal; a wholly-malformed call fails loudly in the tool's own required-field
// checks. A PRESENT value normalizes case+whitespace, and a present-but-
// unrecognized value ("critical", "severe", a number) returns
// "unrecognized(<raw>)" rather than "": the target population is weak local
// models, which emit case variants and synonyms routinely, and a tighten-only
// mechanism must fail CLOSED — the model tried to flag the call, so the park
// logic treats anything unrecognized like high, and the ledger records the raw
// attempt instead of silently discarding it (review finding #1, 2026-08-10).
func securityRisk(args string) string {
	var probe struct {
		SecurityRisk any `json:"security_risk"`
	}
	if err := json.Unmarshal([]byte(args), &probe); err != nil || probe.SecurityRisk == nil {
		return ""
	}
	raw, ok := probe.SecurityRisk.(string)
	if !ok {
		return fmt.Sprintf("unrecognized(%v)", probe.SecurityRisk)
	}
	switch norm := strings.ToLower(strings.TrimSpace(raw)); norm {
	case "":
		return ""
	case "low", "medium", "high":
		return norm
	default:
		return "unrecognized(" + raw + ")"
	}
}

// riskParks reports whether a security_risk annotation parks an effectful call
// on an unattended run: an explicit high, or any present-but-unrecognized
// value (fail closed — see securityRisk).
func riskParks(risk string) bool {
	return risk == "high" || strings.HasPrefix(risk, "unrecognized(")
}
