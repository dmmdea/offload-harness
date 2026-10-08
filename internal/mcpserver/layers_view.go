package mcpserver

import (
	"context"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/displaywatch"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// layersViewTimeout bounds the whole layer-row render inside a status call.
// The readers behind it have their own budgets (a 5 s nvidia-smi, a 4 s seat
// probe, an instant presence read) and a 2 s memo in front of them, but they
// are several and status is the call a session makes to find out whether the
// box is usable at all: a stalled probe must cost a bounded delay and a
// degraded answer, never the whole surface.
const layersViewTimeout = 6 * time.Second

// layersView renders the layer rows offload_status publishes for the LOCAL box:
// the declared spec of every layer and seat, the live occupancy of each seat,
// and the layer's own admissibility verdict. nil on a non-composite box, which
// is what keeps such a box's payload byte-identical to the pre-layer build.
//
// It never loads a model — occupancy stops at /running for a cold seat — and it
// never blocks the caller for longer than layersViewTimeout: on a timeout it
// answers with the SPEC rows alone (no live readings), because a layer table
// with unknown occupancy is still the truth about what the box declares, while
// a missing table reads as "this box has no layers", which is a different and
// false statement.
func layersView(ctx context.Context, cfg config.Config) []placement.LayerRow {
	if !cfg.Composite() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, layersViewTimeout)
	defer cancel()
	done := make(chan []placement.LayerRow, 1)
	go func() {
		// SharedSnapshot, not a fresh one: status runs beside delegation on the
		// same process and must read through the same 2 s memo rather than exec
		// its own nvidia-smi.
		done <- placement.RowsFromConfig(cfg, placement.SharedSnapshot(cfg).Live())
	}()
	select {
	case rows := <-done:
		return rows
	case <-ctx.Done():
		return placement.RowsFromConfig(cfg, placement.Live{})
	}
}

// withDisplayStatus adds, to the local block of offload_status, what a composite box owes about the
// display card's gate: the operator-presence reading and mode (the layer's presence guard and the card
// allocator both read that key), and the post-admission guard's heartbeat and last action (ADR 0075).
// A box that seeds no layers gets its block back untouched, which is what keeps its payload byte for
// byte what it was (the golden pins it); display_guard is omitted unless the watcher has something
// to say.
func withDisplayStatus(ctx context.Context, cfg config.Config, local map[string]any) map[string]any {
	if !cfg.Composite() {
		return local
	}
	local["operator_presence"] = presenceView(ctx, cfg)
	if v := displaywatch.StatusView(cfg, time.Now()); v != nil {
		local["display_guard"] = v
	}
	return local
}

// presenceView is the operator-presence reading offload_status publishes beside the layer rows, on a
// composite box only (the key is the display card's gate: the layer's presence guard and the card
// allocator both read it). It is the probe's own reading for the configured mode, from the same 2 s
// memo the guards read through, so what an operator reads here is what the guards just saw. It never
// blocks the call: a probe that does not answer inside layersViewTimeout is reported as such, with the
// mode, which is configuration and needs no probe.
func presenceView(ctx context.Context, cfg config.Config) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, layersViewTimeout)
	defer cancel()
	done := make(chan placement.Presence, 1)
	go func() { done <- placement.SharedSnapshot(cfg).Presence() }()
	select {
	case p := <-done:
		return presenceViewFor(cfg, p)
	case <-ctx.Done():
		return map[string]any{"mode": cfg.PresenceMode(), "note": "the presence probe did not answer in time"}
	}
}

// presenceViewFor renders one presence reading. admits is the presence guard's own verdict on it (the
// rule the guard and the check on a loaded twin share, placement.PresenceAllows); idle_threshold_sec
// is what `auto` measures the last input against. An `away` mode carries the caution that it is the
// operator's unconditional override: it admits whether or not anyone is at the desk, because it never
// reads the session (ADR 0052 D4), so `auto` is the mode that protects the operator.
func presenceViewFor(cfg config.Config, p placement.Presence) map[string]any {
	admits, reading := placement.PresenceAllows(p)
	view := map[string]any{
		"mode":    cfg.PresenceMode(),
		"known":   p.Known,
		"away":    p.Away,
		"locked":  p.Locked,
		"admits":  admits,
		"reading": reading,
	}
	if p.IdleSec > 0 {
		view["idle_sec"] = p.IdleSec
	}
	if p.Note != "" {
		view["note"] = p.Note
	}
	switch cfg.PresenceMode() {
	case "auto":
		view["idle_threshold_sec"] = int(cfg.OperatorIdle() / time.Second)
	case "away":
		view["caution"] = "away is the operator's unconditional override: it admits whether or not anyone is at the desk (no idle, lock or fullscreen test), so the display card opens at the keyboard too. auto is the supported mode"
	}
	return view
}

