package main

// F82: where a lease that unloads the agent seat is compared to the host.
//
// `gpu reserve --ram N --drain --unload-seat` used to be compared to the host inside Acquire, and the drain and the
// unload ran only after the lease was held. The memory it waited for was held by the seat its own unload frees: a
// film lease asked for 60 GiB while the 3-card seat held 67 GiB of the host's commit, and it waited about six hours
// for a grant it could only get by being granted (the same class as the DRAINING stamp, gpu_drain.go, 0.117.0: the
// gate in front of the step that clears it).
//
// The order now, for a lease that asks for --unload-seat and declares host RAM:
//
//  1. the need is refused at once when no state of the box could ever admit it (waiting cannot help, and a seat
//     cycle for a lease that can never run is bought for nothing);
//  2. the grant takes the place in line and the cards, declaring nothing (Options.HostRAMWaitGiB carries the
//     figure; the allocator's host term is off for the same reason), so the cards are fenced and no seat reloads
//     onto them;
//  3. the drain and the unload run, as they always did, under the lease;
//  4. only then Manager.AwaitHostRAM puts the one admission rule to the figure, still holding the cards, with the
//     same words, inside the same --wait, and the figure becomes the lease's declared need in the same critical
//     section that admitted it;
//  5. the command starts.
//
// A lease without --unload-seat is untouched: --drain alone frees no host memory, so a lease that only drains
// keeps the comparison in the grant (cordoning the seat for a wait the drain cannot shorten would only cost the
// seat's callers).

import (
	"context"
	"fmt"
	"io"
	"os"
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
	start := time.Now()
	told := false
	err := await(gpulease.AwaitHostRAMOptions{
		Deadline: deadline,
		Cancel:   ctx.Done(),
		OnWait: func(e *gpulease.ErrHostRAM) {
			told = true
			fmt.Fprintf(out, "gpu reserve: %s — the drain and the unload are done and the cards are held; waiting up to %s for the host to have the room\n",
				e.Error(), time.Until(deadline).Round(time.Second))
		},
	})
	if err != nil {
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
