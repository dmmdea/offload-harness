package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The heartbeat file hb.<epoch> is the one thing that keeps a lease alive once its declared window is over: the reclaim rule needs a
// stale heartbeat AND an expired window, so after the window the heartbeat is the only protection. A holder rewrites it with a rename
// over the live name (Renew), and while that rename runs the system holds a handle with DELETE access on the old entry, exactly as it
// does for a lease record. A reader that opens the heartbeat without FILE_SHARE_DELETE (os.ReadFile) then fails with
// ERROR_SHARING_VIOLATION, and heartbeatAt answers with the stamp written at acquisition. For a lease that is past its window that
// reads as a stale heartbeat on a live holder: the lease is judged reclaimable, an acquirer's sweep removes it, and the card is
// granted to a second job while the first still runs on it.

// holdDeleteAccess opens path with DELETE access and a share mode of read and write only: the handle the system holds on the old
// entry while a rename replaces it. It returns the function that closes it.
func holdDeleteAccess(t *testing.T, path string) (closeHandle func()) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, windows.DELETE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("opening %s for delete access: %v", path, err)
	}
	closed := false
	closeHandle = func() {
		if !closed {
			closed = true
			_ = syscall.CloseHandle(h)
		}
	}
	t.Cleanup(closeHandle)
	return closeHandle
}

// pastItsWindow acquires a card lease with a window of 50 ms, moves the clock 10 minutes on (far past the window and past the
// heartbeat TTL since the acquisition stamp), and renews it, so that the heartbeat file holds the current time and the record's own
// stamp is stale. It returns the lease, the record, and the heartbeat's value in milliseconds.
func pastItsWindow(t *testing.T, m *Manager, now *time.Time) (*Lease, *Meta, int64) {
	t.Helper()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "long job", Devices: []string{card0}, TTL: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	if err := l.Renew(); err != nil {
		t.Fatalf("renewing the lease past its window: %v", err)
	}
	rec, err := readEpochRecord(m.leaseDir(), l.Epoch())
	if err != nil {
		t.Fatal(err)
	}
	if now.UnixMilli()-rec.RenewedAtMs <= m.heartbeatTTL.Milliseconds() || now.UnixMilli() <= rec.ExpiresAtMs {
		t.Fatalf("setup: the lease must be past its window with a stale acquisition stamp (now %d, renewed_at %d, expires_at %d)", now.UnixMilli(), rec.RenewedAtMs, rec.ExpiresAtMs)
	}
	return l, rec, now.UnixMilli()
}

// TestTheHeartbeatReadsWhileAnotherHandleHoldsDeleteAccessOnIt is the direct form: every reader of the heartbeat file must give the
// renewed stamp while a delete-access handle is open on it, not the stamp from the acquisition.
func TestTheHeartbeatReadsWhileAnotherHandleHoldsDeleteAccessOnIt(t *testing.T) {
	m, now := scopedManager(t)
	l, rec, want := pastItsWindow(t, m, now)
	defer l.Release()
	hb := m.heartbeatPath(l.Epoch())
	holdDeleteAccess(t, hb)

	if _, err := os.ReadFile(hb); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("control: os.ReadFile of the heartbeat with a delete-access handle open returned %v, want ERROR_SHARING_VIOLATION: the test no longer models the race", err)
	}
	if got := heartbeatAt(m.leaseDir(), rec); got != want {
		t.Errorf("heartbeatAt = %d, want the renewed stamp %d (the acquisition stamp is %d)", got, want, rec.RenewedAtMs)
	}
	if got := m.lastHeartbeat(rec); got != want {
		t.Errorf("lastHeartbeat = %d, want the renewed stamp %d (the acquisition stamp is %d)", got, want, rec.RenewedAtMs)
	}
	// keeperSignOfLife is given a record path that does not exist, so that the record's modification time does not stand in for the heartbeat.
	if got := m.keeperSignOfLife(l.Epoch(), filepath.Join(m.leaseDir(), "no-such-record.json")); got.UnixMilli() != want {
		t.Errorf("keeperSignOfLife = %d, want the renewed stamp %d", got.UnixMilli(), want)
	}
}

// TestALiveLeasePastItsWindowIsNotHandedToAnotherJobWhileItsHeartbeatIsBeingRenewed is the consequence: a lease past its window, whose
// holder is renewing, must stay held for a reader that meets the heartbeat mid-rename. An acquirer on the same card is refused, the
// lease is still listed, and the holder's next renewal still succeeds (it was not fenced out).
func TestALiveLeasePastItsWindowIsNotHandedToAnotherJobWhileItsHeartbeatIsBeingRenewed(t *testing.T) {
	m, now := scopedManager(t)
	l, _, _ := pastItsWindow(t, m, now)
	defer l.Release()
	closeHandle := holdDeleteAccess(t, m.heartbeatPath(l.Epoch()))

	if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != l.Epoch() {
		t.Errorf("Leases() = %v while the holder renews, want [%d]", got, l.Epoch())
	}
	intruder, err := asPID(m, os.Getpid()+1).TryAcquire(ClassMedia, Options{Reason: "second job", Devices: []string{card0}, TTL: time.Hour})
	if err == nil {
		_ = intruder.Release()
		t.Errorf("the card of a live lease was granted to a second job (epoch %d) because its heartbeat was read mid-renewal", intruder.Epoch())
	} else {
		var held *ErrHeld
		if !errors.As(err, &held) {
			t.Errorf("the second job was refused with %v, want *ErrHeld", err)
		}
	}

	closeHandle()
	if err := l.Renew(); err != nil {
		t.Errorf("the holder's next renewal after the read: %v", err)
	}
	if err := l.Check(); err != nil {
		t.Errorf("the holder was fenced out: %v", err)
	}
}
