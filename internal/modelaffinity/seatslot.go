package modelaffinity

// The LOCAL run cap (register C-42, diagnosis S-07 / W-09).
//
// A fleet node caps the jobs it accepts from the fleet (FleetConcurrencyLimit,
// "queue full" 503 with Retry-After). Nothing capped the runs started on the
// box itself — agent_run from the MCP door, a delegation's local leg, a lab
// replay: the run registry (internal/gpuactivity) counted them and "never
// gated anything by itself". Sixteen local contracts could land on one seat at
// once, each spending its wall in the engine's queue.
//
// AwaitSeatSlot is the gate: wait while the registered runs on the seat (by
// its configured name and its canonical id, the two spellings one seat has)
// number cap or more, polled at leasePollInterval, bounded by the caller's
// admission deadline — a queue position, never a refusal (ADR 0032). The
// caller's own record is excluded by id so a run that registered before it
// waits does not count itself.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// SeatRuns is what AwaitSeatSlot reads: the registered runs on the named
// seats (gpuactivity.Registry.OnSeat satisfies it).
type SeatRuns func(now time.Time, names ...string) []gpuactivity.Run

// SeatSlotError is a run refused at the seat cap: the deadline passed with
// the seat still full.
type SeatSlotError struct {
	Seat   string
	Cap    int
	Ahead  int
	Waited time.Duration
	cause  error
}

func (e *SeatSlotError) Error() string {
	return fmt.Sprintf("seat %s is at its local run cap (%d registered, cap %d) after waiting %s: no slot freed before the admission deadline — the runs ahead keep the seat, this one is re-placeable (%v)",
		e.Seat, e.Ahead, e.Cap, e.Waited.Round(time.Second), e.cause)
}

func (e *SeatSlotError) Unwrap() error { return e.cause }

// AwaitSeatSlot waits until fewer than cap runs other than selfID are
// registered on seat (or canonical), or the deadline passes. cap <= 0 or a
// nil reader disables the gate.
func AwaitSeatSlot(ctx context.Context, runs SeatRuns, seat, canonical, selfID string, cap int, deadline time.Time) error {
	if runs == nil || cap <= 0 || strings.TrimSpace(seat) == "" {
		return nil
	}
	start := time.Now()
	count := func() int {
		n := 0
		for _, r := range runs(time.Now(), seat, canonical) {
			if r.ID != selfID {
				n++
			}
		}
		return n
	}
	ahead := count()
	if ahead < cap {
		return nil
	}
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return &SeatSlotError{Seat: seat, Cap: cap, Ahead: ahead, Waited: time.Since(start), cause: context.DeadlineExceeded}
		}
		if remain > leasePollInterval {
			remain = leasePollInterval
		}
		select {
		case <-ctx.Done():
			return &SeatSlotError{Seat: seat, Cap: cap, Ahead: ahead, Waited: time.Since(start), cause: ctx.Err()}
		case <-time.After(remain):
		}
		if ahead = count(); ahead < cap {
			return nil
		}
	}
}

// IsSeatSlotError reports whether err is a seat-cap refusal.
func IsSeatSlotError(err error) bool {
	var e *SeatSlotError
	return errors.As(err, &e)
}
