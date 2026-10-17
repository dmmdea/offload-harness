package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
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
