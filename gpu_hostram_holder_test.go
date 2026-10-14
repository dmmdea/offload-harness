package main

// Review round on the F82 hold bound (both reviewers, HIGH): a detached hold's bound was enforced only by the foreground
// reserve. If it died during the wait (a tool timeout, Ctrl-C, a closed terminal) the hidden holder kept the cards for good and
// `gpu status` named a reserve that no longer exists. These tests pin the fix at its root and its edges:
//
//   - the hidden holder releases its OWN lease once the stamped end of the wait has passed and the lease still asks for the
//     host's RAM, warming the seat back only if the lease owes it;
//   - a reserve that is alive releases first, and the holder neither doubles the warm-back nor steps in while it runs, yet a
//     reserve that dies in the middle of its own release is still covered;
//   - a detached wait that --wait ends releases the lease too (cards are never held idle behind a figure the host cannot meet);
//   - a seat that was never unloaded is never warmed by an automatic release;
//   - a release that fails is reported as such, and no text claims a release before it happened.

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// fastHolder shortens the hidden holder's poll and its grace past the stamped end, for one test.
func fastHolder(t *testing.T) {
	t.Helper()
	oldPoll, oldGrace := holdPollEvery, holdBoundGrace
	holdPollEvery, holdBoundGrace = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { holdPollEvery, holdBoundGrace = oldPoll, oldGrace })
}

// holderFixture is the seat-backed host (70 committed once the seat is unloaded: a 30 GiB need never fits, the 21 GiB warm-back
// always does) with the seat loaded or not.
func holderFixture(t *testing.T, seatLoaded bool) (cfgPath string, m *gpulease.Manager, f *orderedSwap) {
	t.Helper()
	f = &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(seatLoaded)
	cfgPath, m = handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	return
}

// runHolder starts the hidden holder in-process, as `gpu hold` runs for a detached lease that asks for 30 GiB after its unload.
func runHolder(cfgPath string) <-chan error {
	args := holdArgs("media", time.Hour, 0, gpulease.Options{Reason: "film", HostRAMWaitGiB: 30}, nil, cfgPath)
	done := make(chan error, 1)
	go func() { done <- runGPUHold(args[2:]) }()
	return done
}

func stampWaitEnd(t *testing.T, m *gpulease.Manager, epoch uint64, until time.Time) {
	t.Helper()
	if err := m.Restamp(epoch, func(mt *gpulease.Meta) { mt.HostRAMHoldUntilMs = until.UnixMilli() }); err != nil {
		t.Fatal(err)
	}
}

func holderEnds(t *testing.T, done <-chan error, within time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the holder ends quietly: %v", err)
		}
	case <-time.After(within):
		t.Fatalf("the holder did not release its lease within %s of the stamped moment: the cards are held for good", within)
	}
}

// THE ROOT FIX. The reserve that took the lease is gone (nothing here releases it), the seat was unloaded for it (its warm-back
// is owed), and the stamp it left says when its wait ended. The holder holds until that moment and then releases its own lease:
// instances stopped, the owed seat warmed back, the lease gone.
func TestTheHiddenHolderReleasesItsOwnLeaseOncetheStampedWaitHasEndedAndNoReserveDid(t *testing.T) {
	cfgPath, m, f := holderFixture(t, false) // the seat was unloaded for this lease
	fastHolder(t)
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	done := runHolder(cfgPath)
	epoch := waitForLeases(t, m, 1)[0].Epoch
	until := time.Now().Add(500 * time.Millisecond)
	stampWaitEnd(t, m, epoch, until)

	time.Sleep(200 * time.Millisecond)
	if !m.Inspect().Held {
		t.Fatal("the holder released before the stamped end of the wait")
	}
	holderEnds(t, done, 15*time.Second)
	if time.Now().Before(until) {
		t.Errorf("the lease was released before the stamped moment %s", until)
	}
	if m.Inspect().Held {
		t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the seat this lease unloaded is warmed back once and the debt is paid: warms=%d owed=%q", n, m.SeatWarmOwed())
	}
}

// A seat nothing unloaded is owed no warm-back, so an automatic release must not load it (2026-09-23: a seat came up on three
// cards with nothing asking for it). The incident's own shape: the seat was not loaded.
func TestTheHolderThatReleasesItsOwnLeaseNeverWarmsASeatThatWasNotUnloaded(t *testing.T) {
	cfgPath, m, f := holderFixture(t, false) // cold, and no warm-back owed
	fastHolder(t)
	done := runHolder(cfgPath)
	epoch := waitForLeases(t, m, 1)[0].Epoch
	stampWaitEnd(t, m, epoch, time.Now().Add(200*time.Millisecond))
	holderEnds(t, done, 15*time.Second)
	if m.Inspect().Held {
		t.Error("the lease is released")
	}
	if n := f.warms.Load(); n != 0 || f.loaded.Load() {
		t.Errorf("a seat that was never unloaded is not warmed by an automatic release: warms=%d loaded=%v", n, f.loaded.Load())
	}
}

