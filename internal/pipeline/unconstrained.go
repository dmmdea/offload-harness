package pipeline

import (
	"encoding/json"

	"github.com/dmmdea/offload-harness/internal/parser"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/validator"
)

// Unconstrained seats (config unconstrained_seats).
//
// The RKLLM runtime on an RK3588 NPU cannot constrain decoding: it answers HTTP 400
// constrained_decoding_unsupported to any grammar, json_schema or response_format, and ignores
// logprobs. Every structured task here (classify, extract; summarize and triage too) always sends
// one of those, so on such a seat each call used to fail as an infra error and defer.
//
// For a seat the box declares in unconstrained_seats, attempt and attemptReasoningOn therefore:
//   - send NO grammar, NO json_schema and NO logprobs request (the vLLM structured_outputs branch
//     is skipped as well: the seat is not vLLM);
//   - state the exact JSON shape in the system prompt (tasks.Built.ForUnconstrained), leaving the
//     user prompt, injected exemplars and the packed input untouched;
//   - parse the reply with parser.ExtractOne (code fences and leading prose are stripped, but a
//     top-level array, a second object and a repeated key are refused) and accept it only after strict validation (validateReply) against the schema
//     the grammar would have enforced: every key present, types right, a classify label inside the
//     allowed set, no extra key. A failure takes the existing correction-retry path (Retries
//     counts it) and then defers with the validator's own words naming what failed.
//
// Grounding still applies to extract, and classify's self-reported confidence gate is unchanged.
// The decision-margin gate needs logprobs, so it is inert on these seats (no margin is recorded):
// the strict validation is the only structural guard, and that is why only classify and extract
// are admitted on the fleet text lane (ADR 0069). Seats that take a grammar are untouched: the
// branch is taken only for a declared seat, and Build's output for every other seat is unchanged.

// unconstrainedFor reports whether this attempt must take the prompt-carried-shape path: the
// model is a declared unconstrained seat AND the task carries a structured shape (a free-text
// task sends no constraint anyway, so it is left as it is).
func (p *Pipeline) unconstrainedFor(model string, built tasks.Built) bool {
	return len(built.Fields) > 0 && p.cfg.DeclaresUnconstrainedSeat(model)
}

// extractReply reads the JSON object out of one model reply. A seat that takes a grammar keeps
// parser.Extract exactly as it was; an unconstrained seat gets parser.ExtractOne, which accepts only
// a reply holding exactly one object (no top-level array, no second value, no repeated key), because
// no decoder guaranteed it. A refusal is a parse failure, so it takes the ordinary correction retry.
func extractReply(content string, unconstrained bool) (json.RawMessage, error) {
	if unconstrained {
		return parser.ExtractOne(content)
	}
	return parser.Extract(content)
}

// validateReply is the schema gate for one model reply: the task's own schema (extract's
// caller-supplied one) and, for an unconstrained seat, the strict schema no grammar enforced.
func validateReply(data []byte, built tasks.Built) error {
	if err := validator.Validate(data, built.Schema); err != nil {
		return err
	}
	if len(built.Strict) > 0 {
		return validator.Validate(data, built.Strict)
	}
	return nil
}

// promptFingerprintSent is the prompt-prefix fingerprint of the prompt an unconstrained seat is
// actually sent. Run stamps the row from the grammar-bearing build before any rung is chosen; the
// shape instruction changes the system prompt, so a row from an unconstrained seat is restamped here
// and the ledger never reports the fingerprint of a prompt that was not sent.
func promptFingerprintSent(built tasks.Built, input string) string {
	return promptPrefixFingerprint(built.System, userPreambleOf(built.User, input))
}
