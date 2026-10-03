package pipeline

import (
	"log"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// RunSizing is one local run sized on its seat before it starts (register
// D-102): the published estimate (wall_estimate_sec / min_turn_sec / wall_note)
// and the least wall that holds any answer at all.
type RunSizing struct {
	// Estimate is the D-03 sizing — the arithmetic the delegation door
	// publishes beside every contract result — for the wall the run is under.
	Estimate seatrate.Estimate
	// MinViableSec is seatrate.MinViableSec for the same seat and contract: one
	// tool step and the smallest final at the seat's rate. 0 = no decode rate,
	// no opinion.
	MinViableSec int
}

// Refuses reports whether a wall of wallSec cannot hold even the smallest
// answer on this seat. A seat with no rate never refuses.
func (r RunSizing) Refuses(wallSec int) bool {
	return r.MinViableSec > 0 && wallSec > 0 && wallSec < r.MinViableSec
}

// SizeRun sizes one local run of contract on seat under a wall of timeoutSec,
// from this box's own measurements: the seat's remembered rate and cold load
// (seat-rates.json under the machine-wide state root), else — for the box's
// configured agent seat ONLY — the configured agent_seat_tok_s. That rate is the
// planner seat's (config.AgentSeatTokS), so it is never attributed to another
// seat: a model the caller named, or the seat composite placement picked, is
// sized from its own store entry or not at all (no rate = no floor = no
// refusal, and no published estimate).
//
// That is deliberately stricter than the delegation door's node. The node prices
// through the same arithmetic but sizes whatever seat it runs, a placed or
// overridden one included, with SeatPolicyFor, which still applies AgentSeatTokS
// to any seat the store has not measured; so for such a seat the two doors do NOT
// size alike, and this door is the right one. Register C-56 (a node-side refusal
// on est.Below) must stop that lending first, or it refuses a wall at the wrong
// seat's rate, the defect this function fixed on the agent_run door.
//
// The agent_run door calls it BEFORE any admission step: it reads one local
// file and dials nothing, so a refusal costs the caller no seat time. It only
// READS the store — never the pipeline's own seatRatesPath, which a run's
// record step writes.
func (p *Pipeline) SizeRun(contract core.AgentContract, seat string, timeoutSec int) RunSizing {
	cfg := p.cfg
	if seat != cfg.AgentPlannerModel("") {
		cfg.AgentSeatTokS = 0 // the configured rate belongs to the agent seat, not to this one
	}
	in := seatrate.InputFor(SeatPolicyFor(cfg, seat, p.rememberedSeatRate(seat)), contract)
	in.TimeoutSec = timeoutSec
	return RunSizing{Estimate: seatrate.Compute(in), MinViableSec: seatrate.MinViableSec(in)}
}

// rememberedSeatRate reads seat's remembered rate and cold load without
// touching any pipeline state. An unresolvable root or an unreadable store is
// "nothing remembered" (the configured rate then stands in), logged here
// because it is the one place a dead store shows on this path.
func (p *Pipeline) rememberedSeatRate(seat string) seatrate.Seat {
	root, err := gpulease.ResolveStateRoot(p.cfg.StateDir)
	if err != nil {
		log.Printf("agent run: seat-rates store unavailable: %v", err)
		return seatrate.Seat{}
	}
	s, lerr := seatrate.Load(seatrate.Path(root))
	if lerr != nil {
		log.Printf("agent run: %v", lerr)
	}
	return s.Get(seat)
}
