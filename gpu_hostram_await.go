package main

// F82: where a lease that unloads the agent seat is compared to the host.
//
// `gpu reserve --ram N --drain --unload-seat` used to be compared to the host inside Acquire, and the drain and the
// unload ran only after the lease was held. The memory it waited for was held by the seat its own unload frees: a
// film lease that declared its host RAM waited about six hours while the 3-card seat held 67 GiB of the host's
// commit, for a grant it could only get by being granted (the same class as the DRAINING stamp, gpu_drain.go, 0.117.0:
// the gate in front of the step that clears it).
//
// The order now, for a lease that asks for --unload-seat, declares host RAM, whose unload reaches a model on its cards
// (unloadReachesLease) and whose host can be read (screenDeferredHostRAM):
//
//  1. the need is refused at once when no state of the box could ever admit it (waiting cannot help, and a seat
//     cycle for a lease that can never run is bought for nothing);
//  2. the grant takes the place in line and the cards, declaring nothing (Options.HostRAMWaitGiB carries the
//     figure; the allocator's host term is off for the same reason), so the cards are fenced and no seat reloads
//     onto them;
//  3. the drain and the unload run, as they always did, under the lease;
//  4. only then Manager.AwaitHostRAM puts the one admission rule to the figure, still holding the cards, with the
//     same words, inside the same --wait AND the hold bound (gpu_hostram_hold_max_sec, default 600 s, --ram-hold-max,
//     0 = none), and the figure becomes the lease's declared need in the same critical section that admitted it;
//     when the bound passes first the lease is released (the seat warmed back as for any release), nothing runs, and
//     the reserve fails with one line (errHostRAMHoldBound);
//  5. the command starts.
//
// Every other lease is untouched and compares in the grant: a lease without --unload-seat (--drain alone frees no host
// memory, so cordoning the seat for a wait the drain cannot shorten would only cost the seat's callers), a lease whose
// unload reaches nothing on its cards (nothing it frees can make room for it), and a lease whose host cannot be read
// (the look after the unload would refuse for the same reason until its deadline). What no screen can know before the
// grant is whether the unload frees ENOUGH: how much of the host's commit the seat holds is not measured, the figure the
// config can state (agent_seat_host_ram_gib) is a floor and its default is a chosen one, and a prediction taken when the
// reserve starts is stale when the grant comes, hours later; a screen that sent the lease back to the grant on it would
// bring the deadlock back whenever the cause changed in between. So the comparison after the unload is the decision, and
// a shortage the unload does not cure costs the cards held (exclusive) and the seat cold, but only for the hold bound.
// Before the bound (2026-10-14) it cost them until --wait ended it: a media lease around a command that is not a render
// helper declared the class default (46.8 GiB), the seat was not even loaded, the unload freed nothing, and the lease held
// all three cards idle for about 50 minutes while a production lease queued behind it. The drain-and-unload-first order is
// unchanged (it breaks a real deadlock); only the time the cards may be held after it is bounded.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// hostRAMSplit says how a reserve's resolved host-RAM need is handled. gate is what the grant compares to the host (and
// declares); later is what is asked of the host after the seat has been cleared. defers is whether the lease's own
// unload can free host memory for it (--unload-seat, and the unload reaches a model: unloadReachesLease) and the host can
// be read (screenDeferredHostRAM); a lease that defers declares nothing in the grant, any other lease, and a need of 0,
// compares in the grant exactly as before.
func hostRAMSplit(defers bool, needGiB float64) (gate, later float64) {
	if defers && needGiB > 0 {
		return 0, needGiB
	}
	return needGiB, 0
}

