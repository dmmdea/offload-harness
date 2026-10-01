package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Register C-59: a draining reserve "loses its epoch to a concurrent acquire".
//
// The reclaim path decides a claim is stale from one read, then removes the
// claim by PATH. Two acquirers that both read the same stale record (a holder
// that died without releasing) can interleave so that the slower one removes
// the claim the faster one has just created: the faster acquirer was granted
// the lease, its record is deleted before it has done anything with it, and a
// reserve that went on to drain under it found "the lease is gone" at its
// restamp, hours later. A removal must only ever delete the record it judged
// stale.

// writeStaleClaim leaves what a holder that died without releasing leaves: a
// claim for a pid that is gone and the fencing counter at its epoch.
func writeStaleClaim(t *testing.T, m *Manager, epoch uint64, deadPID int) {
	t.Helper()
	now := m.now()
	meta := Meta{
		Epoch:        epoch,
		Class:        ClassText,
		Holder:       Holder{PID: deadPID},
		Reason:       "died without releasing",
		AcquiredAtMs: now.Add(-time.Hour).UnixMilli(),
		ExpiresAtMs:  now.Add(time.Hour).UnixMilli(),
		RenewedAtMs:  now.Add(-time.Hour).UnixMilli(),
	}
	b, err := json.Marshal(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.metaPath(), b, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := m.writeEpoch(epoch); err != nil {
		t.Fatal(err)
	}
}

// TestASlowReclaimerNeverDeletesTheClaimTheFirstReclaimerMade interleaves the
// two acquirers deterministically: A reads the stale claim and is held at the
// moment it has decided to reclaim it; B then reclaims it AND takes the card;
// A resumes. A must find the card held — never remove B's live claim.
func TestASlowReclaimerNeverDeletesTheClaimTheFirstReclaimerMade(t *testing.T) {
	m, _ := newTestManager(t)
	const deadPID = 999999
	writeStaleClaim(t, m, 5, deadPID)

	var deadReads atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	orig := pidAliveFn
	// Only the dead holder reads as gone; everyone else is alive. A's FIRST
	// read of it is the cheap probe, its SECOND is the reclaim decision — that
	// one is held until B is done.
	pidAliveFn = func(pid int) bool {
		if pid != deadPID {
			return true
		}
		if deadReads.Add(1) == 2 {
			close(parked)
			<-release
		}
		return false
	}
	t.Cleanup(func() { pidAliveFn = orig })

	type result struct {
		lease *Lease
		err   error
	}
	aDone := make(chan result, 1)
	go func() {
		l, err := m.TryAcquire(ClassText, Options{Reason: "A", TTL: time.Hour})
		aDone <- result{l, err}
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("A never reached its reclaim decision")
	}

	b, berr := m.TryAcquire(ClassText, Options{Reason: "B", TTL: time.Hour})
	if berr != nil {
		close(release)
		t.Fatalf("B could not reclaim the stale claim: %v", berr)
	}
	close(release)
	var a result
	select {
	case a = <-aDone:
	case <-time.After(10 * time.Second):
		t.Fatal("A never returned")
	}

	if a.err == nil {
		t.Fatalf("A was granted the card while B held it: A's late removal deleted B's live claim (B epoch %d, A epoch %d, check: %v)", b.Epoch(), a.lease.Epoch(), b.Check())
	}
	var held *ErrHeld
	if !errors.As(a.err, &held) {
		t.Fatalf("A must be refused as held by B, got %v", a.err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("B's lease did not survive A's reclaim attempt: %v", err)
	}
	if info := m.Inspect(); !info.Held || info.Epoch != b.Epoch() || info.Reason != "B" {
		t.Fatalf("the card must still be B's: %+v", info)
	}
}

// A reclaimer that decided the claim was stale from a first read must look
// again before it removes anything: a holder that has come back to life in
// between (its heartbeat moved, its pid is a live process again) keeps its
// claim.
func TestAReclaimerStandsDownWhenTheHolderComesBackBeforeItRemovesTheClaim(t *testing.T) {
	m, _ := newTestManager(t)
	const deadPID = 999999
	writeStaleClaim(t, m, 5, deadPID)
	var reads atomic.Int64
	orig := pidAliveFn
	// The cheap probe and the reclaim decision (the first two reads) see a dead
	// holder; every read after that sees it alive.
	pidAliveFn = func(pid int) bool {
		if pid != deadPID {
			return true
		}
		return reads.Add(1) > 2
	}
	t.Cleanup(func() { pidAliveFn = orig })
	if l, err := m.TryAcquire(ClassText, Options{Reason: "reclaimer", TTL: time.Hour}); err == nil {
		t.Fatalf("a holder that came back to life lost its claim to a reclaimer (new epoch %d)", l.Epoch())
	} else if !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("the reclaimer must be refused as held, got %v", err)
	}
	if meta, err := m.readMeta(); err != nil || meta.Epoch != 5 {
		t.Fatalf("the returning holder's claim must be untouched: %+v %v", meta, err)
	}
}

// A claim nobody can parse is debris once it is older than the claim grace —
// the stale-claim sweep must still clear it.
func TestAnOldUnreadableClaimIsStillReclaimedAsDebris(t *testing.T) {
	m, now := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	writeUnreadableClaim(t, m, now.Add(-time.Hour))
	l, err := m.TryAcquire(ClassText, Options{Reason: "after debris", TTL: time.Hour})
	if err != nil {
		t.Fatalf("an old unreadable claim must be reclaimed: %v", err)
	}
	defer func() { _ = l.Release() }()
	if info := m.Inspect(); !info.Held || info.Reason != "after debris" {
		t.Fatalf("the card must be the new holder's: %+v", info)
	}
}

// A claim caught unreadable a moment after it was created is a claim in
// progress, not debris: it is held, never removed.
func TestAFreshUnreadableClaimIsAClaimInProgress(t *testing.T) {
	m, now := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	writeUnreadableClaim(t, m, *now)
	if _, err := m.TryAcquire(ClassText, Options{Reason: "impatient", TTL: time.Hour}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a fresh unreadable claim must read as held, got %v", err)
	}
	if _, err := os.Stat(m.metaPath()); err != nil {
		t.Fatalf("a claim in progress must not be removed: %v", err)
	}
}

// The reclaimer judged the claim unreadable and old, but by the time it takes
// the epoch lock the claim reads fine — it was a transient failure to read a
// LIVE record (a rewrite in flight). Removing it then would delete a lease the
// holder still has.
func TestAClaimThatTurnsReadableAfterBeingJudgedDebrisIsNotRemoved(t *testing.T) {
	m, now := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	writeUnreadableClaim(t, m, now.Add(-time.Hour))
	if err := m.writeEpoch(5); err != nil {
		t.Fatal(err)
	}
	live := Meta{
		Epoch: 6, Class: ClassText, Holder: Holder{PID: os.Getpid(), StartTimeMs: 4242}, Reason: "live",
		AcquiredAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(time.Hour).UnixMilli(), RenewedAtMs: now.UnixMilli(),
	}
	liveJSON, err := json.Marshal(&live)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the reclaimer after it has read the claim as unreadable and old, at
	// the clock read inside claimIsFresh — the SECOND read of the clock on this
	// path (the first stamps the record it would have written).
	base := *now
	var clockReads atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	m.now = func() time.Time {
		if clockReads.Add(1) == 2 {
			close(parked)
			<-release
		}
		return base
	}
	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		l, err := m.TryAcquire(ClassText, Options{Reason: "reclaimer", TTL: time.Hour})
		done <- result{l, err}
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never reached its judgement")
	}
	if err := os.WriteFile(m.metaPath(), liveJSON, 0o666); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never returned")
	}
	if r.err == nil {
		t.Fatalf("a live claim was removed as debris and replaced (new epoch %d)", r.lease.Epoch())
	}
	if !errors.As(r.err, new(*ErrHeld)) {
		t.Fatalf("the reclaimer must be refused as held, got %v", r.err)
	}
	if meta, err := m.readMeta(); err != nil || meta.Epoch != 6 || meta.Reason != "live" {
		t.Fatalf("the live claim must be untouched: %+v %v", meta, err)
	}
}

