package main

// Review round 3, items B and C: the hidden holder's own failure paths.
//
// B: a holder whose self-release failed wrote the error to a log in the temp directory and exited 0, and `gpu status` went on
// saying "the holder releases the lease at its next poll" although nothing polled. The holder retries its release, exits non-zero
// when it gives up, marks the record, and the status text says the holder gave up.
//
// C: a holder that let go of its lease because the fence could not be READ (a corrupt record, a permission error) exited without
// a word. It prints the error it let go on, and it retries a failed READ for a while (one line) before it lets go, since a read
// that fails passes more often than a verdict does.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func fastHolderRetries(t *testing.T) {
	t.Helper()
	oldT, oldP, oldIO := holdReleaseTries, holdReleasePause, holdIORetryFor
	holdReleaseTries, holdReleasePause, holdIORetryFor = 4, 5*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { holdReleaseTries, holdReleasePause, holdIORetryFor = oldT, oldP, oldIO })
}

func TestAHolderWhoseOwnReleaseKeepsFailingRetriesReportsExitsNonZeroAndMarksTheRecord(t *testing.T) {
	var removals atomic.Int32
	m, done, epoch := func() (*gpulease.Manager, <-chan error, uint64) {
		cfgPath, m, _ := holderFixture(t, false)
		fastHolder(t)
		fastHolderRetries(t)
		restore := gpulease.UseRemoveFault(func(string) error { removals.Add(1); return errors.New("access denied") })
		t.Cleanup(func() { restore(); releaseAll(t, m) })
		done := runHolder(cfgPath)
		return m, done, waitForLeases(t, m, 1)[0].Epoch
	}()
	var err error
	stderr := captureStderr(t, func() {
		stampWaitEnd(t, m, epoch, time.Now().Add(100*time.Millisecond))
		select {
		case err = <-done:
		case <-time.After(30 * time.Second):
			t.Error("the holder never ended")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "could not be released") || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("a holder that could not release its lease exits non-zero and says why: %v", err)
	}
	if n := removals.Load(); n < 3 {
		t.Errorf("the release is retried a few times before the holder gives up, got %d removal attempt(s)", n)
	}
	if !strings.Contains(stderr, "LEASE NOT RELEASED (epoch 1)") {
		t.Errorf("the holder says the lease was not released:\n%s", stderr)
	}
	rec := m.Leases()
	if len(rec) != 1 || rec[0].HolderGaveUp.IsZero() || !strings.Contains(rec[0].HolderGaveUpWhy, "access denied") {
		t.Errorf("the record says the holder gave up, and why: %+v", rec)
	}
}

func TestAHolderWhoseReleaseFailsTwiceAndThenWorksEndsCleanly(t *testing.T) {
	var removals atomic.Int32
	cfgPath, m, _ := holderFixture(t, false)
	fastHolder(t)
	fastHolderRetries(t)
	restore := gpulease.UseRemoveFault(func(string) error {
		if removals.Add(1) <= 2 {
			return errors.New("access denied")
		}
		return nil
	})
	t.Cleanup(func() { restore(); releaseAll(t, m) })
	done := runHolder(cfgPath)
	epoch := waitForLeases(t, m, 1)[0].Epoch
	stampWaitEnd(t, m, epoch, time.Now().Add(100*time.Millisecond))
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the third try released it: the holder ends cleanly: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the holder never ended")
	}
	if m.Inspect().Held {
		t.Errorf("the lease is released: %+v", m.Inspect())
	}
}

func TestStatusSaysTheHolderGaveUpAndNeverPromisesItsNextPoll(t *testing.T) {
	now := time.Now()
	until := now.Add(-90 * time.Second)
	gaveUp := now.Add(-30 * time.Second)
	note := holderGaveUpNote(gaveUp, "access denied", 4, false)
	for _, want := range []string{"the holder gave up releasing it", gaveUp.Format(time.Kitchen), "access denied", "`gpu release --epoch 4`"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must say %q: %s", want, note)
		}
	}
	if !strings.Contains(holderGaveUpNote(gaveUp, "x", 4, true), "`gpu release --warm-seat --epoch 4`") {
		t.Error("a warm-back is owed: the way out loads the seat back")
	}
	if holderGaveUpNote(time.Time{}, "", 4, false) != "" {
		t.Error("a holder that has not given up adds nothing")
	}
	// The line with the marker is not the one that promises a poll.
	line := hostRAMAskedStatus(30, 12*time.Minute, until, "", now, false, 4, false, false, gaveUp, "access denied")
	if strings.Contains(line, "at its next poll") || !strings.Contains(line, "the holder gave up releasing it") {
		t.Errorf("nothing polls the lease any more, so the text must not promise a poll:\n%s", line)
	}
}