// unloadReachesLease says whether the unload that --unload-seat asks for can take down anything on the cards a lease holds
// (lease ids; none = the whole node, or the cards an allocation has not chosen yet): the agent seat, or a model the roster
// lists that is placed on those cards or placed nowhere. It is the question maintainSeatScoped answers when it runs, put
// before the grant from the same facts (the card table, the layers' pins, the serving config, the roster), and it
// resolves doubt the way the unload does: a table that cannot be read, a model nothing places and a roster that cannot be
// read all count as reaching, so only a lease whose every listed model is positively placed on other cards is told that its
// unload frees nothing for it. A config without a seat target also answers true: the unload refuses at once, so nothing
// waits behind it. The facts are placements and the roster, which do not change while a lease queues; residency does, and is
// deliberately not asked (a seat that is cold when the reserve starts may be loaded when the grant comes).
//
// cards is the table the caller already read for the plan (nil = read it here).
func unloadReachesLease(ctx context.Context, cfg config.Config, ids []string, cards []gpuprobe.Card) bool {
	if len(ids) == 0 {
		return true
	}
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return true
	}
	deps := cardTableDeps()
	if len(cards) > 0 {
		deps.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return cards, "", nil }
	}
	if gpualloc.NewLeaseScope(ctx, cfg, ids, deps, nil).Touches(model) {
		return true
	}
	roster, rerr := rosterIDsFn(ctx, endpoint)
	models, known := gpualloc.UnloadModels(ctx, cfg, roster, rerr, ids, deps, nil)
	return !known || len(models) > 0
}

// screenDeferredHostRAM is the grant's own answer to a deferred need, given before anything is taken or unloaded. A need no
// state of the box can admit is refused at once (waiting cannot help, and a seat cycle for a lease that can never run is
// bought for nothing). A host whose memory cannot be read is not asked after the unload either: the look would refuse for
// the same reason until its deadline, with the seat cycled and the cards held, so the lease keeps the grant's comparison,
// which waits in the queue for a readable host as it did before F82. Anything else passes unchanged.
func screenDeferredHostRAM(m *gpulease.Manager, gate, later float64, wait time.Duration, out io.Writer) (float64, float64, error) {
	if later <= 0 {
		return gate, later, nil
	}
	chk := m.HostRAMCheck(later)
	switch {
	case !chk.OK && chk.Impossible:
		return gate, later, heldHint(&gpulease.ErrHostRAM{HostRAMCheck: chk}, wait)
	case !chk.OK && chk.Unreadable:
		fmt.Fprintf(out, "gpu reserve: the host's memory cannot be read, so nothing can show that the unload makes room for the %.1f GiB: it is compared in the grant, as for a lease that unloads nothing, and the seat is left alone until the host can be read\n", later)
		return later, 0, nil
	}
	return gate, later, nil
}

// hostRAMDeadline is how long the host-RAM wait after the unload may run: the rest of the queue budget (--wait,
// measured from when the reservation began queueing, the clock the drain uses), never under the drain floor. The
// floor is the same one the drain gets: the unload has just been done, so a caller that asked to fail fast on the lease
// (`--wait 0`) still gets a real window for the memory to come back, and does not pay a seat cycle for one reading
// taken a moment after the unload.
func hostRAMDeadline(queuedAt time.Time, wait time.Duration) time.Time {
	return drainDeadline(0, queuedAt, wait)
}

// awaitHostRAMHeld puts the host-RAM check of a lease that cleared the seat. await is the lease's
// AwaitHostRAM (a Lease or a Manager by epoch); ctx ends the wait early (a signal, a lost lease). It says once
// that the cards are held while the host is short, and when the wait ended. A timeout comes back as the refusal the grant
// would have given, with the hint that names the flag that waits longer.
func awaitHostRAMHeld(ctx context.Context, await func(gpulease.AwaitHostRAMOptions) error, deadline time.Time, wait time.Duration, out io.Writer) error {
	return awaitHostRAMBounded(ctx, await, deadline, wait, hostRAMHold{}, out)
}

// hostRAMHold is what bounds the wait after the unload and what the operator is told about it (the hold bound, the
// 2026-10-14 incident). max is the most the lease may hold its cards waiting for the host, counted from the unload; 0 = no
// bound, --wait alone ends the wait. heldSince is when the lease took its cards, for the failure's "held the cards for".
// freed, when it says the unload freed nothing, is printed in the waiting line: the wait cannot have been shortened by it.
// classDefault is whether the declared figure is the media class default, which decides what the failure advises.
type hostRAMHold struct {
	max          time.Duration
	heldSince    time.Time
	freed        unloadOutcome
	classDefault bool
}

// errHostRAMHoldBound is the failure of a wait that the hold bound ended: the lease is released and its command never ran.
// It wraps the host's last refusal (errors.As still finds the *gpulease.ErrHostRAM, whose text carries the need and the
// committed, physical and headroom figures), and it is deliberately not given heldHint's advice to pass a longer --wait:
// --wait did not end this wait.
type errHostRAMHoldBound struct {
	short        *gpulease.ErrHostRAM
	bound        time.Duration // the bound that was in force
	waited       time.Duration // how long the wait after the unload ran
	held         time.Duration // how long the lease held its cards in all
	freedWhy     string        // non-empty when the unload freed nothing: why
	failedText   string        // non-empty when unloads of other residents failed: what to say instead of claiming nothing was there
	classDefault bool
}