// writeUnreadableClaim leaves a claim file nobody can parse, last written at
// modTime.
func writeUnreadableClaim(t *testing.T, m *Manager, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(m.metaPath(), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(m.metaPath(), modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

// A claim the reclaimer PARSED and judged stale, caught unreadable by the time
// it comes to remove it, is a record that failed to read — a rewrite in flight —
// not debris. It is left for the next look, never removed on that one failure.
func TestAClaimThatTurnsUnreadableAfterBeingJudgedStaleIsLeftForTheNextLook(t *testing.T) {
	m, _ := newTestManager(t)
	const deadPID = 999999
	writeStaleClaim(t, m, 5, deadPID)
	var deadReads atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	orig := pidAliveFn
	pidAliveFn = func(pid int) bool {
		if pid == deadPID && deadReads.Add(1) == 2 { // the reclaim decision
			close(parked)
			<-release
		}
		return pid != deadPID
	}
	t.Cleanup(func() { pidAliveFn = orig })
	done := make(chan error, 1)
	go func() {
		_, err := m.TryAcquire(ClassText, Options{Reason: "reclaimer", TTL: time.Hour})
		done <- err
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never reached its decision")
	}
	if err := os.WriteFile(m.metaPath(), []byte("{"), 0o666); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never returned")
	}
	if !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a claim that failed to read once must read as held (in progress), got %v", err)
	}
	if b, rerr := os.ReadFile(m.metaPath()); rerr != nil || string(b) != "{" {
		t.Fatalf("the claim must not have been removed on one failed read: %q %v", b, rerr)
	}
}

// The reclaimer judged an old unreadable claim debris, but by the time it takes
// the epoch lock a rival has already removed that debris and CREATED its own
// claim, which is still empty because the rival has not written the record yet.
// An empty claim that new is a claim in progress, not debris: removing it would
// delete a lease the rival is in the middle of being granted.
func TestAClaimBeingWrittenIsNotRemovedAsDebris(t *testing.T) {
	m, now := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	writeUnreadableClaim(t, m, now.Add(-time.Hour))
	if err := m.writeEpoch(5); err != nil {
		t.Fatal(err)
	}
	// Hold the reclaimer at the clock read inside claimIsFresh (the second read
	// on this path), after it has judged the claim unreadable and old.
	base := *now
	var clockReads atomic.Int64
	parked := make(chan struct{})
	release := make(chan struct{})
	m.now = func() time.Time {
		if clockReads.Add(1) == 2 {
			close(parked)
			<-release
		}
		return base
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.TryAcquire(ClassText, Options{Reason: "reclaimer", TTL: time.Hour})
		done <- err
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never reached its judgement")
	}
	// The rival: removes the debris and creates its claim, not yet written.
	if err := os.Remove(m.metaPath()); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := os.WriteFile(m.metaPath(), nil, 0o666); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimer never returned")
	}
	if !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a claim being written must read as held (in progress), got %v", err)
	}
	if fi, serr := os.Stat(m.metaPath()); serr != nil || fi.Size() != 0 {
		t.Fatalf("the rival's claim must be untouched: %v %v", fi, serr)
	}
}