// The holder acts only on a lease that STILL ASKS for the host's RAM and whose stamped end has PASSED.
func TestTheHolderLeavesAnAdmittedLeaseAndAWaitThatHasNotEndedAlone(t *testing.T) {
	cfgPath, m, f := holderFixture(t, false)
	fastHolder(t)
	done := runHolder(cfgPath)
	epoch := waitForLeases(t, m, 1)[0].Epoch

	// Still waiting, stamped end in the future: held.
	stampWaitEnd(t, m, epoch, time.Now().Add(time.Hour))
	time.Sleep(300 * time.Millisecond)
	if !m.Inspect().Held {
		t.Fatal("a wait that has not ended must not be cut short by the holder")
	}
	// Admitted (it asks for nothing now): even a stale stamp in the past means nothing.
	if err := m.Restamp(epoch, func(mt *gpulease.Meta) {
		mt.HostRAMGiB, mt.HostRAMWaitGiB, mt.HostRAMHoldUntilMs = 30, 0, time.Now().Add(-time.Hour).UnixMilli()
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if !m.Inspect().Held {
		t.Fatal("an admitted lease is working: the holder must never release it on a stale stamp")
	}
	select {
	case err := <-done:
		t.Fatalf("the holder ended on its own: %v", err)
	default:
	}
	if n := f.warms.Load(); n != 0 {
		t.Errorf("nothing was released, so nothing was warmed: warms=%d", n)
	}
	releaseAll(t, m) // the way every holder ends today: the record goes, the holder notices
	holderEnds(t, done, 10*time.Second)
}

// detachWithRealHolder makes the detach seam run the real hidden holder (in-process) instead of a bare lease, so a test sees the
// holder and the parent act on the same record.
func detachWithRealHolder(t *testing.T) <-chan error {
	t.Helper()
	holderDone := make(chan error, 1)
	var once sync.Once
	oldFn := detachHolderFn
	detachHolderFn = func(fs *flag.FlagSet, class string, dur, wait time.Duration, opts gpulease.Options, auto *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		args := holdArgs(class, dur, wait, opts, auto, fs.Lookup("config").Value.String())
		once.Do(func() { go func() { holderDone <- runGPUHold(args[2:]) }() })
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if ls := m.Leases(); len(ls) > 0 {
				return ls[0].Epoch, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return 0, errors.New("the holder never took the lease")
	}
	t.Cleanup(func() { detachHolderFn = oldFn })
	return holderDone
}

const detachedFilm = "--detach --for 1h --wait 30s --drain --unload-seat --ram 30 --reason film"

// A reserve that is alive releases at the bound, and the holder it started notices its lease is gone and exits. The seat is warmed
// back ONCE (by the parent), and the holder neither doubles the warm nor steps in during the minutes a warm-back takes.
func TestAReserveThatIsAliveReleasesFirstAndTheHolderNeitherDoublesTheWarmNorStepsIn(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.warmHold = 700 * time.Millisecond // the warm-back takes a while: the holder polls many times during it
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	fastHolder(t)
	holderDone := detachWithRealHolder(t)

	args := append([]string{"--config", cfgPath, "--ram-hold-max", "300ms"}, strings.Fields(detachedFilm)...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("the bound ended the wait: %v\n%s", err, stderr)
	}
	holderEnds(t, holderDone, 15*time.Second)
	if m.Inspect().Held {
		t.Errorf("the lease is released: %+v", m.Inspect())
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the seat is warmed back exactly once, by the reserve: warms=%d owed=%q\n%s", n, m.SeatWarmOwed(), stderr)
	}
	if strings.Contains(stderr, "LEASE NOT RELEASED") {
		t.Errorf("both parties releasing is harmless and silent:\n%s", stderr)
	}
}

// THE PARENT DIES MID-RELEASE. The reserve reached the bound and began to warm the seat back, and was killed (a tool timeout
// lands at exactly the default bound). Its wait is over and its stamp is cleared, but it pushed the stamped end out before the
// slow part, so the holder, which sees a moment that then passes, releases the lease the reserve will never finish releasing.
func TestAReserveThatDiesInTheMiddleOfItsOwnReleaseIsStillCoveredByTheHolder(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	var warmCalls int
	var mu sync.Mutex
	stuck := make(chan struct{})
	f.onWarm = func() { // the first warm (the parent's) never finishes until the test lets it; the holder's does
		mu.Lock()
		warmCalls++
		first := warmCalls == 1
		mu.Unlock()
		if first {
			<-stuck
		}
	}
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	fastHolder(t)
	oldGrace := detachedReleaseGrace
	detachedReleaseGrace = 400 * time.Millisecond
	t.Cleanup(func() { detachedReleaseGrace = oldGrace })
	holderDone := detachWithRealHolder(t)

	args := append([]string{"--config", cfgPath, "--ram-hold-max", "300ms"}, strings.Fields(detachedFilm)...)
	reserveDone := make(chan error, 1)
	go func() { reserveDone <- runGPUReserve(args) }()
	t.Cleanup(func() { // let the stuck warm-back finish, and wait for the reserve to end before the fixtures go
		close(stuck)
		select {
		case <-reserveDone:
		case <-time.After(20 * time.Second):
		}
	})

	// The reserve is stuck in its warm-back; the holder must end the lease on its own once the pushed-out moment passes.
	holderEnds(t, holderDone, 20*time.Second)
	if m.Inspect().Held {
		t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
	}
	select {
	case err := <-reserveDone:
		t.Fatalf("the reserve was supposed to be stuck in its warm-back: %v", err)
	default:
	}
	if m.SeatWarmOwed() != "" {
		t.Errorf("the holder paid the owed warm-back before it released: owed=%q", m.SeatWarmOwed())
	}
}

// THE REVIEW-ROUND-3 WINDOW. The reserve's wait ends without admission and the reserve dies at that very instant: after the wait
// stopped, before its own release pushed the stamp out. The wait used to clear the stamp as it ended, so for those milliseconds
// the record said "asks for 30 GiB, no bound", which the holder cannot act on, and a reserve killed there (a tool timeout lands at
// exactly the default bound) left the cards held for good, the incident itself. The stamp now stays until admission, so the holder
// has a moment to act on whichever way the wait ended. The test runs the real holder against the real wait and then does what the
// dead reserve does: nothing.
func TestAReserveThatDiesTheInstantItsWaitEndsLeavesTheHolderAMomentToAct(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bound       time.Duration
		cancelAfter time.Duration // a signal ends the wait this long after it began (0 = the bound ends it)
	}{
		{"the bound ends the wait", 400 * time.Millisecond, 0},
		{"a signal ends the wait before the bound", 2 * time.Second, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, m, f := holderFixture(t, false)
			fastHolder(t)
			holderDone := runHolder(cfgPath)
			epoch := waitForLeases(t, m, 1)[0].Epoch
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelAfter > 0 {
				time.AfterFunc(tc.cancelAfter, cancel)
			}
			start := time.Now()
			err := awaitHostRAMBounded(ctx, func(o gpulease.AwaitHostRAMOptions) error { return m.AwaitHostRAM(epoch, o) },
				start.Add(time.Minute), time.Minute, hostRAMHold{max: tc.bound, heldSince: start}, io.Discard)
			if err == nil {
				t.Fatal("the host never has the room: the wait ends without admission")
			}
			// The reserve is dead from here on: nothing releases the lease and nothing pushes the stamp out.
			holderEnds(t, holderDone, 5*time.Second)
			if time.Since(start) < tc.bound {
				t.Errorf("the holder let go %s in, before the moment the wait was stamped to end (%s)", time.Since(start), tc.bound)
			}
			if m.Inspect().Held {
				t.Errorf("the holder ended but the lease is still held: %+v", m.Inspect())
			}
			if n := f.warms.Load(); n != 0 {
				t.Errorf("the seat was never unloaded for this lease, so nothing is warmed: warms=%d", n)
			}
		})
	}
}

// A detached wait that --wait ends first used to leave the lease held, non-exclusive and idle, indefinitely: the incident from a
// smaller --wait. Cards are never held idle behind a RAM figure the host cannot meet, so it releases exactly as the bound does,
// and the failure names what ended the wait. The warm-back is attempted under the release's guards: here the host is still too
// short for the seat (90 + 21 > 92), so it stays owed.
func TestADetachedWaitThatWaitEndsAlsoReleasesTheLease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"the default bound is longer than --wait", nil},
		{"no bound at all", []string{"--ram-hold-max", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 90)
			shortFloor(t, 400*time.Millisecond)
			var held *gpulease.Lease
			oldFn := detachHolderFn
			detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
				l, err := m.TryAcquire(gpulease.Class(class), opts)
				if err != nil {
					return 0, err
				}
				held = l
				return l.Epoch(), nil
			}
			t.Cleanup(func() {
				detachHolderFn = oldFn
				if held != nil {
					_ = held.Release()
				}
			})
			args := append([]string{"--config", cfgPath, "--detach", "--for", "1h", "--wait", "3s", "--drain", "--unload-seat", "--ram", "30", "--reason", "film"}, tc.flags...)
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if n := f.unloads.Load(); n != 1 {
				t.Fatalf("the parent drains and unloads before it asks the host: unloads=%d err=%v\n%s", n, err, stderr)
			}
			if err == nil {
				t.Fatalf("the host never admitted the lease: the detached reserve must fail\n%s", stderr)
			}
			for _, want := range []string{"waiting for host RAM: needs 30.0 GiB, committed 90.0 of 100.0 GiB physical", "within --wait 3s", "being released"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure must say %q (what ended the wait, and what happens to the lease): %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "still held") {
				t.Errorf("the lease is no longer kept: %v", err)
			}
			if info := m.Inspect(); info.Held {
				t.Errorf("a detached wait that ended without admission releases the lease, not holds the cards idle: %+v", info)
			}
			if m.SeatWarmOwed() != "seat" || f.warms.Load() != 0 {
				t.Errorf("the host is still too short for the seat, so its warm-back stays owed and is not forced: owed=%q warms=%d", m.SeatWarmOwed(), f.warms.Load())
			}
		})
	}
}

