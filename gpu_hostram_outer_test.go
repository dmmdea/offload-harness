package main

// Review round 3, items A and E.
//
// A: a detached reserve killed during the drain or the unload, before the host-RAM wait begins, left the lease asking for the host
// with no stamp, and the hidden holder, which acts only on a stamp, kept the cards until --for ended. The reserve now stamps an
// OUTER bound as soon as the holder has the lease: the drain's own deadline plus the hold bound, both known at that point. The
// wait replaces it with its real end when it begins.
//
// E: in the detached form a stamp that cannot be written costs the holder its bound, so the wait fails closed (it requires the
// stamp, tries a few times, and ends the wait with an error and a release); an outer bound already on the record stays.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// firstStamp returns the first lease on the record that carries a stamped end of the wait, polling like another process.
func firstStamp(t *testing.T, m *gpulease.Manager) gpulease.Info {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range m.Leases() {
			if !l.HostRAMHoldUntil.IsZero() {
				return l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no stamped end of the wait ever appeared on the record")
	return gpulease.Info{}
}

const detachedFilmDraining = "--detach --for 1h --wait 20s --drain --unload-seat --ram 30 --reason film"

func TestADetachedReserveStampsAnOuterBoundBeforeTheDrainSoAParentThatDiesThereIsStillCovered(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1) // a request is in flight: the drain waits, and the reserve is "dying" in it
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	fastHolder(t)
	advance := heldHolderClock(t)
	holderDone := detachWithRealHolder(t)

	started := time.Now()
	args := append([]string{"--config", cfgPath, "--ram-hold-max", "5m"}, strings.Fields(detachedFilmDraining)...)
	reserveDone := make(chan error, 1)
	go func() { reserveDone <- runGPUReserve(args) }()
	t.Cleanup(func() {
		f.inflight.Store(0)
		releaseAll(t, m)
		select {
		case <-reserveDone:
		case <-time.After(40 * time.Second):
		}
	})

	l := firstStamp(t, m)
	want := started.Add(5*time.Minute + outerSlack) // now plus the hold bound plus the slack (the drain's own deadline is --wait away)
	if d := l.HostRAMHoldUntil.Sub(want); d < -time.Second || d > 30*time.Second {
		t.Errorf("the outer bound is now plus the hold bound plus the slack (%s), got %s", want, l.HostRAMHoldUntil)
	}
	if l.HostRAMWaitGiB != 30 {
		t.Errorf("the lease still asks for its figure: %.1f", l.HostRAMWaitGiB)
	}

	// The reserve never comes back from the drain (it is dead, for this purpose). The holder sees the outer moment pass.
	advance(l.HostRAMHoldUntil.Add(holdBoundGrace + time.Second))
	holderEnds(t, holderDone, 20*time.Second)
	if m.Inspect().Held {
		t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
	}
	if n := f.warms.Load(); n != 0 {
		t.Errorf("the seat was never unloaded for this lease, so nothing is warmed: warms=%d", n)
	}
}

func TestNoOuterBoundIsStampedWhenThereIsNoBoundOrNothingWasDeferred(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		film  string
	}{
		{"the hold bound is off", []string{"--ram-hold-max", "0"}, detachedFilmDraining},
		{"nothing is deferred: no --unload-seat", nil, "--detach --for 1h --wait 20s --drain --ram 30 --reason film"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			f.inflight.Store(1)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 50, 40) // the grant admits 30 GiB here, so a lease without --unload-seat is granted
			shortFloor(t, time.Second)
			fastHolder(t)
			_ = heldHolderClock(t)
			holderDone := detachWithRealHolder(t)
			args := append(append([]string{"--config", cfgPath}, tc.extra...), strings.Fields(tc.film)...)
			reserveDone := make(chan error, 1)
			go func() { reserveDone <- runGPUReserve(args) }()
			t.Cleanup(func() {
				f.inflight.Store(0)
				releaseAll(t, m)
				select {
				case <-reserveDone:
				case <-time.After(40 * time.Second):
				}
				select {
				case <-holderDone:
				case <-time.After(10 * time.Second):
				}
			})
			waitForLeases(t, m, 1)
			time.Sleep(800 * time.Millisecond) // well past the moment a stamp would be written
			for _, l := range m.Leases() {
				if !l.HostRAMHoldUntil.IsZero() {
					t.Errorf("no outer bound applies here, yet one is stamped: %v", l.HostRAMHoldUntil)
				}
			}
		})
	}
}

