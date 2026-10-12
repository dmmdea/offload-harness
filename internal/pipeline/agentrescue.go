package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/seatwait"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// rescueSecondTry is the least a rescue must have left after a first rung failed for a second to
// be worth starting: a re-pack is a prefill and a decode, and a try that cannot finish only
// spends the time the caller's re-placement is credited back.
const rescueSecondTry = 20 * time.Second

// RescueRepack is the delegator's rescue of a finished answer whose structured
// re-pack failed on the executing node (register C-66, RC-6, PR-4); it satisfies
// delegate.RescueFunc. The delegator holds the answer (contract, node result),
// so it structures it itself, down a ladder:
//
//  1. the lossless reading: the answer may already BE the object, or need only
//     the scalar coercion the re-pack lanes apply;
//  2. ONE completion on this box's agent seat, when that seat is resident (or its
//     residency cannot be read: "could not tell" never moves work off the box);
//  3. when this box's seat is NOT resident, ONE completion on the node that wrote
//     the answer, through that node's chat door (delegate.RescueOrigin, handed in
//     the context). That node has just decoded the answer and holds its seat hot, so
//     the re-pack waits for no load, and a request that leaves the box never queues
//     behind this box's GPU lease (register C-41c). The recorded rescue found this
//     box's seat cold under another session's lease, warmed it, and was cut at its
//     120 s allowance: a finished, correct answer lost for a seat that was never
//     the natural place to re-pack it;
//  4. this box's seat after all, warmed first, when the node could not take the call
//     (an older build with no chat door, a refused or hollow object) and time remains.
//
// Each completion is one seat request, not a run: it registers nothing with the run
// registry and takes no slot of the seat cap. On this box it still goes through the
// client's admission (modelaffinity.Admit), so it queues behind work in flight on the
// seat instead of evicting it — a busy card is a place in line. A seat that is not
// resident is warmed first, on the box's admission budget and outside the
// allowance, as a run's own admission does: the delegator's seat is idle-unloaded
// after five minutes, so the rescue routinely finds it cold. The completion
// is held to the same schema a node's answer is (the contract's own, with every
// field its acceptance reads required) and to the same reading against its answer
// (core.ListsEmptied), and bounded by the re-pack allowance sized from the answer
// and THIS seat's rate (a floor of the flat re-pack bound), never past budget. An
// error means it could not produce a validated object, and names every rung it
// tried; the caller leaves the node's defer as it was. Nothing here judges the
// object beyond the schema and its answer — acceptance still runs over what it
// returns.
func (p *Pipeline) RescueRepack(ctx context.Context, contract core.AgentContract, output string, budget time.Duration) (delegate.Rescued, error) {
	seat := strings.TrimSpace(p.cfg.AgentPlannerModel(""))
	if seat == "" || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return delegate.Rescued{}, errors.New("this box has no agent seat to re-pack on")
	}
	if len(contract.OutputSchema) == 0 {
		return delegate.Rescued{}, errors.New("the contract carries no output_schema")
	}
	schema := core.RequireAcceptanceFields(contract.OutputSchema, contract.Acceptance)
	if direct, coerced, ok := directStructuredNoted(output, schema); ok {
		how := "the finished answer was already the object"
		if coerced != "" {
			how += " (" + coerced + ")"
		}
		return delegate.Rescued{Structured: direct, How: how}, nil
	}
	var schemaMap map[string]any
	_ = json.Unmarshal(schema, &schemaMap) // repackStructuredWith reports an unparseable schema

	// Rung 3: this box's seat is not resident, and the node that wrote the answer holds its seat hot.
	began := time.Now()
	passed := "" // what an earlier rung did, carried to the note or the error of the next
	if origin, ok := delegate.RescueOriginFrom(ctx); ok {
		if resident, known := p.rescueSeatResident(ctx, seat); known && !resident {
			got, err := p.rescueOnOrigin(ctx, schemaMap, output, budget, origin, seat)
			if err == nil {
				return got, nil
			}
			passed = err.Error()
			log.Printf("rescue re-pack: the node that wrote the answer could not re-pack it (%v); this box's seat %s is next", err, seat)
			if budget > 0 {
				if budget -= time.Since(began); budget < rescueSecondTry {
					return delegate.Rescued{}, fmt.Errorf("%s; %s of the rescue's budget left, too little to warm and use this box's seat %s", passed, max(budget, 0).Round(time.Second), seat)
				}
			}
		}
	}

	// Rung 2 (and 4): this box's own seat. A seat that is not resident loads on the first request
	// that reaches it, and a cold load is minutes on a vLLM seat (125–250 s), far more than the
	// re-pack's allowance: the completion was cut by its own load and left a loaded seat nobody
	// used. Warm it first (D-64): a resident seat costs one /running read, and
	// agent_admission_wait_sec -1 turns the gate off.
	how := "one re-pack completion"
	warmTail := ""
	if _, warmNote, attempted := warmSeat(ctx, p.cfg.Endpoint, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec)); warmNote != "" {
		log.Printf("rescue re-pack: seat warm-up (%s): %s", seat, warmNote)
		// The warm-up rides the failure too: whether it spent the window is the first thing a
		// failed rescue on a fenced seat is asked, and the note used to be dropped with `how`.
		warmTail = " (seat warm-up: " + warmNote + ")"
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
	structured, tokens, _, _, err := p.repackStructuredWith(rctx, seat, schema, output, 0, 1, repackOpts{})
	if err != nil {
		own := fmt.Errorf("one re-pack completion on %s%s: %w", seat, warmTail, err)
		if passed != "" {
			return delegate.Rescued{}, fmt.Errorf("%s; %w", passed, own)
		}
		return delegate.Rescued{}, own
	}
	if passed != "" {
		how += "; " + passed
	}
	if kept := core.ListsTrimmed(structured, schemaMap, output); kept != "" {
		how += "; " + kept
	}
	return delegate.Rescued{Structured: structured, Seat: seat, TokensOut: tokens, How: how}, nil
}