// The seat was never loaded (the incident's own shape), so the unload took nothing down and no warm-back is owed: an automatic
// release of a detached hold must not load it. Only the wrapper form had this assertion.
func TestADetachedReleaseAtTheBoundNeverWarmsASeatThatWasNeverLoaded(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(false)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	var held *gpulease.Lease
	oldFn := detachHolderFn
	detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		l, err := m.TryAcquire(gpulease.Class(class), opts)
		if err != nil {
			return 0, err
		}
		held = l
		return l.Epoch(), nil
	}
	t.Cleanup(func() {
		detachHolderFn = oldFn
		if held != nil {
			_ = held.Release()
		}
	})
	args := append([]string{"--config", cfgPath, "--ram-hold-max", "200ms"}, strings.Fields(detachedFilm)...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("the bound ended the wait: %v\n%s", err, stderr)
	}
	if m.Inspect().Held {
		t.Error("the lease is released")
	}
	if n := f.warms.Load(); n != 0 || f.loaded.Load() {
		t.Errorf("a seat that was never loaded must not be warmed by the automatic release: warms=%d loaded=%v\n%s", n, f.loaded.Load(), stderr)
	}
}

// NO TEXT CLAIMS A RELEASE BEFORE IT HAPPENED, and one that fails is said: the failure is built before the release runs, so it
// says "being released"; the outcome comes after, and a release that did not happen prints one line naming the epoch and the way out.
func TestAReleaseThatFailsAtTheBoundIsSaidAndNeverClaimedAsDone(t *testing.T) {
	for _, form := range []string{"wrapper", "detached"} {
		t.Run(form, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 70)
			shortFloor(t, time.Second)
			var args []string
			if form == "wrapper" {
				args, _ = unloadSeatReserve(t, cfgPath, "--wait", "30s", "--drain", "--unload-seat", "--ram", "30", "--ram-hold-max", "200ms")
			} else {
				var held *gpulease.Lease
				oldFn := detachHolderFn
				detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
					l, err := m.TryAcquire(gpulease.Class(class), opts)
					if err != nil {
						return 0, err
					}
					held = l
					return l.Epoch(), nil
				}
				t.Cleanup(func() {
					detachHolderFn = oldFn
					if held != nil {
						_ = held.Release()
					}
				})
				args = append([]string{"--config", cfgPath, "--ram-hold-max", "200ms"}, strings.Fields(detachedFilm)...)
			}
			// Every lease-file removal fails from here on: the release cannot happen.
			restore := gpulease.UseRemoveFault(func(string) error { return errors.New("access denied") })
			t.Cleanup(func() { restore(); releaseAll(t, m) })
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if err == nil || !strings.Contains(err.Error(), "hold bound") {
				t.Fatalf("the bound ended the wait: %v\n%s", err, stderr)
			}
			if strings.Contains(err.Error(), "the lease is released") {
				t.Errorf("the failure was built before the release and must not claim it:\n%s", err)
			}
			if !strings.Contains(err.Error(), "being released") {
				t.Errorf("the failure says only what is true when it is built, that the lease is being released:\n%s", err)
			}
			var lines []string
			for _, l := range strings.Split(stderr, "\n") {
				if strings.Contains(l, "LEASE NOT RELEASED") {
					lines = append(lines, l)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("exactly one line says the lease was not released, got %d:\n%s", len(lines), stderr)
			}
			for _, want := range []string{"gpu reserve: LEASE NOT RELEASED (epoch 1)", "access denied", "gpu release --warm-seat --epoch 1"} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("the line must say %q: %s", want, lines[0])
				}
			}
			if !m.Inspect().Held {
				t.Error("the removal failed, so the lease is still held (this is what the line tells the operator)")
			}
		})
	}
}

