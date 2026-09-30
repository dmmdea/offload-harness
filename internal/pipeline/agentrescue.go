package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

// RescueRepack is the delegator's rescue of a finished answer whose structured
// re-pack failed on the executing node (register C-66, RC-6, PR-4); it satisfies
// delegate.RescueFunc. The delegator holds the answer (contract, node result),
// so it structures it itself: first the lossless reading (the answer may
// already BE the object, or need only the scalar coercion the re-pack lanes
// apply), then ONE completion on this box's agent seat.
//
// It is one seat request, not a run: it registers nothing with the run
// registry and takes no slot of the seat cap. It still goes through the client's
// admission (modelaffinity.Admit), so it queues behind work in flight on the
// seat instead of evicting it — a busy card is a place in line. A seat that is
// not resident is warmed first, on the box's admission budget and outside the
// allowance, as a run's own admission does: the delegator's seat is idle-unloaded
// after five minutes, so the rescue routinely finds it cold. The completion
// is held to the same schema a node's answer is (the contract's own, with every
// field its acceptance reads required) and bounded by the re-pack allowance
// sized from the answer and THIS seat's rate (a floor of the flat re-pack
// bound), never past budget. An error means it could not produce a validated
// object; the caller leaves the node's defer as it was. Nothing here judges the
// object beyond the schema — acceptance still runs over what it returns.
func (p *Pipeline) RescueRepack(ctx context.Context, contract core.AgentContract, output string, budget time.Duration) (delegate.Rescued, error) {
	seat := strings.TrimSpace(p.cfg.AgentPlannerModel(""))
	if seat == "" || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return delegate.Rescued{}, errors.New("this box has no agent seat to re-pack on")
	}
	if len(contract.OutputSchema) == 0 {
		return delegate.Rescued{}, errors.New("the contract carries no output_schema")
	}
	schema := core.RequireAcceptanceFields(contract.OutputSchema, contract.Acceptance)
	if direct, ok := directStructured(output, schema); ok {
		return delegate.Rescued{Structured: direct, How: "the finished answer was already the object"}, nil
	}
	// A seat that is not resident loads on the first request that reaches it, and a
	// cold load is minutes on a vLLM seat (125–250 s), far more than the re-pack's
	// allowance: the completion was cut by its own load and left a loaded seat
	// nobody used. Warm it first (D-64): a resident seat costs one /running read,
	// and agent_admission_wait_sec -1 turns the gate off.
	how := "one re-pack completion"
	if _, warmNote, attempted := warmSeat(ctx, p.cfg.Endpoint, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec)); warmNote != "" {
		log.Printf("rescue re-pack: seat warm-up (%s): %s", seat, warmNote)
		if attempted {
			how += "; " + warmNote
		}
	}
	allow := agent.StallPolicy{
		Repack: livenessRepack, Floor: livenessFloor, Slack: livenessSlack, TokS: p.rescueTokS(seat),
	}.Allowance(agent.PhaseRepack, expectedRepackTokens(output))
	if budget > 0 && allow > budget {
		allow = budget
	}
	rctx, cancel := context.WithTimeout(ctx, allow)
	defer cancel()
	// A seat that answers "busy" — llama-swap's 429 when peers hold its slots, a
	// 503 while its process is not ready — is a place in line, not a refusal: with
	// no budget on the context the first busy answer stands. The rescue waits on
	// the standard busy-seat budget, inside its own allowance, like every other
	// seat call of a contract.
	rctx = seatwait.WithBudget(rctx, seatwait.NewBudget(p.cfg.SeatContentionWaitSec))
	structured, tokens, _, _, err := p.repackStructuredWith(rctx, seat, schema, output, 0, 1)
	if err != nil {
		return delegate.Rescued{}, fmt.Errorf("one re-pack completion on %s: %w", seat, err)
	}
	return delegate.Rescued{Structured: structured, Seat: seat, TokensOut: tokens, How: how}, nil
}

// rescueTokS is the decode rate this box remembers for its agent seat (else the
// configured one), read without touching the pipeline's own store handle: the
// rescue runs on whichever goroutine the delegation fan-out gives it.
func (p *Pipeline) rescueTokS(seat string) float64 {
	root, err := gpulease.ResolveStateRoot(p.cfg.StateDir)
	if err != nil {
		log.Printf("rescue re-pack: seat-rates store unavailable, the configured rate sizes the allowance: %v", err)
		return p.cfg.AgentSeatTokS
	}
	store, lerr := seatrate.Load(seatrate.Path(root))
	if lerr != nil {
		// A corrupt store reads as empty (seatrate.Load): say so, as seatRates does.
		log.Printf("rescue re-pack: %v; the configured rate sizes the allowance", lerr)
	}
	if store != nil {
		if t := store.Get(seat).TokS; t > 0 {
			return t
		}
	}
	return p.cfg.AgentSeatTokS
}