// E: the wait of a detached reserve requires its stamp; the wrapper's does not (the wrapper is its own holder).
func TestTheDetachedWaitRequiresItsStampAndTheWrapperDoesNot(t *testing.T) {
	for _, failClosed := range []bool{true, false} {
		var got gpulease.AwaitHostRAMOptions
		await := func(o gpulease.AwaitHostRAMOptions) error { got = o; return nil }
		hold := hostRAMHold{max: time.Minute, heldSince: time.Now(), failClosed: failClosed}
		if err := awaitHostRAMBounded(context.Background(), await, time.Now().Add(time.Hour), time.Hour, hold, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
		if got.RequireStamp != failClosed {
			t.Errorf("failClosed=%v: the wait is asked to require its stamp = %v", failClosed, got.RequireStamp)
		}
	}
}

func TestAStampThatCannotBeWrittenEndsTheWaitWithAnErrorThatNamesTheOuterBoundIfItStays(t *testing.T) {
	cause := &gpulease.ErrHoldStamp{Epoch: 3, Tries: 4, Err: errors.New("disk full")}
	outer := time.Date(2026, 10, 14, 15, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		name  string
		outer time.Time
		want  []string
	}{
		{"an outer bound is on the record", outer, []string{"disk full", "4 tries", "outer bound", outer.Format(time.Kitchen), "stays on the record"}},
		{"no bound is on the record", time.Time{}, []string{"disk full", "4 tries", "no bound is on the record"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			await := func(gpulease.AwaitHostRAMOptions) error { return cause }
			hold := hostRAMHold{max: time.Minute, heldSince: time.Now(), failClosed: true, outer: tc.outer}
			err := awaitHostRAMBounded(context.Background(), await, time.Now().Add(time.Hour), time.Hour, hold, &strings.Builder{})
			if err == nil {
				t.Fatal("a stamp that cannot be written fails the detached wait")
			}
			var stampErr *gpulease.ErrHoldStamp
			if !errors.As(err, &stampErr) {
				t.Errorf("the cause stays reachable: %v", err)
			}
			for _, want := range append(tc.want, "being released") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error must say %q: %v", want, err)
				}
			}
		})
	}
}

// End to end: every stamp of the wait fails. The detached reserve ends the wait, releases the lease, and says why; the host would
// not have admitted the figure either way, and it was never looked at.
func TestADetachedReserveWhoseStampCannotBeWrittenFailsClosedAndReleases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outerLands bool
		wantInErr  string
	}{
		{"nothing could be stamped", false, "no bound is on the record"},
		{"the outer bound landed first", true, "stays on the record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 70)
			shortFloor(t, time.Second)
			detachWithBareLease(t)
			calls := 0
			restore := gpulease.UseStampFault(func(uint64) error {
				calls++
				if tc.outerLands && calls == 1 {
					return nil
				}
				return errors.New("disk full")
			})
			t.Cleanup(func() { restore(); releaseAll(t, m) })
			args := append([]string{"--config", cfgPath, "--ram-hold-max", "5m"}, strings.Fields(detachedFilm)...)
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("the wait failed closed on the stamp and says so (%q): %v\n%s", tc.wantInErr, err, stderr)
			}
			if m.Inspect().Held {
				t.Errorf("a detached hold whose bound the holder cannot see is released, not kept: %+v", m.Inspect())
			}
			if n := strings.Count(stderr, "could not stamp the hold bound"); n != 1 {
				t.Errorf("the failed stamp is said once, got %d:\n%s", n, stderr)
			}
			if tc.outerLands && calls < 2 {
				t.Errorf("the wait must have tried its own stamp after the outer one: %d stamp attempt(s)", calls)
			}
		})
	}
}