// C: a fence that cannot be READ.
func TestAHolderRetriesAFenceItCannotReadAndLetsGoOnlyAfterSayingWhy(t *testing.T) {
	unreadable := &gpulease.RecordUnreadableError{Epoch: 1, Err: errors.New("access denied")}
	t.Run("it passes", func(t *testing.T) {
		cfgPath, m, _ := holderFixture(t, false)
		fastHolder(t)
		fastHolderRetries(t)
		// The retry window is by the clock and the faults are by the count of reads: under CPU starvation a short window ran out
		// before the holder reached the read that works (1 run in 8 on two cores with six busy loops). This case costs nothing for a
		// long one, it ends at the working read; the case that never recovers is the one that needs a short window.
		holdIORetryFor = 60 * time.Second
		// The first read fails, the three confirming looks fail, and four more fail inside the retry window: the ninth works. Counted
		// by reads and not by the clock, so no scheduler delay decides whether the holder reached the retry.
		var reads atomic.Int32
		restore := gpulease.UseCheckFault(func(uint64) error {
			if reads.Add(1) <= 8 {
				return unreadable
			}
			return nil
		})
		t.Cleanup(restore)
		var done <-chan error
		stderr := captureStderr(t, func() {
			done = runHolder(cfgPath)
			waitForLeases(t, m, 1)
			time.Sleep(900 * time.Millisecond) // past the retry window: had the holder let go it would have by now
			if !m.Inspect().Held {
				t.Error("a read that failed for a moment and then worked must not cost the holder its lease")
			}
			select {
			case err := <-done:
				t.Errorf("the holder ended on a read that recovered: %v", err)
			default:
			}
		})
		if n := strings.Count(stderr, "cannot read the record of lease epoch 1"); n != 1 {
			t.Errorf("one line says the record cannot be read and that the holder retries, got %d:\n%s", n, stderr)
		}
		if strings.Contains(stderr, "letting go of lease epoch") {
			t.Errorf("the holder did not let go:\n%s", stderr)
		}
		releaseAll(t, m)
		holderEnds(t, done, 10*time.Second)
	})
	t.Run("it never does", func(t *testing.T) {
		cfgPath, m, _ := holderFixture(t, false)
		fastHolder(t)
		fastHolderRetries(t)
		restore := gpulease.UseCheckFault(func(uint64) error { return unreadable })
		t.Cleanup(restore)
		var done <-chan error
		stderr := captureStderr(t, func() {
			done = runHolder(cfgPath)
			waitForLeases(t, m, 1)
			holderEnds(t, done, 20*time.Second)
		})
		for _, want := range []string{"cannot read the record of lease epoch 1", "access denied", "letting go of lease epoch 1"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("the holder says %q when it lets go on a fence it could not read:\n%s", want, stderr)
			}
		}
	})
}

// A verdict (the lease is gone) is not retried, but the holder says what it let go on.
func TestAHolderThatLetsGoOfALeaseThatIsGoneSaysWhatItSaw(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	fastHolder(t)
	fastHolderRetries(t)
	var done <-chan error
	start := time.Now()
	stderr := captureStderr(t, func() {
		done = runHolder(cfgPath)
		waitForLeases(t, m, 1)
		releaseAll(t, m) // an operator releases it from outside
		holderEnds(t, done, 20*time.Second)
	})
	if time.Since(start) > 15*time.Second {
		t.Error("a verdict is not retried for the I/O window")
	}
	if !strings.Contains(stderr, "letting go of lease epoch 1") || !strings.Contains(stderr, "gone") {
		t.Errorf("the holder says what it let go on:\n%s", stderr)
	}
	if strings.Contains(stderr, "cannot read the record") {
		t.Errorf("a lease that is gone is not a failed read:\n%s", stderr)
	}
}
