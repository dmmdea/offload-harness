package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A whole-node acquisition creates meta.json by an exclusive create, and a holder's restamp replaces that same name by a rename (and
// on Windows a rename over a name can leave it absent for tens of milliseconds while the rename is still running). The restamp holds
// the epoch lock, so a creator that holds it too cannot create the claim inside that window. The tests pin that the claim is created
// under the lock, and that every way out of the locked section lets the lock go.

// TestAWholeNodeGrantCreatesItsClaimWhileHoldingTheEpochLock: the epoch lock is held at the instant the claim is created, not only
// while the epoch is issued. Before this the lock was let go as bumpEpoch returned, and the record was built and the claim created
// with nothing held, so an exclusive create could land inside a restamp's rename.
func TestAWholeNodeGrantCreatesItsClaimWhileHoldingTheEpochLock(t *testing.T) {
	m, _ := newTestManager(t)
	calls, held := 0, false
	m.beforeClaimHook = func() {
		calls++
		_, err := os.Stat(m.epochLockPath())
		held = err == nil
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
	if err != nil {
		t.Fatalf("TryAcquire = %v", err)
	}
	defer l.Release()
	if calls != 1 {
		t.Fatalf("the claim was created %d times, want once", calls)
	}
	if !held {
		t.Error("the claim was created without the epoch lock held: an exclusive create can then land inside a restamp's rename of the same name")
	}
	if exists(m.epochLockPath()) {
		t.Error("the epoch lock is still there after the grant")
	}
	if cur, rerr := m.readMeta(); rerr != nil || cur == nil || cur.Epoch != l.Epoch() {
		t.Errorf("the claim on disk = %+v, %v; want the grant's own record (epoch %d)", cur, rerr, l.Epoch())
	}
}

// TestAWholeNodeGrantThatMeetsAnotherClaimLetsTheEpochLockGoAndLeavesTheClaimAlone: the way out of the locked section when the
// exclusive create loses. Another holder's claim lands just before ours (the hook plants it by hand: a second acquisition would wait
// on the lock this one holds). The grant answers held, the lock is released, and the winner's record is untouched.
func TestAWholeNodeGrantThatMeetsAnotherClaimLetsTheEpochLockGoAndLeavesTheClaimAlone(t *testing.T) {
	m, now := newTestManager(t)
	m.beforeClaimHook = func() {
		rec, _ := json.Marshal(&Meta{Epoch: 99, Class: ClassMedia, Holder: Holder{PID: os.Getpid(), StartTimeMs: 4242}, Reason: "winner",
			AcquiredAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(DefaultTTL).UnixMilli(), RenewedAtMs: now.UnixMilli(), Format: RecordFormat})
		if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(m.metaPath(), rec, 0o666); err != nil {
			t.Error(err)
		}
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "loser"})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("TryAcquire = %v, want *ErrHeld for the claim that landed first", err)
	}
	if exists(m.epochLockPath()) {
		t.Error("the epoch lock is still there after a grant that lost the exclusive create")
	}
	cur, rerr := m.readMeta()
	if rerr != nil || cur == nil || cur.Epoch != 99 || cur.Reason != "winner" {
		t.Errorf("the claim on disk = %+v, %v; want the winner's record, untouched", cur, rerr)
	}
}

