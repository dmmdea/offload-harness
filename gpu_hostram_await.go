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

// hostRAMLoadSplit is hostRAMSplit for a declaration of both quantities: they move together, to the grant or to the look after
// the unload. A declaration that declares nothing is compared in the grant, as it always was.
func hostRAMLoadSplit(defers bool, load gpuprobe.HostLoad) (gate, later gpuprobe.HostLoad) {
	if defers && load.Declares() {
		return gpuprobe.HostLoad{}, load
	}
	return load, gpuprobe.HostLoad{}
}

// unloadReachesLease says whether the unload that --unload-seat asks for can take down anything on the cards a lease holds
// (lease ids; none = the whole node, or the cards an allocation has not chosen yet): the agent seat, or a model the roster
// lists that is placed on those cards or placed nowhere. It is the question maintainSeat answers when it runs, put
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
func screenDeferredHostRAM(m *gpulease.Manager, gate, later gpuprobe.HostLoad, wait time.Duration, out io.Writer) (gpuprobe.HostLoad, gpuprobe.HostLoad, error) {
	if !later.Declares() {
		return gate, later, nil
	}
	chk := m.HostRAMCheckLoad(later)
	switch {
	case !chk.OK && chk.Impossible:
		return gate, later, heldHint(&gpulease.ErrHostRAM{HostRAMCheck: chk}, wait)
	case !chk.OK && chk.Unreadable:
		fmt.Fprintf(out, "gpu reserve: the host's memory cannot be read, so nothing can show that the unload makes room for the %.1f GiB: it is compared in the grant, as for a lease that unloads nothing, and the seat is left alone until the host can be read\n", max(later.TouchGiB, later.CommitGiB))
		return later, gpuprobe.HostLoad{}, nil
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
	// failClosed: a stamp that cannot be written ends the wait with an error (the detached form, whose hidden holder is the only
	// thing that releases the lease if this process dies). outer is the outer bound already on the record, zero if none.
	failClosed bool
	outer      time.Time
}

// errHostRAMHoldBound is the failure of a wait that the hold bound ended: the lease is released and its command never ran.
// It wraps the host's last refusal (errors.As still finds the *gpulease.ErrHostRAM, whose text carries the need, the
// figures of the condition that refused and the headroom), and it is deliberately not given heldHint's advice to pass a longer --wait:
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
	fmt.Fprintf(&b, "the host RAM hold bound passed (%s): the lease held the cards for %s (%s of it waiting for the host after the unload) and the host never had the room: %s; nothing ran under it and the lease is being released.",
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
	} else if e.short.CommitShort && !e.short.TouchShort && e.short.CommitStated {
		fmt.Fprintf(&b, " If %.1f GiB of commit is more than the command is charged, pass a smaller --ram-commit <GiB>; otherwise wait for the host to free commit and reserve again.", e.short.CommitNeedGiB)
	} else if e.short.CommitShort && !e.short.TouchShort {
		fmt.Fprintf(&b, " If %.1f GiB is more than the command is charged, pass a smaller --ram <GiB> (no commit was stated, so the --ram figure is held to physical RAM, or to the commit limit where an enforced one sits below it; --ram-commit <GiB> states a measured one); otherwise wait for the host to free commit and reserve again.", e.short.CommitNeedGiB)
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

// awaitHostRAMBounded puts the host-RAM check of a lease that cleared the seat, still holding the cards. await is the
// lease's AwaitHostRAM (a Lease or a Manager by epoch); ctx ends the wait early (a signal, a lost lease). It says once that the
// cards are held while the host is short, and when the wait ended. With a zero hostRAMHold it is the plain wait inside --wait;
// with a bound the wait ends at the earlier of the --wait deadline and the
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
	saidStamp := false
	err := await(gpulease.AwaitHostRAMOptions{
		Deadline: effective,
		// The stamped end of the wait is whichever of the bound and --wait comes first: the lease is released then, by this
		// process or, if it died, by the lease's hidden holder.
		HoldUntil: effective,
		// The detached form requires its stamp: its hidden holder is the only thing that releases the lease if this process dies.
		RequireStamp: h.failClosed,
		Cancel:       ctx.Done(),
		// A stamp that cannot be written is said once however often the library reports it (it retries at every look): it costs
		// the status line and the hidden holder's own enforcement of the bound, never the wait.
		OnStampError: func(serr error) {
			if !saidStamp {
				saidStamp = true
				fmt.Fprintf(out, "gpu reserve: warning: could not stamp the hold bound on the lease record (%v); the bound still ends this wait, but `gpu status` cannot show when the lease is released and the hidden holder of a --detach lease cannot enforce the bound if this process dies\n", serr)
			}
		},
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
		var stampErr *gpulease.ErrHoldStamp
		if errors.As(err, &stampErr) {
			return holdStampError(stampErr, h.outer)
		}
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

// releaseByEpochFn is Manager.ReleaseByEpoch. A variable so a test can make it answer "nothing was held" for a lease that is.
var releaseByEpochFn = func(m *gpulease.Manager, epoch uint64) (bool, error) { return m.ReleaseByEpoch(epoch) }

// holdStampError is the failure of a detached wait whose stamp could not be written: the hold bound the hidden holder would enforce
// if this process died is not on the record, so the wait does not run. An outer bound stamped before the drain, if it landed, stays
// (nothing clears it): it is still a moment by which the holder releases the lease. The lease is released by the caller, and the
// text says only that it is being released; the outcome is reported after it happens.
func holdStampError(e *gpulease.ErrHoldStamp, outer time.Time) error {
	kept := "no bound is on the record"
	if !outer.IsZero() {
		kept = fmt.Sprintf("the outer bound stamped before the drain (%s) stays on the record, so the holder still releases the lease then", outer.Format(time.Kitchen))
	}
	return &errHoldStampDetached{cause: e, text: fmt.Sprintf("the hold bound could not be written on the lease record after %d tries (%v): a detached hold whose end the hidden holder cannot see would be held for good if this process died, so the wait is not run and the lease is being released; %s", e.Tries, e.Err, kept)}
}

// errHoldStampDetached is holdStampError's error: the sentence for the operator, with the library's *ErrHoldStamp still reachable.
type errHoldStampDetached struct {
	cause *gpulease.ErrHoldStamp
	text  string
}

func (e *errHoldStampDetached) Error() string { return e.text }
func (e *errHoldStampDetached) Unwrap() error { return e.cause }

// leaseNotReleasedLine is the one line printed when a release the harness just tried did not happen. The failure text of a
// bound or a wait is built BEFORE the release runs and so says only that the lease is being released; this line is the report of
// the outcome, and the way out.
func leaseNotReleasedLine(prefix string, epoch uint64, err error, warmOwed bool) string {
	return fmt.Sprintf("%s: LEASE NOT RELEASED (epoch %d): %v; run %s", prefix, epoch, err, releaseCommand(epoch, warmOwed))
}

// releaseCommand is the command that ends lease `epoch` by hand. --warm-seat is part of it only when a warm-back is owed (the
// marker an unload leaves): releaseWarmGuard has no onlyIfOwed, so advising it for a lease that unloaded nothing could load a
// seat that nothing asked for.
func releaseCommand(epoch uint64, warmOwed bool) string {
	if warmOwed {
		return fmt.Sprintf("gpu release --warm-seat --epoch %d", epoch)
	}
	return fmt.Sprintf("gpu release --epoch %d", epoch)
}

// leaseStillHeld is true when the lease with this epoch is on the record now, and also when its record exists and cannot be read:
// that lease is held for every decision here. It looks at the record itself (Manager.RecordState) and not through Inspect(), which
// reads a record it cannot read as absent.
func leaseStillHeld(m *gpulease.Manager, epoch uint64) bool {
	st, _ := m.RecordState(epoch)
	return st != gpulease.RecordAbsent
}

// stillHeldError says why the lease with this epoch is still on the record after a release that claimed otherwise, or nil when it
// is not.
func stillHeldError(m *gpulease.Manager, epoch uint64, claim string) error {
	switch st, cause := m.RecordState(epoch); st {
	case gpulease.RecordHeld:
		return fmt.Errorf("%s, but epoch %d is still held", claim, epoch)
	case gpulease.RecordUnreadable:
		return fmt.Errorf("%s, but the record of epoch %d cannot be read, so the lease is treated as still held: %w", claim, epoch, cause)
	}
	return nil
}

// confirmReleased turns the answer of a release into an error when the answer cannot be believed. ReleaseByEpoch says (false,
// nil), "no lease was held", for a lease that is gone and, when a record could not be examined, for one that is not: a caller
// that took that for "released" printed nothing and left the cards held. So a release that did nothing is believed only if the
// epoch is no longer on the record.
func confirmReleased(m *gpulease.Manager, epoch uint64, released bool, err error) error {
	if err != nil {
		return err
	}
	if !released {
		return stillHeldError(m, epoch, "the release reported that no lease was held")
	}
	return nil
}

// releaseConfirmed ends lease `epoch` by its number and confirms it is gone.
func releaseConfirmed(m *gpulease.Manager, epoch uint64) error {
	released, err := releaseByEpochFn(m, epoch)
	return confirmReleased(m, epoch, released, err)
}

// releaseLeaseConfirmed releases a lease object and confirms it is gone: Lease.Release answers nil for a record it could not
// read ("already gone"), which is a claim about a lease nobody looked at.
func releaseLeaseConfirmed(m *gpulease.Manager, lease *gpulease.Lease) error {
	if err := lease.Release(); err != nil {
		return err
	}
	return stillHeldError(m, lease.Epoch(), "the release reported success")
}

// releaseFailedError is what a command returns when the release it began failed: the cause it was already returning, then the
// truth. The cause was built BEFORE the release ran and says the lease is being released; this is the last word.
func releaseFailedError(cause error, epoch uint64, rerr error) error {
	return fmt.Errorf("%w; the release then failed and the lease is STILL HELD (epoch %d): %v", cause, epoch, rerr)
}

// autoReleaseWarmGuard is releaseWarmGuard for a release the harness makes on its own, not one an operator asked for with
// `gpu release --warm-seat`: the seat is warmed back only if the lease OWES it (the marker an unload leaves), exactly as the
// wrapper form's guard does. Without it a seat that was never loaded (the incident of 2026-10-14) is loaded by the release of a
// lease that never unloaded it, the 2026-09-23 defect: a seat came up on three cards with nothing asking for it.
func autoReleaseWarmGuard(m *gpulease.Manager, epoch uint64) warmGuard {
	g := releaseWarmGuard(m, epoch)
	g.onlyIfOwed = true
	return g
}

// detachedReleaseGrace is how far past now the parent of a detached hold pushes the stamped end of the wait when it starts to
// release the lease itself (warming the seat back takes minutes): until the release is done the hidden holder sees a moment
// still ahead and leaves the lease to its parent, and a parent that dies mid-release (a tool timeout lands at exactly the
// default bound) leaves a moment the holder acts on. A variable so a test can watch it pass.
var detachedReleaseGrace = 15 * time.Minute

// pushedOutHook, when set, is called by releaseAfterRAMWait the moment the stamped end of the wait is pushed out, before the slow part
// of the release begins. A test seam: a test that pits the hidden holder against a live reserve holds the reserve here until it has
// decided what the holder may see, instead of polling for a window that a starved test goroutine can miss. Nil in production.
var pushedOutHook func(epoch uint64, until time.Time)

// releaseAfterRAMWait ends a detached hold whose host-RAM wait ended without admission, WHATEVER ended it (the hold bound,
// --wait, an impossible figure): the cards are never held idle behind a RAM figure the host cannot meet. It does what `gpu
// release --warm-seat --epoch N` does, except that the seat is warmed back only if the lease owes it (autoReleaseWarmGuard): the
// instances kept under the lease are stopped, the seat the unload took down is warmed back under the release guards, the lease
// is released. err is what ended the wait; it is returned (so the reserve exits non-zero) with the fact that the lease is being
// released, and the OUTCOME of the release is reported after it happens: one stderr line when it did not.
//
// Before the slow part it pushes the stamped end of the wait out by detachedReleaseGrace. The hidden holder releases a lease
// whose stamped end has passed and which still asks for the host; this reserve is releasing it itself and must not be raced by
// the holder for the minutes a warm-back takes, yet if it dies in the middle the lease must not stay held for good: the pushed
// stamp is the moment the holder takes over. The lease being gone already (the holder got there first) is not a failure.
func releaseAfterRAMWait(m *gpulease.Manager, epoch uint64, cfg config.Config, err error) error {
	if !leaseStillHeld(m, epoch) {
		return detachedMaintainError(m, epoch, err) // the lease is not this reservation's any more: nothing to release
	}
	// Else it is on the record, or its record exists and cannot be read, which is a lease that is held: proceed to stop the
	// instances, warm the seat back and release, and report the release if it fails.
	pushedTo := time.Now().Add(detachedReleaseGrace)
	if serr := m.StampHostRAMHoldPhase(epoch, pushedTo, gpulease.HoldPhaseRelease); serr == nil {
		if pushedOutHook != nil {
			pushedOutHook(epoch, pushedTo)
		}
	} else {
		fmt.Fprintf(os.Stderr, "gpu reserve: warning: could not push the lease's stamped end out before releasing it (%v); if this process dies during the warm-back the hidden holder may release the lease sooner than the warm-back finishes\n", serr)
	}
	stopKeptInstances(cfg, epoch, os.Stderr)
	warmBackGuarded(cfg, autoReleaseWarmGuard(m, epoch), os.Stderr)
	if rerr := releaseConfirmed(m, epoch); rerr != nil {
		fmt.Fprintln(os.Stderr, leaseNotReleasedLine("gpu reserve", epoch, rerr, m.SeatWarmOwed() != ""))
		return releaseFailedError(err, epoch, rerr)
	}
	var bound *errHostRAMHoldBound
	if errors.As(err, &bound) {
		return err // it already says that the lease is being released
	}
	return fmt.Errorf("%w; the lease is being released: cards are never held idle behind a host-RAM figure the host did not meet", err)
}

// holderGaveUpNote is what `gpu status` adds to a lease whose hidden holder gave up releasing it: nothing polls the lease now, so
// the text must not promise a release "at its next poll".
func holderGaveUpNote(gaveUp time.Time, why string, epoch uint64, warmOwed bool) string {
	if gaveUp.IsZero() {
		return ""
	}
	reason := ""
	if why != "" {
		reason = " (" + why + ")"
	}
	return fmt.Sprintf("the holder gave up releasing it at %s%s and nothing polls the lease now; `%s` frees it", gaveUp.Format(time.Kitchen), reason, releaseCommand(epoch, warmOwed))
}

// hostRAMAskedStatus is what `gpu status` says about a lease that holds its cards while the host has not admitted the RAM it
// asked for (F82): the figure, how long the cards have been held, and, once the wait for the host has begun, when the lease is
// released if the host still lacks the room (the record carries that moment, until: the earlier of the hold bound and --wait).
// Without a stamp the wait has not begun (the drain and the unload come first) or has no end the record knows: the sentence
// says only what is known. Past the stamped moment the lease is still there, so whoever releases it has not yet: a wrapper
// does it when it exits; the hidden holder of a --detach lease does it at its next poll IF one is running (a reserve that died
// cannot, and a holder whose own release failed has gone), and `gpu release --epoch N` does it now (with --warm-seat when a
// warm-back is owed). long adds the explanation of the order, for the one-line summary of the holder.
// The stamp is one of three moments (phase, the record's Meta.HostRAMHoldPhase): the OUTER bound a live detached reserve keeps moving
// while it clears the seat (the host has not been asked, nothing releases the lease unless the reserve dies), the end of the WAIT
// for the host (the sentence above), or the RELEASE push-out of a reserve that is letting the lease go. Without a phase it is the wait's.
func hostRAMAskedStatus(giB, commitGiB float64, held time.Duration, until time.Time, phase string, now time.Time, long bool, epoch uint64, byWrapper, warmOwed bool, gaveUp time.Time, gaveUpWhy string) string {
	release := releaseCommand(epoch, warmOwed)
	s := fmt.Sprintf("host RAM %.1f GiB asked, not yet admitted", giB)
	if commitGiB > 0 && commitGiB != giB {
		s = fmt.Sprintf("host RAM %.1f GiB touched, %.1f GiB commit asked, not yet admitted", giB, commitGiB)
	}
	heldFor := held.Round(time.Second)
	switch {
	case !gaveUp.IsZero():
		return s + fmt.Sprintf(": held the cards for %s; %s", heldFor, holderGaveUpNote(gaveUp, gaveUpWhy, epoch, warmOwed))
	case until.IsZero():
		if long {
			s += ": the seat is cleared first, then the host is asked"
		}
		return s + fmt.Sprintf("; held the cards for %s", heldFor)
	case phase == gpulease.HoldPhaseOuter && until.After(now):
		// The detached reserve is clearing the seat (the drain, the unload) and keeps this moment ahead while it lives: the host has
		// not been asked, and a live reserve never lets the lease go now. The holder releases it only if the reserve dies.
		return s + fmt.Sprintf(": held the cards for %s; the seat is cleared first, then the host is asked; if the reserve dies the holder releases about %s", heldFor, until.Format(time.Kitchen))
	case phase == gpulease.HoldPhaseOuter:
		return s + fmt.Sprintf(": held the cards for %s; the reserve that was clearing the seat has stopped moving its bound (it passed at %s), so it is gone or stalled: the holder, if one is running, releases the lease at its next poll; `%s` frees it now", heldFor, until.Format(time.Kitchen), release)
	case phase == gpulease.HoldPhaseRelease && until.After(now):
		return s + fmt.Sprintf(": held the cards for %s; the reserve is releasing the lease; if it dies the holder releases about %s", heldFor, until.Format(time.Kitchen))
	case until.After(now):
		return s + fmt.Sprintf(": held the cards for %s; the lease is released at %s, in %s, if the host still lacks the room (the hold bound, or --wait if that ends first)", heldFor, until.Format(time.Kitchen), until.Sub(now).Round(time.Second))
	case byWrapper:
		return s + fmt.Sprintf(": held the cards for %s; the wait ended at %s and the wrapper that holds the lease releases it when it exits (`%s` frees it now if that process is gone)", heldFor, until.Format(time.Kitchen), release)
	default:
		return s + fmt.Sprintf(": held the cards for %s; the wait ended at %s: the holder, if one is running, releases the lease at its next poll; `%s` frees it now", heldFor, until.Format(time.Kitchen), release)
	}
}