// Round 4, item 5. The outer bound was drainDeadline + holdMax with drainDeadline = queuedAt + --wait (8 h by default): a default
// detached reserve killed a minute into its drain left the holder holding the cards until about --wait + the bound, the incident's
// own command shape. The LIVE parent now re-stamps now + holdMax + slack every few seconds while the drain and the unload run, so a
// dead parent is covered within about the bound plus the slack, whatever --wait says.
func TestADetachedReserveWhoseParentDiesDuringALongWaitDrainIsReleasedWithinTheBoundNotAtWait(t *testing.T) {
	for _, dead := range []bool{false, true} {
		name := "a live parent keeps the bound moving and the holder leaves the lease alone"
		if dead {
			name = "a dead parent leaves a moment within the bound and the holder releases at it"
		}
		t.Run(name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			f.inflight.Store(1) // a request is in flight: the drain waits
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 70)
			shortFloor(t, time.Second)
			fastHolder(t)
			oldE, oldS := outerRefreshEvery, outerSlack
			outerRefreshEvery, outerSlack = 50*time.Millisecond, 500*time.Millisecond
			t.Cleanup(func() { outerRefreshEvery, outerSlack = oldE, oldS })
			holderDone := detachWithRealHolder(t)

			started := time.Now()
			// --wait 8h, the default: the drain's own deadline is eight hours away.
			args := append([]string{"--config", cfgPath, "--ram-hold-max", "1s"}, strings.Fields("--detach --for 1h --wait 8h --drain --unload-seat --ram 30 --reason film")...)
			reserveDone := make(chan error, 1)
			go func() { reserveDone <- runGPUReserve(args) }()
			t.Cleanup(func() {
				f.inflight.Store(0)
				releaseAll(t, m)
				select {
				case <-reserveDone:
				case <-time.After(40 * time.Second):
				}
			})

			first := firstStamp(t, m)
			if limit := started.Add(time.Second + 500*time.Millisecond + 5*time.Second); first.HostRAMHoldUntil.After(limit) {
				t.Fatalf("the outer bound is now + the hold bound + the slack, not the drain's deadline eight hours away: stamped %s, started %s", first.HostRAMHoldUntil, started)
			}
			if !dead {
				// A live parent: the stamp moves forward, and the holder never releases a lease its parent is still draining.
				deadline := time.Now().Add(10 * time.Second)
				moved := false
				for time.Now().Before(deadline) && !moved {
					for _, l := range m.Leases() {
						if l.HostRAMHoldUntil.After(first.HostRAMHoldUntil.Add(300 * time.Millisecond)) {
							moved = true
						}
					}
					time.Sleep(20 * time.Millisecond)
				}
				if !moved {
					t.Fatal("the live parent never moved the outer bound forward")
				}
				time.Sleep(3 * time.Second) // past bound + slack + grace from the start: a stamp that did not move would have fired
				if !m.Inspect().Held {
					t.Fatal("the holder released a lease whose parent is alive and still draining")
				}
				return
			}
			// A dead parent: from the first stamp on, nothing re-stamps (it is killed).
			restore := gpulease.UseStampFault(func(uint64) error { return errors.New("the parent is dead") })
			t.Cleanup(restore)
			holderEnds(t, holderDone, 15*time.Second)
			if m.Inspect().Held {
				t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
			}
			if took := time.Since(started); took > 30*time.Second {
				t.Errorf("released after %s: within the bound, not at --wait", took)
			}
		})
	}
}

// Round 4, addendum 5a: the outer bound was also wrong in the TOO EARLY direction. The unload is not bounded by the drain's deadline
// (the maintenance client allows 15 minutes per request), so a drain that ends near its deadline followed by an unload longer than
// the bound and the grace made the holder release a LIVE reserve's lease in the middle of the unload: the warm-back was skipped, the
// seat left owed and cold, and the reserve failed with "no longer this reservation's". The reviewer's probe, compressed: --wait 1s,
// --ram-hold-max 300ms, a 1 s holder grace, an unload that takes 6 s. The live reserve's re-stamping under the drain AND the unload
// keeps the holder out of it. (Red against e7bcf2b5, the tree before the re-stamp; see the report.)
func TestAnUnloadLongerThanTheBoundDoesNotLoseALiveReservesLeaseToItsHolder(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.onUnload = func() { time.Sleep(6 * time.Second) }
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	fastHolder(t) // 20 ms poll, 1 s grace
	oldE, oldS := outerRefreshEvery, outerSlack
	outerRefreshEvery, outerSlack = 100*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { outerRefreshEvery, outerSlack = oldE, oldS })
	holderDone := detachWithRealHolder(t)

	args := append([]string{"--config", cfgPath, "--ram-hold-max", "300ms"}, strings.Fields("--detach --for 1h --wait 1s --drain --unload-seat --ram 30 --reason film")...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("the unload ran 6 s, the reserve was alive throughout, and the bound ended its wait: %v\n%s", err, stderr)
	}
	if strings.Contains(err.Error(), "no longer this reservation's") {
		t.Errorf("the holder released a live reserve's lease in the middle of its unload: %v", err)
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the reserve released its own lease and paid the warm-back (a skipped warm-back leaves the seat owed and cold): warms=%d owed=%q\n%s", n, m.SeatWarmOwed(), stderr)
	}
	holderEnds(t, holderDone, 15*time.Second)
}

