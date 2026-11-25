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
//
// A cap nobody set is only where the line starts (SeatMore): past it the first
// run in line is admitted whenever the engine itself says it takes one more.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/seatload"
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
	// Engine is why the engine took no run past the cap, as last read; "" when the cap is fixed or it was never asked.
	Engine string
	cause  error
}

func (e *SeatSlotError) Error() string {
	engine := ""
	if e.Engine != "" {
		engine = "; no run was admitted past the cap: " + e.Engine
	}
	return fmt.Sprintf("seat %s is at its local run cap (%d runs ahead of this one, cap %d) after waiting %s: no slot freed before the wait's deadline (the run's wall)%s — the runs ahead keep the seat, this one is re-placeable (%v)",
		e.Seat, e.Ahead, e.Cap, e.Waited.Round(time.Second), engine, e.cause)
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
	return AwaitSeatLine(ctx, runs, seat, canonical, selfID, cap, nil, deadline, tick)
}

// SeatMore asks the seat's engine whether it takes one more run while held runs hold a slot on the seat, and says what the answer
// rests on. nil = the cap is fixed and the engine is not asked.
type SeatMore func(ctx context.Context, held int) (ok bool, why string)

// EngineRoom is the SeatMore of the seat served behind the llama-swap at endpoint (seatload.ReadRoom). nil when there is no endpoint
// or no seat to ask.
func EngineRoom(endpoint, seat string) SeatMore {
	endpoint, seat = strings.TrimSpace(endpoint), strings.TrimSpace(seat)
	if endpoint == "" || seat == "" {
		return nil
	}
	return func(ctx context.Context, held int) (bool, string) {
		rctx, cancel := context.WithTimeout(ctx, engineRoomTimeout)
		defer cancel()
		room := seatload.ReadRoom(rctx, engineRoomClient, endpoint, seat, held)
		return room.OK, room.Why
	}
}

// engineRoomTimeout bounds one EngineRoom question: two readings of the engine a second apart.
const engineRoomTimeout = 6 * time.Second

var engineRoomClient = &http.Client{Timeout: 3 * time.Second}

// SeatLine is how the runs registered on a seat stand for one run: a run in line, or one that is not registered yet.
type SeatLine struct {
	// Held is the runs past admission: each holds a slot.
	Held int
	// Fresh is how many of those have produced nothing yet (loading, being probed, reading their first prompt), so the engine's
	// present rate does not show what they will cost.
	Fresh int
	// Waiting is the runs in admission that come before this one.
	Waiting int
}

// LineOf counts the line ahead of the run selfID (register C-60, 0.143.0): every run
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
func LineOf(list []gpuactivity.Run, selfID string) SeatLine {
	var selfStart int64
	found := false
	for _, r := range list {
		if r.ID == selfID {
			selfStart, found = r.StartedAtMs, true
			break
		}
	}
	var l SeatLine
	for _, r := range list {
		if r.ID == selfID {
			continue
		}
		if r.Phase != gpuactivity.PhaseAdmission {
			l.Held++
			if r.TokensOut == 0 && r.Step == 0 {
				l.Fresh++
			}
			continue
		}
		if !found || r.StartedAtMs < selfStart || (r.StartedAtMs == selfStart && r.ID < selfID) {
			l.Waiting++ // a waiter ahead in line (or one past the gate still in its pre-flight)
		}
	}
	return l
}

// Ahead is the number of runs ahead of this one: the ones that hold a slot and the ones in line before it.
func (l SeatLine) Ahead() int { return l.Held + l.Waiting }

// PastCap reports whether the engine takes this run although the line is at its cap. The runs past the cap enter one at a time:
// only the first in line is admitted, only when every run that holds a slot has produced something (the last one in has shown what
// it costs), and only on the engine's own word. why says what the answer rests on; "" when there is no engine to ask.
func (l SeatLine) PastCap(ctx context.Context, more SeatMore) (ok bool, why string) {
	switch {
	case more == nil:
		return false, ""
	case l.Waiting > 0:
		return false, fmt.Sprintf("%d run(s) come first in line", l.Waiting)
	case l.Fresh > 0:
		return false, fmt.Sprintf("%d of the %d run(s) on the seat have produced nothing yet", l.Fresh, l.Held)
	}
	return more(ctx, l.Held)
}

// AwaitSeatLine is AwaitSeatSlotReporting for a line whose cap is where it starts and not where it ends: while the cap is reached,
// the first run in line also asks more (the engine) at every poll, and is admitted on its yes. A nil more is the fixed cap.
func AwaitSeatLine(ctx context.Context, runs SeatRuns, seat, canonical, selfID string, cap int, more SeatMore, deadline time.Time, tick func(ahead int)) error {
	if runs == nil || cap <= 0 || strings.TrimSpace(seat) == "" {
		return nil
	}
	start := time.Now()
	ahead, engine := 0, ""
	admitted := func() bool {
		line := LineOf(runs(time.Now(), seat, canonical), selfID)
		if ahead = line.Ahead(); ahead < cap {
			return true
		}
		ok, why := line.PastCap(ctx, more)
		if ok {
			log.Printf("seat line (%s): run admitted past the cap of %d with %d on the seat: %s", seat, cap, line.Held, why)
			return true
		}
		engine = why
		return false
	}
	if admitted() {
		return nil
	}
	for {
		if tick != nil {
			tick(ahead)
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return &SeatSlotError{Seat: seat, Cap: cap, Ahead: ahead, Waited: time.Since(start), Engine: engine, cause: context.DeadlineExceeded}
		}
		if remain > leasePollInterval {
			remain = leasePollInterval
		}
		select {
		case <-ctx.Done():
			return &SeatSlotError{Seat: seat, Cap: cap, Ahead: ahead, Waited: time.Since(start), Engine: engine, cause: ctx.Err()}
		case <-time.After(remain):
		}
		if admitted() {
			return nil
		}
	}
}

// IsSeatSlotError reports whether err is a seat-cap refusal.
func IsSeatSlotError(err error) bool {
	var e *SeatSlotError
	return errors.As(err, &e)
}
