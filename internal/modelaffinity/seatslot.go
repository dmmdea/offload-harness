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
	return fmt.Sprintf("seat %s is at its local run cap (%d runs ahead of this one, cap %d) after waiting %s: no slot freed before the wait's deadline (the run's wall) — the runs ahead keep the seat, this one is re-placeable (%v)",
		e.Seat, e.Ahead, e.Cap, e.Waited.Round(time.Second), e.cause)
}

func (e *SeatSlotError) Unwrap() error { return e.cause }

// SeatCapDeadline is how long a run may wait in line at the seat cap
// (register C-60, 0.143.0): as long as its own wall, never less than the
// admission deadline the cap wait used to share, never past the caller's
// context deadline. Waiting in line is queueing, not admission — the shared
// 300 s budget refused 88 of the 2026-09-29 cap waits after exactly 5m0s, most
// with the seat about to free — and INV-4 queues rather than refuses. Callers
// move the admission deadline out by the time spent in line, so the pre-flight,
// the cold load and the probes keep their whole budget.
func SeatCapDeadline(ctx context.Context, now time.Time, wall time.Duration, admissionEnd time.Time) time.Time {
	end := now.Add(wall)
	if end.Before(admissionEnd) {
		end = admissionEnd
	}
	if dl, ok := ctx.Deadline(); ok && dl.Before(end) {
		end = dl
	}
	return end
}

// AwaitSeatSlot waits until fewer than cap runs other than selfID are
// registered on seat (or canonical), or the deadline passes. cap <= 0 or a
// nil reader disables the gate.
func AwaitSeatSlot(ctx context.Context, runs SeatRuns, seat, canonical, selfID string, cap int, deadline time.Time) error {
	return AwaitSeatSlotReporting(ctx, runs, seat, canonical, selfID, cap, deadline, nil)
}

// AwaitSeatSlotReporting is AwaitSeatSlot with a heartbeat: tick (when set)
// is called once when the run joins the line and after every poll while it
// stays there, with the number of runs still ahead of it. A run that waits in
// line publishes it so a remote delegator follows the wait on evidence
// (register C-60) instead of giving up a job the node still holds.
func AwaitSeatSlotReporting(ctx context.Context, runs SeatRuns, seat, canonical, selfID string, cap int, deadline time.Time, tick func(ahead int)) error {
	if runs == nil || cap <= 0 || strings.TrimSpace(seat) == "" {
		return nil
	}
	start := time.Now()
	// count is the runs AHEAD of this one (register C-60, 0.143.0): every run
	// past admission holds a slot, but a run still in admission counts only
	// when it arrived first (FIFO by registration time, the id breaking a tie).
	// Until 0.143.0 every other registered run counted, waiters included — and
	// every waiter registers in admission before it reaches this gate — so
	// once cap waiters stacked up they blocked EACH OTHER with the seat free:
	// 92 local runs refused on 2026-09-29 after the full admission budget while
	// slots stood open. A waiter behind this one never blocks it, and when a
	// slot frees exactly the next waiter in line sees room — never all of them
	// at once. Unregistered (self not listed): every waiter counts, the old
	// conservative reading.
	count := func() int {
		list := runs(time.Now(), seat, canonical)
		var selfStart int64
		found := false
		for _, r := range list {
			if r.ID == selfID {
				selfStart, found = r.StartedAtMs, true
				break
			}
		}
		n := 0
		for _, r := range list {
			if r.ID == selfID {
				continue
			}
			if r.Phase != gpuactivity.PhaseAdmission {
				n++ // holds a slot
				continue
			}
			if !found || r.StartedAtMs < selfStart || (r.StartedAtMs == selfStart && r.ID < selfID) {
				n++ // a waiter ahead in line (or one past the gate still in its pre-flight)
			}
		}
		return n
	}
	ahead := count()
	if ahead < cap {
		return nil
	}
	for {
		if tick != nil {
			tick(ahead)
		}
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