// Round 4, addendum 5a: when the drain or the unload fails and the lease is gone, the error says when the outer bound passed, so the
// operator is not left to guess who released it.
func TestADrainErrorSaysWhenTheOuterBoundReleasedTheLease(t *testing.T) {
	_, m, _ := holderFixture(t, false)
	cause := errors.New("the unload failed")
	outer := time.Now().Add(-time.Minute)
	got := detachedMaintainErrorAfter(m, 7, cause, outer)
	for _, want := range []string{"the unload failed", "outer hold bound", outer.Format(time.Kitchen), "hidden holder"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("the error must say %q: %v", want, got)
		}
	}
	if plain := detachedMaintainErrorAfter(m, 7, cause, time.Time{}); strings.Contains(plain.Error(), "outer hold bound") {
		t.Errorf("no outer bound was set, so none is named: %v", plain)
	}
	if future := detachedMaintainErrorAfter(m, 7, cause, time.Now().Add(time.Hour)); strings.Contains(future.Error(), "outer hold bound") {
		t.Errorf("a bound that has not passed did not release anything: %v", future)
	}
}

// Round 5. With the hold bound off (--ram-hold-max 0) the wait for the host is ended by --wait alone, and so is a reserve that dies
// during the drain or the unload: the outer stamp is the --wait deadline, kept ahead of a live reserve by the same ticker (never
// behind now plus the slack), and the stamp says it is the outer phase. Before, there was no stamp: a dead parent left the holder
// nothing to act on until `gpu release`.
func TestAReserveWithNoHoldBoundStampsTheWaitDeadlineAsItsOuterBound(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1) // the drain waits: the reserve is "dying" in it
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	fastHolder(t)
	advance := heldHolderClock(t)
	holderDone := detachWithRealHolder(t)

	started := time.Now()
	args := append([]string{"--config", cfgPath, "--ram-hold-max", "0"}, strings.Fields("--detach --for 1h --wait 1h --drain --unload-seat --ram 30 --reason film")...)
	reserveDone := make(chan error, 1)
	go func() { reserveDone <- runGPUReserve(args) }()
	t.Cleanup(func() {
		f.inflight.Store(0)
		releaseAll(t, m)
		select {
		case <-reserveDone:
		case <-time.After(40 * time.Second):
		}
		select {
		case <-holderDone:
		case <-time.After(10 * time.Second):
		}
	})

	l := firstStamp(t, m)
	want := started.Add(time.Hour) // the --wait deadline: no bound, so --wait alone ends it
	if d := l.HostRAMHoldUntil.Sub(want); d < -5*time.Second || d > 30*time.Second {
		t.Errorf("with no hold bound the outer stamp is the --wait deadline (%s), got %s", want, l.HostRAMHoldUntil)
	}
	if l.HostRAMHoldPhase != gpulease.HoldPhaseOuter {
		t.Errorf("the stamp is the outer phase: %q", l.HostRAMHoldPhase)
	}

	// The reserve never comes back from the drain (dead, for this purpose): the holder sees the moment pass and releases the lease.
	advance(l.HostRAMHoldUntil.Add(holdBoundGrace + time.Second))
	holderEnds(t, holderDone, 20*time.Second)
	if m.Inspect().Held {
		t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
	}
}