func (e *errHostRAMHoldBound) Unwrap() error { return e.short }

// Error is one line: what ended the wait, the need and the host's figures (the refusal's own words), how long the cards were
// held, what happened to the lease, and what to do about it.
func (e *errHostRAMHoldBound) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the host RAM hold bound passed (%s): the lease held the cards for %s (%s of it waiting for the host after the unload) and the host never had the room: %s; the lease is released and nothing ran under it.",
		roundHold(e.bound), roundHold(e.held), roundHold(e.waited), e.short.Error())
	if e.freedWhy != "" {
		fmt.Fprintf(&b, " The unload freed nothing on these cards (%s), so the shortage is not the seat's.", e.freedWhy)
	}
	if e.failedText != "" {
		fmt.Fprintf(&b, " Note: %s.", e.failedText)
	}
	need := e.short.NeedGiB
	if e.classDefault {
		fmt.Fprintf(&b, " The declared %.1f GiB is the media class default, not a measure of this command: pass --ram <GiB> with the real figure (--ram 0 when the weights sit on the cards).", need)
	} else {
		fmt.Fprintf(&b, " If %.1f GiB is more than the command loads, pass a smaller --ram <GiB>; otherwise wait for the host to free memory and reserve again.", need)
	}
	b.WriteString(" To hold longer: --ram-hold-max <duration>, or config gpu_hostram_hold_max_sec (0 = no bound, --wait alone ends the wait).")
	return b.String()
}

