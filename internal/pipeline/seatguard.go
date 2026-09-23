// seatguard.go applies the cascade seat guard (internal/seatguard) to one
// call's chain: while a vLLM seat is loaded on this box, no rung whose load
// would evict it is sent to this box's llama-swap.
//
// Each rung the guard protects gets the first non-evicting door, in the order
// the existing routing rules already rank them:
//
//  1. its cascade lane, when the lane verifiably serves the SAME model — the
//     busy-aware lane rule (C-41): routing changes WHERE, never WHICH. The
//     rung stays in the chain and is sent WithLocalBusy, which makes the send
//     take the lane although neither lane busy gate fires (an idle loaded
//     seat is not "busy"; that is the eviction);
//  2. otherwise the loaded seat itself, as the rung: D-129 made a vLLM seat
//     an eligible cascade rung (structured_outputs, the non-thinking render),
//     and it is the model already holding the cards — the D-88 reasoning for
//     the in-loop tools, applied to the front door. A call that finds the seat
//     busy waits its turn in the seat's own queue; nothing is refused;
//  3. with neither — the reading is unknown and no loaded seat is known, and
//     no lane serves the rung — the configured rung, logged: there is no seat
//     the guard can name to protect, and no door it could send the call to
//     instead.
//
// The terminal reasoning tier is guarded too: kept when its lane serves it;
// otherwise its ATTEMPT runs on the loaded seat — still the reasoning attempt
// (one try, the reasoning budget, Reasoning=true, behind the grammar and
// truncation gate), with the seat's own structured_outputs in place of the
// think-wrapped GBNF a vLLM seat cannot take — and it is skipped only when
// that seat already answered this call.
package pipeline