// A lease that is already gone when the detached reserve goes to release it (the holder got there first) is not a failure to
// report: nothing was left to release.
func TestAReleaseOfALeaseTheHolderAlreadyReleasedIsNotAFailure(t *testing.T) {
	_, m, _ := holderFixture(t, false)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("the bound's failure")
	var got error
	stderr := captureStderr(t, func() { got = releaseAfterRAMWait(m, epoch, loadCfgPath(t.TempDir()+"/none.json"), cause) })
	if strings.Contains(stderr, "LEASE NOT RELEASED") {
		t.Errorf("a lease that is already gone is not a failed release:\n%s", stderr)
	}
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), "no longer this reservation's") {
		t.Errorf("the cause is kept and the lease is said to be no longer this reservation's: %v", got)
	}
}

// awaitHostRAMBounded stamps the END of the wait, whatever ends it: a detached hold whose reserve dies is released by its holder
// at the moment --wait would have ended the wait too, not only at the hold bound.
func TestTheStampedEndOfTheWaitIsWhicheverOfTheBoundAndWaitComesFirst(t *testing.T) {
	short := &gpulease.ErrHostRAM{}
	short.Why = "waiting for host RAM: needs 30.0 GiB, committed 70.0 of 100.0 GiB physical, 8.0 GiB headroom"
	for _, tc := range []struct {
		name     string
		max      time.Duration
		deadline time.Duration
		boundErr bool
	}{
		{"the bound ends it first", 10 * time.Minute, 8 * time.Hour, true},
		{"--wait ends it first", 10 * time.Minute, time.Minute, false},
		{"no bound", 0, 2 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got gpulease.AwaitHostRAMOptions
			await := func(o gpulease.AwaitHostRAMOptions) error { got = o; return short }
			err := awaitHostRAMBounded(context.Background(), await, time.Now().Add(tc.deadline), tc.deadline, hostRAMHold{max: tc.max, heldSince: time.Now()}, &strings.Builder{})
			if got.HoldUntil.IsZero() || !got.HoldUntil.Equal(got.Deadline) {
				t.Errorf("the stamped end of the wait is the wait's own deadline (bound or --wait): HoldUntil %v, Deadline %v", got.HoldUntil, got.Deadline)
			}
			var bound *errHostRAMHoldBound
			if errors.As(err, &bound) != tc.boundErr {
				t.Errorf("the bound's failure only when the bound ended the wait: %v", err)
			}
		})
	}
}

