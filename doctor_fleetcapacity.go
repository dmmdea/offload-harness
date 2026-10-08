package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// writeFleetCapacitySection prints one row comparing the workers this box publishes
// (fleet_max_concurrent_jobs, default 4) with the slots its agent seat really serves, read from the
// LIVE serving config (serving_config_path), never from a template: a live yaml can differ from what
// the installer would render today.
//
// The delegator deals a node as many jobs as the node's published workers leave free. On a seat that
// serves --parallel 1 the engine runs one request at a time, so a node that publishes four holds three
// jobs that only wait inside the seat, and that wait is charged to the job's wall while the delegator
// counts the three as parallel work. The same key is also this box's own run-cap line as a delegator,
// so the row reads the same on a box that only delegates.
//
//	capacity:   OK    fleet_max_concurrent_jobs 1 = <seat> --parallel 1 (<yaml>)
//	capacity:   OK    fleet_max_concurrent_jobs 1 is below <seat> --parallel 4 (<yaml>): ...
//	capacity:   WARN  fleet_max_concurrent_jobs 4 exceeds <seat> --parallel 1 (<yaml>): 3 of 4 workers wait ...
//	capacity:   UNKNOWN fleet_max_concurrent_jobs 4: <why the slots could not be read>
//
// The slots are the seat's own statement: a llama.cpp entry's --parallel (a stated -1 or auto is
// llama-server's 4 slots), or, for an entry that is not llama.cpp, its llama-swap concurrencyLimit, which
// is how a vLLM seat publishes its max_num_seqs. The cap has to follow the seat this box serves NOW, so
// a WARN says to set it for that seat and to set it again when the seat changes. A box of a vLLM tier
// (the embedded tier table declares a vllm_seat) whose venv is not installed serves that seat's llama.cpp
// fallback, and the row then names the vLLM seat the tier means and the cap to go back to.
//
// It is informational: it never changes doctor's exit code (like the skew and token rows), and it
// prints nothing on a config with no agent seat, so config.Default() stays as short as it is. UNKNOWN
// says which unknown it is: the path is unset, the file cannot be read, or the seat states no slots.
func writeFleetCapacitySection(w io.Writer, cfg config.Config) {
	seat := strings.TrimSpace(cfg.AgentModel)
	if seat == "" {
		return
	}
	limit := cfg.FleetConcurrencyLimit()
	have := fmt.Sprintf("fleet_max_concurrent_jobs %d", limit)
	if limit == 0 {
		have = "fleet_max_concurrent_jobs is unlimited (negative)"
	}
	unknown := func(why string) {
		fmt.Fprintf(w, "capacity:   UNKNOWN %s: %s\n", have, why)
	}
	path := strings.TrimSpace(cfg.ServingConfigPath)
	if path == "" {
		unknown(fmt.Sprintf("serving_config_path is unset, so the slots of agent seat %q cannot be read (set it to the llama-swap config this box serves)", seat))
		return
	}
	text, err := os.ReadFile(path)
	if err != nil {
		unknown(fmt.Sprintf("serving_config_path %s cannot be read, so the slots of agent seat %q are unknown: %v", path, seat, err))
		return
	}
	read, why := servingtmpl.SeatSlotsWhy(string(text), seat)
	if why != "" {
		unknown(fmt.Sprintf("the slots of agent seat %q are not in %s: %s", seat, path, why))
		return
	}
	slots := read.N
	tier := vllmTierOf(cfg, seat)
	where := fmt.Sprintf("%s %s (%s)", seat, slotLabel(read, tier), path)
	note := tier.note(limit, slots)
	switch {
	case limit == slots:
		fmt.Fprintf(w, "capacity:   OK    %s = %s%s\n", have, where, note)
	case limit > 0 && limit < slots:
		fmt.Fprintf(w, "capacity:   OK    %s is below %s: the seat has slots the workers never fill%s\n", have, where, note)
	case limit == 0:
		fmt.Fprintf(w, "capacity:   WARN  %s over %s: %s; %s%s\n", have, where, overConsequence(read, 0, slots), followTheSeat(slots), note)
	default:
		fmt.Fprintf(w, "capacity:   WARN  %s exceeds %s: %s; %s%s\n", have, where, overConsequence(read, limit, slots), followTheSeat(slots), note)
	}
}

// slotLabel names a seat's slots the way its config states them: --parallel N for a llama.cpp entry
// (a stated -1 or auto, which llama-server serves as 4 slots, says so), max_num_seqs N for a vLLM seat the
// tier table knows (its concurrencyLimit is set to it), else the entry's concurrencyLimit.
func slotLabel(read servingtmpl.SeatSlots, tier vllmTier) string {
	switch {
	case read.Limit && tier.serving:
		return fmt.Sprintf("max_num_seqs %d", read.N)
	case read.Limit:
		return fmt.Sprintf("concurrencyLimit %d", read.N)
	case read.Auto:
		return fmt.Sprintf("--parallel auto, %d slots", read.N)
	}
	return fmt.Sprintf("--parallel %d", read.N)
}