import (
	"context"
	"log"
	"strings"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

// guardPlan is one call's chain after the guard.
type guardPlan struct {
	chain []string
	// lane holds the reason for every rung kept because its lane serves it:
	// the send carries WithLocalBusy(reason), and the climb must not probe the
	// rung's window on this box (tierNCtx asks /upstream/<rung>/props, which
	// LOADS the rung here).
	lane map[string]string
	// reasoningOn is the loaded seat the terminal reasoning attempt runs on,
	// when the reasoning tier would evict it and no lane serves it: the
	// attempt keeps its reasoning shape (one attempt, the reasoning budget,
	// Reasoning=true) on the seat, rather than joining the chain as a plain
	// rung (review, PR #448 round 2).
	reasoningOn string
	// reasoningLane is the reason when the reasoning tier rides its lane.
	reasoningLane string
	// laneSeat / reasoningSeat name the seat each lane rung protects ("" when
	// the verdict could name none): the door a rung takes when its lane is
	// gone at the send (review HIGH 2).
	laneSeat      map[string]string
	reasoningSeat string
}

// errClassLaneGone is the err_class of a send the client refused because the
// planned lane no longer served the rung (llamaclient.ErrLaneUnavailable). Run
// never lets it reach a ledger row: it takes the next door instead.
const errClassLaneGone = "lane_unavailable"

// laneGone returns the model a lane rung falls back to when its lane is gone
// at the send, and logs the divergence: the loaded seat the plan protected,
// or — with no seat known — the configured rung (the guard's documented
// residual for an unknown reading; never a refusal).
func (g guardPlan) laneGone(rung string) string {
	if seat := g.laneSeat[rung]; seat != "" {
		log.Printf("cascade seat guard: %s: lane gone at send (%s) -> served by the loaded seat %s", rung, g.lane[rung], seat)
		return seat
	}
	log.Printf("cascade seat guard: %s: lane gone at send (%s) -> the configured rung runs here: no loaded seat is known to substitute", rung, g.lane[rung])
	return rung
}

// reasoningLaneGone is laneGone for the terminal reasoning tier. It returns
// the seat to run as the final attempt, or "" when there is none to run: no
// seat known (the caller then runs the reasoning tier as configured) or the
// seat already answered in this call.
func (g guardPlan) reasoningLaneGone(reasoning string, tried map[string]bool) string {
	switch seat := g.reasoningSeat; {
	case seat == "":
		log.Printf("cascade seat guard: reasoning %s: lane gone at send (%s) -> the configured reasoning tier runs here: no loaded seat is known to substitute", reasoning, g.reasoningLane)
		return ""
	case tried[seat]:
		log.Printf("cascade seat guard: reasoning %s: lane gone at send (%s) -> not run: the loaded seat %s already answered this call", reasoning, g.reasoningLane, seat)
		return ""
	default:
		log.Printf("cascade seat guard: reasoning %s: lane gone at send (%s) -> served by the loaded seat %s", reasoning, g.reasoningLane, seat)
		return seat
	}
}

// opts returns the per-call options the guard adds for rung model.
func (g guardPlan) opts(model string) []llamaclient.GenOption {
	if why, ok := g.lane[model]; ok {
		return []llamaclient.GenOption{llamaclient.WithLocalBusy(why)}
	}
	return nil
}

func (g guardPlan) reasoningOpts() []llamaclient.GenOption {
	if g.reasoningLane != "" {
		return []llamaclient.GenOption{llamaclient.WithLocalBusy(g.reasoningLane)}
	}
	return nil
}

// seatVerdict asks the guard about one model (the test seam when set).
func (p *Pipeline) seatVerdict(ctx context.Context, model string) seatguard.Verdict {
	if p.seatVerdictFn != nil {
		return p.seatVerdictFn(ctx, model)
	}
	return p.seatGuard.Check(ctx, model)
}

// guardActive reports whether any guard is wired; with none the chain is
// returned untouched and no reading is taken.
func (p *Pipeline) guardActive() bool { return p.seatGuard != nil || p.seatVerdictFn != nil }

type rungDoor int

const (
	doorLocal rungDoor = iota // the configured rung, on this box
	doorLane                  // the configured rung, on its lane
	doorSeat                  // the loaded seat, as the rung
	// doorUnguarded: the guard protects a seat it cannot name (an unknown
	// reading) and no lane serves the rung, so the configured rung runs.
	doorUnguarded
)

// guardRung picks one rung's door. It logs nothing itself: planSeatGuard
// writes one line per door per call, because a chain of four protected rungs
// is one decision, not four.
//
// An open circuit breaker on the seat does not re-open the evicting door: a
// seat that is failing makes its own calls defer as infrastructure errors,
// and the guard never trades those for an eviction (under load a busy seat's
// timeouts are exactly when evicting it would hurt most).
func (p *Pipeline) guardRung(ctx context.Context, model string) (rungDoor, string, string) {
	v := p.seatVerdict(ctx, model)
	if !v.Protect {
		return doorLocal, "", ""
	}
	why := "cascade seat guard: " + v.Reason
	if p.client != nil && p.client.OffBoxFor(model) {
		return doorLane, why, v.Seat
	}
	if v.Seat != "" {
		return doorSeat, why, v.Seat
	}
	return doorUnguarded, why, ""
}

// planSeatGuard applies the guard to chain and to the terminal reasoning tier
// (reasoning = "" when this call will not run one). An inactive guard returns
// the chain as given — the pre-guard cascade, byte for byte.
func (p *Pipeline) planSeatGuard(ctx context.Context, chain []string, reasoning string) guardPlan {
	plan := guardPlan{chain: chain}
	if !p.guardActive() {
		return plan
	}
	out := make([]string, 0, len(chain)+1)
	add := func(m string) {
		for _, x := range out {
			if x == m {
				return
			}
		}
		out = append(out, m)
	}
	// One log line per door this call used: the first protected rung's reason,
	// then every rung that took the door.
	type doorLog struct {
		why   string
		seat  string
		rungs []string
	}
	logs := map[rungDoor]*doorLog{}
	note := func(door rungDoor, why, seat, rung string) {
		d := logs[door]
		if d == nil {
			d = &doorLog{why: why, seat: seat}
			logs[door] = d
		}
		d.rungs = append(d.rungs, rung)
	}
	for _, m := range chain {
		door, why, seat := p.guardRung(ctx, m)
		switch door {
		case doorLane:
			if plan.lane == nil {
				plan.lane, plan.laneSeat = map[string]string{}, map[string]string{}
			}
			plan.lane[m], plan.laneSeat[m] = why, seat
			add(m)
		case doorSeat:
			add(seat)
		default:
			add(m)
		}
		if door != doorLocal {
			note(door, why, seat, m)
		}
	}
	if reasoning != "" {
		door, why, seat := p.guardRung(ctx, reasoning)
		switch door {
		case doorLane:
			plan.reasoningLane, plan.reasoningSeat = why, seat
		case doorSeat:
			plan.reasoningOn = seat
		}
		if door != doorLocal {
			note(door, why, seat, "reasoning "+reasoning)
		}
	}
	plan.chain = out
	for _, door := range []rungDoor{doorLane, doorSeat, doorUnguarded} {
		d := logs[door]
		if d == nil {
			continue
		}
		rungs := strings.Join(d.rungs, ", ")
		switch door {
		case doorLane:
			log.Printf("%s -> served off this box by the cascade lane that serves each rung (%s)", d.why, rungs)
		case doorSeat:
			log.Printf("%s -> served by the loaded seat %s (no lane serves %s)", d.why, d.seat, rungs)
		case doorUnguarded:
			log.Printf("%s -> the configured rung runs: no lane serves %s and no loaded seat is known to substitute", d.why, rungs)
		}
	}
	return plan
}
