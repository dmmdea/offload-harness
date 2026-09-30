package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/validator"
)

// The rescue of a finished answer (register C-66, RC-6, PR-4).
//
// A node whose agent loop FINISHED but whose structured re-pack failed — stalled,
// unreachable, cut, or a shape the schema refused — defers, and a deferred result
// is never offered to acceptance, so the finished answer used to reach the caller
// as prose inside an error envelope and be counted as lost work. Every one of
// the 80 re-packs the fleet's nodes killed on 2026-09-29 carried a finished
// answer. The delegator holds the answer, so it re-packs it itself: the lossless
// reading first, then ONE completion on its own seat — never a registered run, so
// it takes no slot of the run cap — and the ordinary delegator-side acceptance
// then decides whether the result is a success. A rescue that cannot produce a
// validated object leaves the defer exactly as the node sent it.

// RescueFunc structures a finished answer for a contract. It is a seam, like
// LocalRunner, because the completion runs on the delegator's own pipeline and
// this package cannot import it. budget is the most it may wait (never below
// rescueFloor). It returns the object, the seat that produced it and how; an
// error means it could not, and the defer stays.
type RescueFunc func(ctx context.Context, contract core.AgentContract, output string, budget time.Duration) (Rescued, error)

// Rescued is what a RescueFunc hands back.
type Rescued struct {
	// Structured is the answer as an object of the contract's schema. The
	// delegator validates it again before it delivers anything.
	Structured json.RawMessage
	// Seat is the delegator-side seat that produced it ("" when no completion ran).
	Seat string
	// TokensOut is the completion's own generation, added to the result's total.
	TokensOut int
	// How says what produced the object, for the note.
	How string
}

// rescueFloor is the least a rescue may be given to wait: one re-pack of a long
// answer on a slow seat is minutes, and the contract's own budget may already be
// spent by the loop that finished.
const rescueFloor = 120 * time.Second

// SchemaMissRescuable reports whether a wire result is a finished answer whose
// structuring failed, and so worth re-packing on the delegator: a defer that
// carries the loop's complete answer and no structured object. A node that
// publishes SchemaMiss says so; one that predates the flag (0.140.x) is read from
// what it always sent: the loop ended `done`, and the defer's reason is a
// re-pack failure. A cut answer (output_truncated) is never rescuable — a
// partial cannot be re-packed into the whole object — and neither is a defer
// whose reason says the CALLER's context ended.
func SchemaMissRescuable(w core.AgentWireResult) bool {
	if !w.Deferred || len(w.Structured) != 0 || strings.TrimSpace(w.Output) == "" || w.OutputTruncated {
		return false
	}
	// The caller's context ended (the delegator abandoned the poll, the node is
	// shutting down): nobody is waiting for the object. Read before the node's own
	// flag, because the node sets SchemaMiss on every failure arm of its re-pack,
	// this one included, and an older node's reason carries the same prefix.
	if strings.HasPrefix(w.Reason, core.RepackCanceledReason) {
		return false
	}
	if w.SchemaMiss {
		return true
	}
	return w.StopReason == "done" &&
		(strings.HasPrefix(w.Reason, core.RepackFailedReason) || strings.HasPrefix(w.Reason, core.SchemaFailedReason))
}

// rescueSchemaMiss turns a deferred schema miss into a delivered result when the
// delegator can structure the finished answer, and runs acceptance over it like
// any other result. It runs inside finish, BEFORE the row is recorded, so the
// ledger and the corpus say what the caller received. Anything else — no rescue
// wired, no schema, an error, an object that does not validate — returns the
// result untouched apart from a note that a rescue was tried.
func (r *runner) rescueSchemaMiss(ctx context.Context, contract core.AgentContract, pr PlacedResult, start time.Time) PlacedResult {
	return rescueSchemaMiss(ctx, r.rescue, contract, pr, start, r.strikeOnFingerprint)
}

// rescueSchemaMiss is the rescue without a runner, for the route that has none
// (the queue): strike, when set, is told the base of a node whose rescued object
// fails the document fingerprint.
func rescueSchemaMiss(ctx context.Context, rescue RescueFunc, contract core.AgentContract, pr PlacedResult, start time.Time, strike func(base string, failures []string)) PlacedResult {
	if rescue == nil || pr.Err != "" || len(contract.OutputSchema) == 0 || !SchemaMissRescuable(pr.Result) {
		return pr
	}
	budget := time.Duration(executionBudgetSec(contract))*time.Second + pollGrace - time.Since(start)
	if budget < rescueFloor {
		budget = rescueFloor
	}
	got, err := rescue(ctx, contract, pr.Result.Output, budget)
	if err == nil {
		err = validateRescued(contract, got.Structured)
	}
	if err != nil {
		log.Printf("delegate: the finished answer of job %s on %s could not be re-packed on the delegator (the defer stands): %v", jobLabel(pr), nodeLabel(pr), err)
		pr.Result.RepackNote = joinRepackNote(pr.Result.RepackNote, "rescue on the delegator failed: "+errText(err))
		return pr
	}
	w := pr.Result
	was := w.Reason
	w.Structured = got.Structured
	w.Deferred, w.DeferClass, w.Reason, w.SchemaMiss = false, "", "", false
	w.TokensOut += got.TokensOut
	seat := got.Seat
	if seat == "" {
		seat = "the delegator"
	}
	how := got.How
	if how == "" {
		how = "re-packed"
	}
	w.RepackNote = joinRepackNote(w.RepackNote, fmt.Sprintf("rescued on %s (%s) after the node's re-pack failed: %s", seat, how, clipReason(was)))
	pr.Result = w
	// The rescued result is judged like any other: only a validated object that
	// passes the contract's checks reads as a success (the prose alone never does).
	pr.AcceptanceFailures = EvalAcceptance(contract, w)
	if strike != nil && pr.ranBase != "" {
		strike(pr.ranBase, pr.AcceptanceFailures)
	}
	return pr
}

// validateRescued holds a rescue to the schema the contract's answer is held to
// (its own, with every field the acceptance reads required), whatever produced
// the object: the delegator delivers nothing it has not validated itself.
func validateRescued(contract core.AgentContract, structured json.RawMessage) error {
	if len(structured) == 0 {
		return fmt.Errorf("the rescue returned no structured object")
	}
	// An OBJECT: a schema that only lists `properties` validates a bare string or
	// number, and every field check reads an object.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(structured, &obj); err != nil || obj == nil {
		return fmt.Errorf("the rescue returned %.40q, not a JSON object", string(structured))
	}
	var schema map[string]any
	if err := json.Unmarshal(core.RequireAcceptanceFields(contract.OutputSchema, contract.Acceptance), &schema); err != nil {
		return fmt.Errorf("output_schema is not a JSON object: %w", err)
	}
	if err := validator.Validate(structured, schema); err != nil {
		return fmt.Errorf("the rescued object does not validate: %w", err)
	}
	return nil
}

// joinRepackNote appends a finding to the result's repack_note.
func joinRepackNote(existing, add string) string {
	if strings.TrimSpace(existing) == "" {
		return add
	}
	return existing + "; " + add
}

// clipReason keeps the node's original reason readable in a note.
func clipReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
