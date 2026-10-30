// Helpers of the delegate and research verbs: contract-file parsing, per-subtask
// intake, engine errors, placement pins and the exit status.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// repeatedFlag collects a repeatable string flag (--remote a --remote b).
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// cliIntake prepares a CLI verb's subtasks one by one (register D-111), the verbs' half of what the MCP doors do: a
// subtask that fails intake no longer rejects the valid ones beside it. The returned batch holds the contracts that passed
// (tagged with the verb's door, in input order, each with its input position) and the rejections, and the verb splices
// those back among the engine's results (PreparedBatch.Splice) so the answer holds one result per subtask sent. The lints
// are kept by INPUT position, as the wire builder reads them. The error is only for a call with nothing valid in it, and it
// names every bad subtask: label and ids word it as the MCP doors do ("subtask 2: ...", or "source 4: ..." for a research
// page, named by its place in the url list; nil ids = the input position).
func cliIntake(specs []delegate.SubtaskSpec, readRoot string, capBytes int, door, label string, ids []int) (delegate.PreparedBatch, [][]string, error) {
	batch := delegate.PrepareBatchWithCap(specs, readRoot, capBytes)
	if batch.AllRejected() {
		return batch, nil, fmt.Errorf("%s", batch.FlatReason(label, ids))
	}
	lints := make([][]string, len(specs))
	for k := range batch.Contracts {
		batch.Contracts[k].Door = door
		// Linted on the PREPARED contract (context_paths already inlined: grounding is judged against everything the
		// sub-agent will see).
		lints[batch.Indices[k]] = delegate.LintAcceptance(batch.Contracts[k])
	}
	return batch, lints, nil
}

// cliEngineErr is the error a verb returns when the engine refuses the whole call, so that nothing ran and there is no result
// to splice a refusal into: the engine's own error (kept wrapped, so errors.Is still sees it), and the subtasks intake refused
// beside it, so fixing the first complaint does not meet the second on the retry (the MCP doors word the same sentence).
func cliEngineErr(err error, batch delegate.PreparedBatch, label string, ids []int) error {
	if len(batch.Rejections) == 0 {
		return err
	}
	return fmt.Errorf("%w; also refused at intake: %s", err, batch.FlatReason(label, ids))
}

// cliPinOptions builds the engine's options for the delegate and research verbs, the CLI doors that offer pin_reason (ADR
// 0078): a --route local or remote without --pin-reason is a hint that placement may override, exactly as it is on the MCP
// tools; with one it is the pin it always was, and the reason is recorded. One builder, so the rescue and the door's half
// of the pin contract are wired in one place (TestEveryDelegatorSurfaceWiresTheRescue reads it one hop away).
func cliPinOptions(priority int, tenant string, rescue delegate.RescueFunc, pinReason string) *delegate.RunOptions {
	return &delegate.RunOptions{Priority: priority, Tenant: tenant, Rescue: rescue, PinReason: pinReason, PinNeedsReason: true}
}

// delegateExitErr maps a run summary onto the verb's exit code. Two things
// exit non-zero, and both mean "a human has to look":
//
//   - Failed: transport/config per-subtask errors (auth rejected, dispatch
//     refused, broken wire, a node that never answered).
//   - Infrastructure: defers whose class blames the STACK or the config rather
//     than the work. A node with a dead llama-swap defers every subtask, and
//     without this it exits 0 — a broken fleet reading as a good run is exactly
//     the silent failure this contract exists to prevent.
//
// Everything else — honest abstentions, budget defers, CONTRACT-classed defers
// (a contract no healthy node would accept: no output_schema, past the origin
// hop, too big for any advertised ceiling), and failed verification — stays
// exit 0: those are RESULT shapes, reported in the JSON above, matching every
// other verb's defer posture.
//
// BOTH counts are reported when both are non-zero. Returning on the first
// non-zero class named only the failures, so an operator who fixed the
// transport error learned about the broken node only on the next run.
//
// THE TWO SURFACES DIVERGE ON PURPOSE (C-75, ADR 0065). The MCP door's error
// flag (mcpserver.delegateIsError) once matched this rule on the case that
// matters — a subtask LOST to the stack (Summary's LostToStack: its contracted
// output never arrived and a box is why, which includes a finished loop whose
// structured re-pack seat was unreachable) — so one of two subtasks eaten by a
// dead llama-server could not read as a clean call on either surface (R5-2).
// C-75 narrowed the flag: IsError says THE CALL FAILED, the MCP client answers an
// error-flagged body by keeping only its head and tail, and a partial call that
// delivered seven digests did not fail. It is now set only when nothing succeeded
// and something failed, was lost or was skipped; a partial result is a successful
// call whose body says what is missing (summary.lost_to_stack, each result's
// defer_class and reason). This verb keeps the wider rule: an exit code sits
// BESIDE the printed results, so it cannot hide a digest, and a script has no
// other loud signal. A subtask lost to the stack, and a fleet-down LOCAL SUCCESS
// (Infrastructure with no LostToStack — every subtask delivered, but the fleet
// failed its probe), exit non-zero here whether or not a sibling succeeded.
func delegateExitErr(sum delegate.Summary) error {
	var parts []string
	if sum.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d subtask(s) failed (transport/config) — see results[].reason", sum.Failed))
	}
	if sum.Infrastructure > 0 {
		parts = append(parts, fmt.Sprintf("%d subtask(s) hit infrastructure/config rather than the work (a defer blaming the stack, or a local fallback taken because the fleet failed its health probe) — see results[].defer_class, reason, and placement", sum.Infrastructure))
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}

// parseContractFile reads a delegation contract file: either ONE subtask
// object or an array of them. Tolerant decode (the wire's reader posture) —
// unknown fields are ignored, so a contract file written for a newer harness
// still parses here.
func parseContractFile(path string) ([]delegate.SubtaskSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading contract file: %w", err)
	}
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "[") {
		var specs []delegate.SubtaskSpec
		if err := json.Unmarshal([]byte(trimmed), &specs); err != nil {
			return nil, fmt.Errorf("contract file %s: %w", path, err)
		}
		return specs, nil
	}
	var spec delegate.SubtaskSpec
	if err := json.Unmarshal([]byte(trimmed), &spec); err != nil {
		return nil, fmt.Errorf("contract file %s: %w", path, err)
	}
	return []delegate.SubtaskSpec{spec}, nil
}