// A FENCE READ THAT FAILS FOR A MOMENT IS NOT A LOST LEASE. The holder's exit releases its lease, and its fence (Lease.Check)
// reads the record: a read that lands while a restamp replaces the record fails for a moment on Windows ("access denied"), and the
// holder took that for "the lease is gone" and let go of a live lease. The reserve now restamps the record more often during a
// host-RAM wait (the stamp, its clear, the push-out before its own release), and the holder polls it, so the race had to be closed:
// the holder confirms a failed fence with further looks before it lets go. Found as a 1-in-60 flake of the test above.
func TestTheHolderDoesNotLetGoOfALiveLeaseBecauseOneFenceReadFailed(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	fastHolder(t)
	var calls atomic.Int32
	restore := gpulease.UseCheckFault(func(uint64) error {
		if calls.Add(1) == 1 {
			return errors.New("access denied: the record was being replaced")
		}
		return nil
	})
	t.Cleanup(restore)
	done := runHolder(cfgPath)
	waitForLeases(t, m, 1)
	time.Sleep(500 * time.Millisecond) // many polls: the first fence read failed, the rest are fine
	if !m.Inspect().Held {
		t.Fatal("one fence read that failed while the record was being replaced released a live lease")
	}
	select {
	case err := <-done:
		t.Fatalf("the holder let go of its lease on one failed fence read: %v", err)
	default:
	}
	if calls.Load() < 2 {
		t.Fatalf("the holder never looked again: %d fence read(s)", calls.Load())
	}
	// A lease that really is gone still ends the holder promptly.
	releaseAll(t, m)
	holderEnds(t, done, 10*time.Second)
}
