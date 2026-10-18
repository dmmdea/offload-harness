package main

// Review round 3 (D, F, J): the outcome of a release the harness makes on its own.
//
//   - ReleaseByEpoch answers (false, nil) for "no lease was held", and the callers used to ignore the bool: a lease that was in
//     fact still held was reported as released (the detached reserve) or as "no lease is held" (`gpu release`), and the cards sat
//     for the 15 minute grace;
//   - when a release fails, the error the command returns says so as its last word (it used to end "the lease is being released");
//   - the line that tells the operator how to free a lease advises --warm-seat only when a warm-back is owed.

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// answersNothingHeld makes the release answer what the review found it answering for a lease it could not examine.
func answersNothingHeld(t *testing.T) {
	t.Helper()
	old := releaseByEpochFn
	releaseByEpochFn = func(*gpulease.Manager, uint64) (bool, error) { return false, nil }
	t.Cleanup(func() { releaseByEpochFn = old })
}

func TestADetachedReleaseThatReportsNothingHeldWhileTheLeaseIsHeldIsAFailureAndIsSaid(t *testing.T) {
	_, m, _ := holderFixture(t, false)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	t.Cleanup(func() { releaseAll(t, m) })
	answersNothingHeld(t)

	cause := errors.New("the bound's failure")
	var got error
	stderr := captureStderr(t, func() { got = releaseAfterRAMWait(m, epoch, loadCfgPath(t.TempDir()+"/none.json"), cause) })
	var lines int
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "LEASE NOT RELEASED (epoch 1)") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("the release said nothing was held while epoch 1 is held: exactly one LEASE NOT RELEASED line, got %d:\n%s", lines, stderr)
	}
	if !m.Inspect().Held {
		t.Fatal("the fake release released nothing")
	}
	if got == nil || !errors.Is(got, cause) || !strings.Contains(got.Error(), "STILL HELD") {
		t.Errorf("the returned error keeps its cause and ends by saying the lease is still held: %v", got)
	}
}

// `gpu release` said "no lease is held" and exited 0 for a lease that was held.
func TestGPUReleaseNeverSaysNoLeaseIsHeldWhileOneIs(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	if _, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseAll(t, m) })

	answersNothingHeld(t)
	var rerr error
	out := captureStdout(t, func() { rerr = runGPURelease([]string{"--config", cfgPath}) })
	if rerr == nil || strings.Contains(out, "no lease is held") {
		t.Fatalf("a lease is held and the release did nothing: that is an error, not \"no lease is held\" (err=%v, stdout=%q)", rerr, out)
	}
	if !strings.Contains(rerr.Error(), "still held") {
		t.Errorf("the error says the lease is still held: %v", rerr)
	}

	// With nothing held the verb says so, as it always did.
	releaseAll(t, m)
	out = captureStdout(t, func() { rerr = runGPURelease([]string{"--config", cfgPath}) })
	if rerr != nil || !strings.Contains(out, "no lease is held") {
		t.Errorf("nothing is held: the verb says so and succeeds (err=%v, stdout=%q)", rerr, out)
	}
}

// The failure of a bound or a wait is built BEFORE the release runs, so it says the lease is being released; when that release then
// fails, the error the command returns must END by saying what is true.
func TestWhenTheReleaseFailsTheReturnedErrorEndsByNamingTheFailureAndTheLeaseStillHeld(t *testing.T) {
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
				detachWithBareLease(t)
				args = append([]string{"--config", cfgPath, "--ram-hold-max", "200ms"}, strings.Fields(detachedFilm)...)
			}
			restore := gpulease.UseRemoveFault(func(string) error { return errors.New("access denied") })
			t.Cleanup(func() { restore(); releaseAll(t, m) })
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if err == nil || !strings.Contains(err.Error(), "hold bound") {
				t.Fatalf("the bound ended the wait: %v\n%s", err, stderr)
			}
			text := err.Error()
			at := strings.LastIndex(text, "STILL HELD")
			if at < 0 || !strings.Contains(text[at:], "access denied") || at < strings.Index(text, "being released") {
				t.Errorf("the last thing the error says is that the release failed and why, after the claim that it was being released:\n%s", text)
			}
		})
	}
}

// detachWithBareLease makes the detach seam take the lease in this process, as the hidden holder would, with no poll loop.
func detachWithBareLease(t *testing.T) {
	t.Helper()
	var held *gpulease.Lease
	old := detachHolderFn
	detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		l, err := m.TryAcquire(gpulease.Class(class), opts)
		if err != nil {
			return 0, err
		}
		held = l
		return l.Epoch(), nil
	}
	t.Cleanup(func() {
		detachHolderFn = old
		if held != nil {
			_ = held.Release()
		}
	})
}

// J: the advice names --warm-seat only when a warm-back is owed.
func TestTheNotReleasedLineAdvisesAWarmBackOnlyWhenOneIsOwed(t *testing.T) {
	cause := errors.New("access denied")
	owed := leaseNotReleasedLine("gpu reserve", 7, cause, true)
	if !strings.Contains(owed, "gpu release --warm-seat --epoch 7") {
		t.Errorf("a warm-back is owed: the way out loads the seat back: %s", owed)
	}
	plain := leaseNotReleasedLine("gpu reserve", 7, cause, false)
	if !strings.Contains(plain, "gpu release --epoch 7") || strings.Contains(plain, "--warm-seat") {
		t.Errorf("no warm-back is owed: advising --warm-seat could load a seat nothing unloaded: %s", plain)
	}
}

func TestStatusPastTheStampedEndNamesTheReleaseThatIsHonestWhetherOrNotAHolderIsRunning(t *testing.T) {
	now := time.Now()
	until := now.Add(-90 * time.Second)
	holder := hostRAMAskedStatus(30, 30, 12*time.Minute, until, "", now, false, 4, false, false, time.Time{}, "")
	for _, want := range []string{"the holder, if one is running, releases the lease at its next poll", "`gpu release --epoch 4` frees it now"} {
		if !strings.Contains(holder, want) {
			t.Errorf("a --detach lease past its stamped end: the holder may be gone, so the text says %q:\n%s", want, holder)
		}
	}
	if strings.Contains(holder, "--warm-seat") {
		t.Errorf("no warm-back is owed, so --warm-seat is not advised:\n%s", holder)
	}
	wrapper := hostRAMAskedStatus(30, 30, 12*time.Minute, until, "", now, true, 4, true, true, time.Time{}, "")
	if !strings.Contains(wrapper, "`gpu release --warm-seat --epoch 4`") {
		t.Errorf("a warm-back is owed: the wrapper's text advises --warm-seat:\n%s", wrapper)
	}
}