// rescueSeatResident reads whether this box's agent seat is loaded: one /running read, the seat
// matched under its alias too. known is false when the read failed, and then the rescue stays where
// it always was: "could not tell" never moves work off the box.
func (p *Pipeline) rescueSeatResident(ctx context.Context, seat string) (resident, known bool) {
	sc, err := swapclient.New(p.cfg.Endpoint, admissionPoll)
	if err != nil {
		return false, false
	}
	rows, err := sc.Running(ctx)
	if err != nil {
		return false, false
	}
	m := newSeatMatcher(p.cfg.Endpoint, seat)
	ready := func() bool {
		for _, r := range rows {
			if m.matches(r.ID) && r.State == "ready" {
				return true
			}
		}
		return false
	}
	if ready() {
		return true, true
	}
	if m.resolve(ctx) && ready() {
		return true, true
	}
	return false, true
}

// rescueOnOrigin is ONE chat completion on the node that wrote the answer, on the seat that wrote
// it, through the node's chat door with the fleet bearer. The client's lane table names that one
// seat and node, and the call carries WithLocalBusy, so it leaves the box or does not run at all:
// a node that does not serve the seat, advertises no chat door, or is not reachable refuses it
// before any request (llamaclient.ErrLaneUnavailable) and the call never falls through to this
// box's endpoint, where a seat of the same name would load onto cards it must not evict. The
// grammar-free chat lane is the one surface every seat serves; the object is validated, coerced
// and read against its answer exactly as the chat lane of a node-side re-pack is.
func (p *Pipeline) rescueOnOrigin(ctx context.Context, schemaMap map[string]any, output string, budget time.Duration, o delegate.RescueOrigin, ownSeat string) (delegate.Rescued, error) {
	label := strings.TrimSpace(o.Node)
	if label == "" {
		label = o.Base
	}
	fail := func(err error) (delegate.Rescued, error) {
		return delegate.Rescued{}, fmt.Errorf("one re-pack completion on node %s (seat %s): %w", label, o.Seat, err)
	}
	if schemaMap == nil {
		return fail(errors.New("output_schema is not a JSON object"))
	}
	allow := agent.StallPolicy{
		Repack: livenessRepack, Floor: livenessFloor, Slack: livenessSlack, TokS: p.rescueTokS(o.Seat),
	}.Allowance(agent.PhaseRepack, expectedRepackTokens(output))
	if budget > 0 && allow > budget {
		allow = budget
	}
	rctx, cancel := context.WithTimeout(ctx, allow)
	defer cancel()
	rctx = seatwait.WithBudget(rctx, seatwait.NewBudget(p.cfg.SeatContentionWaitSec))
	tokens := repackBudget(output)
	resident, route := llamaclient.FleetLaneGates(p.cfg.FleetAuthToken)
	client := llamaclient.New(p.cfg.Endpoint, "/v1/chat/completions", p.cfg.Model, repackAttemptDeadline(rctx, p.cfg, tokens, 1)).
		WithSeatEndpoints(p.cfg.SeatEndpoints).
		WithRemoteLanes(map[string]string{o.Seat: o.Base}, func() bool { return false }, nil, resident).
		WithLaneRoute(route)
	reason := "this box's seat " + ownSeat + " is not resident; the node that wrote the answer holds its seat hot"
	structured, gres, coerced, err := p.repackViaChat(rctx, client, o.Seat, schemaMap, output, tokens, "", false, "", llamaclient.WithLocalBusy(reason))
	if err != nil {
		return fail(err)
	}
	how := fmt.Sprintf("one re-pack completion on node %s, which wrote the answer and holds its seat hot (this box's seat %s is not resident)", label, ownSeat)
	if coerced != "" {
		how += "; " + coerced
	}
	if kept := core.ListsTrimmed(structured, schemaMap, output); kept != "" {
		how += "; " + kept
	}
	return delegate.Rescued{Structured: structured, Seat: o.Seat, TokensOut: gres.TokensOut, How: how}, nil
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
