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
	want := started.Add(20*time.Second + 5*time.Minute) // the drain's deadline (queued + --wait) plus the hold bound
	if d := l.HostRAMHoldUntil.Sub(want); d < -time.Second || d > 30*time.Second {
		t.Errorf("the outer bound is the drain's deadline plus the hold bound (%s), got %s", want, l.HostRAMHoldUntil)
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