// roundHold prints a hold or a bound the way an operator reads it: whole seconds from ten seconds up, tenths below.
func roundHold(d time.Duration) string {
	if d >= 10*time.Second {
		return d.Round(time.Second).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// awaitHostRAMBounded is awaitHostRAMHeld with the hold bound: the wait ends at the earlier of the --wait deadline and the
// bound (counted from now, the moment the unload is done, and not under the drain floor: the floor exists for the memory the
// unload frees to come back, and a bound the operator set is an answer to that). When the bound is the earlier one and it
// passes without the host's admission, the failure is errHostRAMHoldBound; when --wait ends first, or there is no bound, it
// is the refusal the grant would have given (heldHint), exactly as before the bound.
func awaitHostRAMBounded(ctx context.Context, await func(gpulease.AwaitHostRAMOptions) error, deadline time.Time, wait time.Duration, h hostRAMHold, out io.Writer) error {
	start := time.Now()
	effective, boundAt := deadline, time.Time{}
	if h.max > 0 {
		if b := start.Add(h.max); b.Before(deadline) {
			effective, boundAt = b, b
		}
	}
	told := false
	err := await(gpulease.AwaitHostRAMOptions{
		Deadline:  effective,
		HoldUntil: boundAt,
		Cancel:    ctx.Done(),
		OnWait: func(e *gpulease.ErrHostRAM) {
			told = true
			freed := ""
			switch {
			case h.freed.freedNothing():
				freed = fmt.Sprintf(" (the unload freed nothing on these cards: %s, so it did not shorten this wait)", h.freed.why)
			case h.freed.failed > 0:
				freed = " (" + h.freed.failedUnloadsText() + ")"
			}
			bound := ""
			if !boundAt.IsZero() {
				bound = fmt.Sprintf("; if the host still lacks the room then, the lease is released and nothing runs under it (hold bound %s: --ram-hold-max, config gpu_hostram_hold_max_sec)", roundHold(h.max))
			}
			fmt.Fprintf(out, "gpu reserve: %s — the drain and the unload are done and the cards are held%s; waiting up to %s for the host to have the room%s\n",
				e.Error(), freed, time.Until(effective).Round(time.Second), bound)
		},
	})
	if err != nil {
		var short *gpulease.ErrHostRAM
		if !boundAt.IsZero() && errors.As(err, &short) && !short.Impossible {
			e := &errHostRAMHoldBound{short: short, bound: h.max, waited: time.Since(start), held: time.Since(h.heldSince), classDefault: h.classDefault}
			if h.freed.freedNothing() {
				e.freedWhy = h.freed.why
			}
			e.failedText = h.freed.failedUnloadsText()
			return e
		}
		return heldHint(err, wait)
	}
	if told {
		fmt.Fprintf(out, "gpu reserve: the host has the room: admitted after %s of waiting for the memory\n", time.Since(start).Round(time.Second))
	}
	return nil
}

// detachedHostRAMError is detachedMaintainError for a detached holder whose host-RAM wait failed (the host never had
// the room inside --wait, or the wait was cancelled). The lease is kept, as after a failed drain: the cards stay
// reserved and work keeps routing elsewhere. What a failed step must not keep is the stamp it left on the record: the
// exclusive stamp is put on once the drain succeeded, before this wait, and left on it cordons the text lane on those
// cards (and fences the seat that is now unloaded) for the rest of --for, which is how one failed drain became a box
// refused for 8 hours (register C-50, S-31; the drain path clears DRAINING for the same reason). So it is taken off, and
// the way out names the release that also warms the seat back, because the seat is cold.
func detachedHostRAMError(m *gpulease.Manager, epoch uint64, err error) error {
	if info := m.Inspect(); info.Held && info.HoldsEpoch(epoch) {
		if serr := m.Restamp(epoch, func(mt *gpulease.Meta) { mt.Exclusive = false }); serr != nil {
			fmt.Fprintf(os.Stderr, "gpu reserve: could not take the exclusive stamp off the lease after the failed wait for host RAM: %v\n", serr)
		}
		return fmt.Errorf("%w (the lease is still held and no longer exclusive; the seat is unloaded: `gpu release --warm-seat` frees the lease and warms the seat back, `gpu release` frees it and leaves the warm owed)", err)
	}
	return detachedMaintainError(m, epoch, err)
}

// releaseAfterHoldBound ends a detached hold whose host-RAM wait the hold bound ended: it does what `gpu release --warm-seat
// --epoch N` does (the instances kept under the lease are stopped, the seat the unload took down is warmed back under the
// release guards, the lease is released) and returns the bound's failure so the reserve exits non-zero. A detached hold is the
// form that can sit on the cards longest (a hidden holder renews its term while its owner vouches for it), and the incident this
// bound answers was cards held idle behind a wait that could not end; keeping the lease "so the operator can look" is what
// failed there. A wait that --wait ended first keeps the lease (detachedHostRAMError), as it always did.
func releaseAfterHoldBound(m *gpulease.Manager, epoch uint64, cfg config.Config, err error) error {
	if info := m.Inspect(); !info.Held || !info.HoldsEpoch(epoch) {
		return detachedMaintainError(m, epoch, err) // the lease is not this reservation's any more: nothing to release
	}
	stopKeptInstances(cfg, epoch, os.Stderr)
	warmBackGuarded(cfg, releaseWarmGuard(m, epoch), os.Stderr)
	if _, rerr := m.ReleaseByEpoch(epoch); rerr != nil {
		return fmt.Errorf("%w (and the lease could not be released: %v; `gpu release --warm-seat` frees it)", err, rerr)
	}
	return err
}

// hostRAMAskedStatus is what `gpu status` says about a lease that holds its cards while the host has not admitted the RAM it
// asked for (F82): the figure, how long the cards have been held, and, when the wait for the host runs under the hold bound
// (the record carries the moment it ends, until), when the bound releases the lease. Without a stamp the wait has not begun
// (the drain and the unload come first) or runs with no bound (--ram-hold-max 0): the sentence says only what is known.
// long adds the explanation of the order, for the one-line summary of the holder.
func hostRAMAskedStatus(giB float64, held time.Duration, until, now time.Time, long bool) string {
	s := fmt.Sprintf("host RAM %.1f GiB asked, not yet admitted", giB)
	heldFor := held.Round(time.Second)
	switch {
	case until.IsZero():
		if long {
			s += ": the seat is cleared first, then the host is asked"
		}
		return s + fmt.Sprintf("; held the cards for %s", heldFor)
	case until.After(now):
		return s + fmt.Sprintf(": held the cards for %s; the hold bound releases the lease at %s, in %s", heldFor, until.Format(time.Kitchen), until.Sub(now).Round(time.Second))
	default:
		return s + fmt.Sprintf(": held the cards for %s; the hold bound passed at %s, and the reserve that holds the lease releases it at its next look (`gpu release --warm-seat` frees it now)", heldFor, until.Format(time.Kitchen))
	}
}