// overConsequence says what a cap above the slots costs: the jobs past the slots of a llama.cpp seat
// wait inside it, and the jobs past a concurrencyLimit are answered 429 by llama-swap. limit is 0 for an
// unlimited cap.
func overConsequence(read servingtmpl.SeatSlots, limit, slots int) string {
	switch {
	case read.Limit && limit == 0:
		return "every job past the entry's concurrencyLimit is answered 429 by llama-swap instead of served"
	case read.Limit:
		return fmt.Sprintf("%d of %d workers are past the entry's concurrencyLimit, where llama-swap answers 429 instead of serving them", limit-slots, limit)
	case limit == 0:
		return fmt.Sprintf("every job the node admits runs against %s, and the wait counts against the delegator's wall", slotsPhrase(slots))
	}
	return fmt.Sprintf("%d of %d workers wait behind %s, and that wait counts against the delegator's wall", limit-slots, limit, slotsPhrase(slots))
}

// followTheSeat is the advice of a WARN: the cap is the slots of the seat this box serves now, and it
// moves when the seat does (a vLLM seat serves max_num_seqs requests at once, far above a fallback's).
func followTheSeat(slots int) string {
	return fmt.Sprintf("set fleet_max_concurrent_jobs %d to match the seat this box serves now, and change it again whenever the seat changes (to max_num_seqs on a vLLM seat)", slots)
}

// slotsPhrase names a seat's generation slots for the capacity row.
func slotsPhrase(slots int) string {
	if slots == 1 {
		return "one generation slot"
	}
	return fmt.Sprintf("%d generation slots", slots)
}

// vllmTier is what the embedded tier table says about the agent seat of the tier this box is installed as,
// when that tier declares a vLLM seat. The zero value is "no such seat to speak of", and so is a value with
// neither flag set (the operator chose an agent seat that is neither the vLLM seat nor its fallback): the row
// reads the two flags and nothing else.
type vllmTier struct {
	tier string
	seat vllmseat.Spec
	// serving: agent_model is the vLLM seat (its id or an alias), so the box runs it.
	serving bool
	// fallback: agent_model is the seat's llama.cpp fallback, which is what a box without the vLLM venv
	// serves in its place.
	fallback bool
}

// vllmTierOf finds the vLLM seat the tier of this box declares and whether the box serves it or its
// fallback. The tier is the one the installer recorded (installed.json, the source the report row reads
// first), else the config's tier_profile, which tierseed seeds for composite tiers only. A box whose
// tier is unknown, declares no vLLM seat, or whose agent_model is neither the seat nor its fallback (the
// operator chose another) gets no tier clause: the row never guesses a tier.
func vllmTierOf(cfg config.Config, agent string) vllmTier {
	id := cfg.TierProfile
	if info, err := fleetnode.ReadInstalledInfo(fleetnode.InstalledJSONPath()); err == nil && info.Profile != "" {
		id = info.Profile
	}
	if id == "" {
		return vllmTier{}
	}
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		return vllmTier{}
	}
	p, ok := profiles[id]
	if !ok || p.VLLMSeat == nil {
		return vllmTier{}
	}
	t := vllmTier{tier: id, seat: *p.VLLMSeat}
	switch {
	case agent == t.seat.ID:
		t.serving = true
	case agent == t.seat.Fallback:
		t.fallback = true
	default:
		for _, a := range t.seat.Aliases {
			if agent == a {
				t.serving = true
			}
		}
	}
	return t
}

// note is the tier clause appended to the row, empty when there is nothing to add. On the fallback it
// says the tier means a vLLM seat and what the cap goes back to when that seat takes over; on the vLLM
// seat itself it says a cap below its max_num_seqs leaves its slots idle.
func (t vllmTier) note(limit, slots int) string {
	switch {
	case t.fallback:
		return fmt.Sprintf("; tier %s declares the vLLM agent seat %s (max_num_seqs %d) and this box serves its llama.cpp fallback %s until the vLLM venv is installed, so raise fleet_max_concurrent_jobs to %d when %s takes over",
			t.tier, t.seat.ID, t.seat.MaxNumSeqs, t.seat.Fallback, t.seat.MaxNumSeqs, t.seat.ID)
	case t.serving && limit > 0 && limit < slots:
		return fmt.Sprintf("; %s is tier %s's vLLM seat, so raise fleet_max_concurrent_jobs toward its max_num_seqs %d to use them", t.seat.ID, t.tier, slots)
	}
	return ""
}
