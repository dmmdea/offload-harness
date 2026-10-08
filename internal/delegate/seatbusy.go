// seatbusy.go: the delegator's "is this local seat busy" reading, offered to the lane doors (ADR 0078).
//
// The vision lane (ADR 0040 decision 6) sent an auto call to a fleet node only while the machine-wide GPU lease was
// held. A box whose vision seat was serving other requests, with no lease anywhere, still queued the next image behind
// them while a node with an idle vision seat sat free. The delegator already knows how to ask whether a local seat is
// busy: probeLocalBusy reads the seat's in-flight count through llama-swap (seatload.Inflight), treats a load in progress
// as busy, treats a seat whose load would unload another loaded vLLM seat as busy, and fails open to idle when it cannot
// read. LocalSeatBusy is that reading for a seat the caller names, through the same probeSeatBusy, so a lane door and the
// delegator mean the same thing by "busy".
//
// What it is not: the agent run-cap line (localRunCapRoom, localSlotAhead). That line counts the agent loops this
// harness registered on the seat (gpuactivity's run registry), and a single-shot vision call is not a registered run, so
// the line cannot see its load. And the threshold is the seat's own, not the agent cap: the cap (fleet_max_concurrent_jobs)
// is how many agent runs the harness allows on the agent seat at once and means nothing for a vision seat, so any request
// in flight reads busy, which is the reading the spread deal uses for the agent seat.

package delegate

import (
	"context"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

// SeatBusy is the reading of one local seat.
type SeatBusy struct {
	// Busy is true when the seat has a request in flight, has a load or an unload in progress (llama-swap lists both as a
	// state change the seat cannot be asked through), or is not loaded and loading it would unload another loaded vLLM
	// seat. A seat that cannot be read is not busy: the reading is a reason to place a call elsewhere, never a gate, so
	// "could not read" falls toward running where the call always ran.
	Busy bool
	// Why is the fact behind Busy, for a placement note: "2 in flight", "a load or unload is in progress", or the seat a
	// load would evict. Empty when the seat is not busy.
	Why string
}

// LocalSeatBusy reads whether the local seat named seat is busy right now. what names the seat's role for the log
// ("vision seat"). An empty seat or a box with no local endpoint reads idle: there is nothing to queue behind.
func LocalSeatBusy(ctx context.Context, cfg config.Config, seat, what string) SeatBusy {
	return localSeatBusy(ctx, cfg, seat, what, func(c context.Context, model string) seatguard.Verdict {
		return seatguard.Shared(cfg).Check(c, model)
	})
}

// localSeatBusy is LocalSeatBusy with the seat guard as a parameter, so a test can name the occupant.
func localSeatBusy(ctx context.Context, cfg config.Config, seat, what string, guard func(context.Context, string) seatguard.Verdict) SeatBusy {
	seat = strings.TrimSpace(seat)
	if seat == "" {
		return SeatBusy{}
	}
	rd := probeSeatBusy(ctx, cfg, seat, what, guard)
	if !rd.busy {
		return SeatBusy{}
	}
	switch {
	case rd.occupiedBy != "":
		return SeatBusy{Busy: true, Why: "loading it would evict the loaded vLLM seat " + rd.occupiedBy}
	case rd.loading:
		return SeatBusy{Busy: true, Why: "a load or unload is in progress"}
	}
	return SeatBusy{Busy: true, Why: fmt.Sprintf("%d in flight", rd.inflight)}
}
