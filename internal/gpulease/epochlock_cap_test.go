package gpulease

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"
)

// A second acquirer spins for the epoch lock for epochLockAttempts x epochLockPause (2 s) and then fails. The gate waits for a rename in
// flight, up to a budget per file, and a hold of the lock reads every lease's heartbeat. With a fresh staged leftover beside the
// heartbeat of each of N leases (writers that died a moment ago) and nothing remembered, the hold was N budgets long, and from the ninth
// lease on it outlasted the spin: the second acquirer gave up on a lock that was held by a live process, and a queued acquire left the
// queue with an error. The wait under the lock is therefore bounded as a whole, whatever the number of files.

// stuckLeases takes n card leases on distinct cards and puts a fresh leftover beside each heartbeat.
func stuckLeases(t *testing.T, c *gateClock, m *Manager, n int) []*Lease {
	t.Helper()
	var held []*Lease
	for i := 0; i < n; i++ {
		l := mustDevices(t, m, fmt.Sprintf("gpu-test-%d", 10+i))
		held = append(held, l)
		t.Cleanup(func() { _ = l.Release() })
	}
	for _, l := range held {
		stuckBeside(t, c, m.heartbeatPath(l.Epoch()))
	}
	return held
}

func TestTheGateWaitsOfOneHoldOfTheEpochLockStayInsideTheSpinOfASecondAcquirer(t *testing.T) {
	spin := time.Duration(epochLockAttempts) * epochLockPause
	for _, n := range []int{3, 9, 12} {
		t.Run(fmt.Sprintf("%d leases, each with a fresh leftover beside its heartbeat", n), func(t *testing.T) {
			c := onGateClock(t)
			m := scopedRealClock(t)
			stuckLeases(t, c, m, n)
			gatePauses.Store(0)

			start := c.now()
			if err := m.verifyNoDeviceLeases(1000); err == nil {
				t.Fatal("the verification found no live card lease: the test does not observe the reads")
			}
			held := c.now().Sub(start) // the gate's clock moves only when the gate pauses: this is the gate time of the hold
			if held >= spin {
				t.Errorf("%d leases: the verification waited %v on the gate with the epoch lock held, the spin of a second acquirer is %v (%d pauses)", n, held, spin, gatePauses.Load())
			}
		})
	}
}

// The same holds for a card grant, whose sweep and judgement of every lease run under the lock, when the leftovers land after the
// grant's unlocked probe (so that nothing was waited for yet and nothing is remembered).
func TestTheGateWaitsOfACardGrantThatMeetsLeftoversAfterItsProbeStayInsideTheSpin(t *testing.T) {
	c := onGateClock(t)
	m := scopedRealClock(t)
	var held []*Lease
	for i := 0; i < 12; i++ {
		l := mustDevices(t, m, fmt.Sprintf("gpu-test-%d", 10+i))
		held = append(held, l)
		t.Cleanup(func() { _ = l.Release() })
	}
	m.afterHostReadHook = func() { // after the probe, before the lock
		for _, l := range held {
			stuckBeside(t, c, m.heartbeatPath(l.Epoch()))
		}
	}
	p := countGatePausesAroundTheLock(t, m.epochLockPath())
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "card", Devices: []string{card3}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("a grant of a free card = %v", err)
	}
	defer l.Release()
	spin := time.Duration(epochLockAttempts) * epochLockPause
	if under := time.Duration(p.under.Load()) * absentPause; p.under.Load() == 0 || under >= spin {
		t.Errorf("the grant waited %v (%d pauses) on the gate with the epoch lock held, want some, and less than the spin of a second acquirer, %v: the test must observe the waits it bounds", under, p.under.Load(), spin)
	}
}

// The cap belongs to a hold and ends with it: a read outside any hold waits its whole budget, a read of another directory is not
// counted against a hold of this one, and the hold is forgotten when the lock is let go, however the function inside it ended.
func TestTheCapOfAHoldOfTheEpochLockIsPerHoldAndEndsWithIt(t *testing.T) {
	t.Run("a read outside any hold waits its whole budget", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		l := mustDevices(t, m, card0)
		defer l.Release()
		hb := m.heartbeatPath(l.Epoch())
		stuckBeside(t, c, hb)
		_ = removeIfExists(hb)
		gatePauses.Store(0)
		if _, err := readSharedFile(hb); err == nil {
			t.Fatal("the read of a heartbeat with only a stuck leftover beside it succeeded")
		}
		if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
			t.Errorf("a read outside any hold paused %d times, want %d (the whole budget)", got, want)
		}
	})
	t.Run("a hold of another directory does not shorten a wait here", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		other := scopedRealClock(t)
		end := beginEpochHold(other.leaseDir())
		defer end()
		c.sleep(2 * lockGateCap) // that hold is long over its cap
		l := mustDevices(t, m, card0)
		defer l.Release()
		hb := m.heartbeatPath(l.Epoch())
		stuckBeside(t, c, hb)
		_ = removeIfExists(hb)
		gatePauses.Store(0)
		_, _ = readSharedFile(hb)
		if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
			t.Errorf("a read in one lease directory paused %d times while another directory's hold was over its cap, want %d", got, want)
		}
	})
	t.Run("the hold is forgotten when the lock is let go, and the cap answered at once while it ran", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		l := mustDevices(t, m, card0)
		defer l.Release()
		hb := m.heartbeatPath(l.Epoch())
		stuckBeside(t, c, hb)
		_ = removeIfExists(hb)
		var inside error
		lockCapHits.Store(0)
		gatePauses.Store(0)
		if err := m.withEpochLock(func() error {
			c.sleep(lockGateCap) // the hold has run for its cap
			_, inside = readSharedFile(hb)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errorsIsInFlight(inside) {
			t.Errorf("a read inside a hold that had spent its cap = %v, want the I/O failure of a wait that ran out", inside)
		}
		if gatePauses.Load() != 0 || lockCapHits.Load() != 1 {
			t.Errorf("the read inside the spent hold paused %d times and hit the cap %d times, want 0 and 1", gatePauses.Load(), lockCapHits.Load())
		}
		if holdCount.Load() != 0 {
			t.Errorf("%d holds are still registered after the lock was let go", holdCount.Load())
		}
		if _, err := readSharedFile(hb); !errorsIsInFlight(err) {
			t.Fatalf("a read after the hold = %v", err)
		}
		if got := gatePauses.Load(); got == 0 {
			t.Errorf("a read after the hold took no pause: the cap outlived the hold")
		}
	})
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func errorsIsInFlight(err error) bool { return errors.Is(err, errRenameInFlight) }