// withPlaced publishes a placement block on a result map, and publishes
// NOTHING when there is none: a non-composite box's agent_run payload must
// carry no new key at all (the byte-identity constraint), and a reader that
// finds no `placed` is looking at one implicit layer, never at an unplaced run.
func withPlaced(out map[string]any, placed *core.Placed) {
	if placed == nil {
		return
	}
	out["placed"] = placed
}

// placeAgentRun resolves the planner seat for one agent_run and, on a composite
// box, the placement block that run publishes. It returns (seat, placed, nil)
// to proceed, or (_, _, refusal) with the deferred-shape payload when placement
// refuses — the front door's house rule: a refusal is a result, never an error.
//
// Seat resolution, in the order the doors document:
//
//   - an EXPLICIT model wins — the caller named a seat and gets it — but it is
//     GUARDED when that seat belongs to an opt-in layer. Walking an explicit
//     model past the display-card guards is exactly how a 10.5 GiB load lands
//     on the desktop's card while the operator is at it, so the layer's own
//     admissibility is evaluated before the seat is touched, and a dormant
//     layer is refused outright (only the operator wakes one).
//   - otherwise the placement table decides, and its seat wins over
//     agent_model: that is what "the box routes per task" means. A decision
//     that names no seat (a router rung) falls back to the configured planner.
//   - a box with no layers decides nothing and keeps cfg.AgentPlannerModel,
//     byte for byte.
//
// A decision that asks to WAIT is not a refusal here: this door has no capacity
// wait, and the seat's own admission gate (modelaffinity.AwaitRunSlot) already
// holds the run until the cards are free. The reason still rides in `placed`,
// so the caller can see the eviction it paid for.
func placeAgentRun(cfg config.Config, explicit, goal, contextClass string, timeoutSec int) (string, *core.Placed, map[string]any) {
	if err := core.ValidateContextClass(contextClass); err != nil {
		return "", nil, map[string]any{"deferred": true, "reason": err.Error()}
	}
	seat := cfg.AgentPlannerModel(explicit)
	if !cfg.Composite() {
		return seat, nil, nil
	}
	live := placement.SharedSnapshot(cfg).Live()
	if explicit != "" {
		l, s, ok := layerSeatByModel(cfg, seat)
		if !ok || !(l.OptIn || l.Dormant) {
			// Not a guarded seat: the caller's choice stands, unrecorded —
			// placement did not make it, so placement does not claim it.
			return seat, nil, nil
		}
		placed := &core.Placed{Tier: l.Tier, Layer: l.Name, Role: s.Role, Seat: seat, Devices: s.DeviceList(), CtxTokens: s.CtxTokens}
		if l.Dormant {
			placed.Reason = "layer " + l.Name + " is dormant (operator decision) — an explicit seat does not wake it"
			return "", placed, map[string]any{"deferred": true, "reason": placed.Reason, "placed": placed}
		}
		admitted, reason, guard := placement.LayerAdmissible(l, s, live, nil)
		placed.Reason = reason
		if !admitted {
			placed.Guard = guard
			return "", placed, map[string]any{"deferred": true, "reason": "layer " + l.Name + " refused " + seat + " — " + reason, "placed": placed}
		}
		return seat, placed, nil
	}
	contract := core.AgentContract{Goal: goal, TimeoutSec: timeoutSec, ContextClass: contextClass}
	req := placement.RequestForContract(contract, placement.EstimateTokens(contract), cfg.AgentMaxTokens)
	dec := placement.Decide(req, cfg.Layers, live)
	placed := dec.Placed
	if dec.Defer {
		return "", &placed, map[string]any{"deferred": true, "reason": dec.Reason, "defer_class": dec.DeferClass, "placed": &placed}
	}
	if placed.Seat != "" {
		seat = placed.Seat
	}
	return seat, &placed, nil
}

// layerSeatByModel finds the layer and seat a model name belongs to, matching a
// router seat's model_map twins too (the display layer's rungs are named only
// there). ok=false when no layer declares the model — the common case for a
// box whose planner is not a layer seat at all.
func layerSeatByModel(cfg config.Config, model string) (config.LayerSpec, config.LayerSeat, bool) {
	for _, l := range cfg.Layers {
		for _, s := range l.Seats {
			if s.Model == model {
				return l, s, true
			}
			for _, twin := range s.ModelMap {
				if twin == model {
					return l, s, true
				}
			}
		}
	}
	return config.LayerSpec{}, config.LayerSeat{}, false
}