// TestAWholeNodeGrantReadsTheHoldersProcessBeforeItTakesTheEpochLock: what a record needs from the process table and from the session
// registry is read before the lock, so that those reads (they open files) are not paid while every other acquirer on the machine spins
// on the lock. A guard: it passes before the claim moved under the lock, and fails if the record's reads move in with it.
func TestAWholeNodeGrantReadsTheHoldersProcessBeforeItTakesTheEpochLock(t *testing.T) {
	m, _ := newTestManager(t)
	reads, underLock := 0, 0
	m.procStart = func(int) (int64, bool) {
		reads++
		if exists(m.epochLockPath()) {
			underLock++
		}
		return 4242, true
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole", Owner: Owner{PID: os.Getpid(), Session: "s1"}})
	if err != nil {
		t.Fatalf("TryAcquire = %v", err)
	}
	defer l.Release()
	if reads == 0 {
		t.Fatal("the grant never read a process start: the test does not observe the record's reads")
	}
	if underLock != 0 {
		t.Errorf("%d of %d process reads of the record were made with the epoch lock held, want none", underLock, reads)
	}
}

// TestTheWholeNodeVerificationPaysAtMostOneWaitPerFile: the second critical section of a whole-node grant (the verification that no
// card lease is live, which sweeps and then reads every record and heartbeat) is bounded the way a card grant's is
// (TestAFreshLeftoverStagedHeartbeatCostsAGrantAtMostOneWaitPerFile): three card leases are held and each heartbeat has a fresh staged
// file left beside it by a writer that died. The verification reads each heartbeat several times and waits for each file once. A pin
// for the count; the hold itself, in milliseconds, is a measurement in the change fragment.
func TestTheWholeNodeVerificationPaysAtMostOneWaitPerFile(t *testing.T) {
	c := onGateClock(t)
	m := scopedRealClock(t)
	var held []*Lease
	for _, card := range []string{card0, card1, card2} {
		l := mustDevices(t, m, card)
		held = append(held, l)
		t.Cleanup(func() { _ = l.Release() })
	}
	for _, l := range held {
		stuckBeside(t, c, m.heartbeatPath(l.Epoch()))
	}
	gatePauses.Store(0)
	gateExpiries.Store(0)

	if err := m.verifyNoDeviceLeases(1000); err == nil {
		t.Fatal("the verification found no live card lease: the test does not observe the reads")
	}

	files := int64(len(held))
	budgetPauses := int64(absentBudget / absentPause)
	if gatePauses.Load() == 0 {
		t.Fatal("the verification never met the leftovers: the test does not observe the wait")
	}
	if got := gateExpiries.Load(); got > files {
		t.Errorf("the verification ran out %d budgets for %d files, want at most one each", got, files)
	}
	if got := gatePauses.Load(); got > files*budgetPauses {
		t.Errorf("the verification paused %d times for %d files, want at most %d (one budget of %d for each file, not one for each read of it)", got, files, files*budgetPauses, budgetPauses)
	}
}

// lockedPauses counts the pauses of the read gate taken with the epoch lock held and without it.
type lockedPauses struct{ under, outside atomic.Int64 }

func countGatePausesAroundTheLock(t *testing.T, lock string) *lockedPauses {
	t.Helper()
	p := &lockedPauses{}
	old := gateSleep
	gateSleep = func(d time.Duration) {
		if exists(lock) {
			p.under.Add(1)
		} else {
			p.outside.Add(1)
		}
		old(d)
	}
	t.Cleanup(func() { gateSleep = old })
	return p
}

// TestTheProbeBeforeTheLockPaysTheGateWaitsSoTheLockIsHeldWithoutOne: what keeps the epoch lock short when a heartbeat has a fresh
// leftover beside it (a writer that died a moment ago). Every grant probes the leases without the lock first, over ALL leases and not
// only the ones that conflict, and a wait that ran out is remembered, so the reads under the lock find the answer waiting. The waits
// are paid outside the lock (the hold is then milliseconds, not 250 ms per file: the measurement is in the change fragment). A guard
// for that order: a grant that took its first look under the lock would hold it for a budget per file.
func TestTheProbeBeforeTheLockPaysTheGateWaitsSoTheLockIsHeldWithoutOne(t *testing.T) {
	t.Run("card grant, live leases on other cards", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		var held []*Lease
		for _, card := range []string{card0, card1, card2} {
			l := mustDevices(t, m, card)
			held = append(held, l)
			t.Cleanup(func() { _ = l.Release() })
		}
		for _, l := range held {
			stuckBeside(t, c, m.heartbeatPath(l.Epoch()))
		}
		p := countGatePausesAroundTheLock(t, m.epochLockPath())
		l4, err := m.TryAcquire(ClassMedia, Options{Reason: "card", Devices: []string{card3}, TTL: time.Hour})
		if err != nil {
			t.Fatalf("a grant of a free card = %v", err)
		}
		defer l4.Release()
		if p.outside.Load() == 0 {
			t.Fatal("the probe never met the leftovers: the test does not observe the wait")
		}
		if n := p.under.Load(); n != 0 {
			t.Errorf("%d gate pauses were taken with the epoch lock held (%d outside it), want none: the probe has paid", n, p.outside.Load())
		}
	})
	t.Run("whole-node grant over leases whose holders are gone", func(t *testing.T) {
		c := onGateClock(t)
		m := scopedRealClock(t)
		const deadA, deadB = 7001, 7002
		deadPIDs(t) // every pid reads alive while the leases are taken
		a := mustDevices(t, asPID(m, deadA), card0, card1)
		b := mustDevices(t, asPID(m, deadB), card2)
		deadPIDs(t, deadA, deadB)
		stuckBeside(t, c, m.heartbeatPath(a.Epoch()))
		stuckBeside(t, c, m.heartbeatPath(b.Epoch()))
		p := countGatePausesAroundTheLock(t, m.epochLockPath())
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
		if err != nil {
			t.Fatalf("a whole-node grant over reclaimable leases = %v", err)
		}
		defer l.Release()
		if p.outside.Load() == 0 {
			t.Fatal("the probe never met the leftovers: the test does not observe the wait")
		}
		if n := p.under.Load(); n != 0 {
			t.Errorf("%d gate pauses were taken with the epoch lock held (%d outside it), want none: the probe has paid", n, p.outside.Load())
		}
	})
}
