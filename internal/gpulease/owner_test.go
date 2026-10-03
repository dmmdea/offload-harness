package gpulease

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcs swaps the process table for a deterministic one: the pids in alive are live,
// every other pid is dead. The start identity every live pid reports is the one
// newTestManager's procStart returns (4242), so a record carrying another start reads as
// a recycled pid.
func fakeProcs(t *testing.T, alive ...int) func(pid int, up bool) {
	t.Helper()
	var mu sync.Mutex
	live := map[int]bool{os.Getpid(): true}
	for _, p := range alive {
		live[p] = true
	}
	old := pidAliveFn
	pidAliveFn = func(pid int) bool {
		mu.Lock()
		defer mu.Unlock()
		return live[pid]
	}
	t.Cleanup(func() { pidAliveFn = old })
	return func(pid int, up bool) {
		mu.Lock()
		defer mu.Unlock()
		live[pid] = up
	}
}

func orphanMarks(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), orphanMarkPrefix) && !strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestOwnerStampedFromFlags(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	// Flags beat the environment, and a process named by flags is recorded with its start
	// identity (a launcher that detaches before the lease is taken has to name its owner).
	o := ResolveOwner("sess-flag", 4321, 99, false, "sess-env")
	if o.Session != "sess-flag" || o.PID != 4321 || o.StartMs != 99 || o.Remote {
		t.Fatalf("flags must win over the environment: %+v", o)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "r", TTL: time.Hour, Owner: o})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	if info.Owner == nil || info.Owner.Session != "sess-flag" || info.Owner.PID != 4321 || info.Owner.StartMs != 99 {
		t.Fatalf("the owner must be on the lease record: %+v", info.Owner)
	}
	if info.Unattended {
		t.Fatal("a lease is attended unless it says otherwise")
	}
	// A pid named without its start identity gets the one the process table reports now.
	l2o := ResolveOwner("", 4322, 0, false, "")
	if l2o.StartMs != 0 {
		t.Fatalf("ResolveOwner must not invent a start identity: %+v", l2o)
	}
}

func TestOwnerStampedFromEnvSessionID(t *testing.T) {
	// The environment supplies a SESSION only: a pid taken from the process a wrapper
	// happens to be run from is a short-lived shell, and recording it would read every
	// lease as abandoned when the shell exits.
	o := ResolveOwner("", 0, 0, false, "  sess-env  ")
	if o.Session != "sess-env" || o.PID != 0 || o.StartMs != 0 {
		t.Fatalf("env session only, no pid: %+v", o)
	}
	if o := ResolveOwner("", 0, 0, false, ""); !o.IsZero() {
		t.Fatalf("no flags and no environment is an unknown owner: %+v", o)
	}
	// A remote owner is not named by this host's environment.
	if o := ResolveOwner("", 0, 0, true, "sess-env"); o.Session != "" || !o.Remote {
		t.Fatalf("a remote owner must not inherit this host's session: %+v", o)
	}
	// A start identity without a pid names nothing.
	if o := ResolveOwner("s", 0, 77, false, ""); o.StartMs != 0 {
		t.Fatalf("a start identity without a pid is dropped: %+v", o)
	}
}

func TestLegacyMetaReadsOwnerUnknown(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	// A pre-P8 record: no owner, no unattended, no progress.
	legacy := `{"epoch":7,"class":"media","holder":{"pid":` + strconv.Itoa(os.Getpid()) + `,"start_time_ms":4242},` +
		`"acquired_at_ms":` + strconv.FormatInt(now.UnixMilli(), 10) + `,"expires_at_ms":` + strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10) +
		`,"renewed_at_ms":` + strconv.FormatInt(now.UnixMilli(), 10) + `}`
	if err := os.WriteFile(m.metaPath(), []byte(legacy), 0o666); err != nil {
		t.Fatal(err)
	}
	info := m.Inspect()
	if !info.Held || info.Epoch != 7 {
		t.Fatalf("the legacy record must read as held: %+v", info)
	}
	if info.Owner != nil || info.Unattended || info.Progress != nil {
		t.Fatalf("a legacy record names no owner: %+v", info)
	}
	st := m.Standing(info, 0)
	if st.Owner != OwnerUnknown || st.Orphaned || st.Overdue || st.Progress.Declared {
		t.Fatalf("a legacy lease is an unknown owner and is never orphaned: %+v", st)
	}
	if len(orphanMarks(t, m)) != 0 {
		t.Fatal("an unknown owner must never collect an orphan marker")
	}
}

func TestOwnerGoneWithinGraceIsAlive(t *testing.T) {
	fakeProcs(t) // pid 777 is dead
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "s1", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	st := m.Standing(m.Inspect(), 15*time.Minute)
	if st.Owner != OwnerGone {
		t.Fatalf("a dead owner process reads as gone: %+v", st)
	}
	if st.Orphaned {
		t.Fatalf("an owner seen gone a moment ago is still inside the grace: %+v", st)
	}
	*now = now.Add(14 * time.Minute)
	_ = l.Renew()
	if st := m.Standing(m.Inspect(), 15*time.Minute); st.Orphaned {
		t.Fatalf("14 minutes of a 15 minute grace is not orphaned: %+v", st)
	}
	*now = now.Add(2 * time.Minute)
	_ = l.Renew()
	st = m.Standing(m.Inspect(), 15*time.Minute)
	if !st.Orphaned || st.OrphanedSince.IsZero() {
		t.Fatalf("16 minutes gone is past the grace: %+v", st)
	}
	// The default grace is 15 minutes.
	if DefaultOrphanGrace != 15*time.Minute {
		t.Fatalf("DefaultOrphanGrace = %s", DefaultOrphanGrace)
	}
}

func TestOrphanMarkerStampedByFirstObserverUnderLock(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()

	// Hold the epoch lock: the first observer must wait for it, not write around it.
	if err := os.WriteFile(m.epochLockPath(), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	first := *now
	done := make(chan time.Time, 1)
	go func() {
		_, since := m.ObserveOwner(info)
		done <- since
	}()
	time.Sleep(150 * time.Millisecond)
	if n := len(orphanMarks(t, m)); n != 0 {
		t.Fatalf("a marker was written while the epoch lock was held by someone else (%d)", n)
	}
	if err := os.Remove(m.epochLockPath()); err != nil {
		t.Fatal(err)
	}
	select {
	case since := <-done:
		if !since.Equal(first) {
			t.Fatalf("the first observer stamps its own clock: got %v want %v", since, first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the observer never finished once the lock was free")
	}
	marks := orphanMarks(t, m)
	if len(marks) != 1 || marks[0] != orphanMarkPrefix+strconv.FormatUint(info.Epoch, 10) {
		t.Fatalf("exactly one marker named for the epoch: %v", marks)
	}
	// A LATER observer reads the first observer's moment, it does not start its own clock.
	*now = now.Add(7 * time.Minute)
	_, since := m.ObserveOwner(info)
	if !since.Equal(first) {
		t.Fatalf("a later observer must see the first moment %v, got %v", first, since)
	}
}

func TestOrphanMarkerClearedOnReappear(t *testing.T) {
	setAlive := fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	m.ObserveOwner(info)
	if len(orphanMarks(t, m)) != 1 {
		t.Fatal("the marker must be stamped while the owner is gone")
	}
	setAlive(777, true) // the owner is back
	if state, since := m.ObserveOwner(info); state != OwnerAlive || !since.IsZero() {
		t.Fatalf("an owner that is back has no orphaned-since: %v %v", state, since)
	}
	if n := len(orphanMarks(t, m)); n != 0 {
		t.Fatalf("the marker must be cleared when the owner reappears: %d left", n)
	}
	// Gone again later: the grace starts afresh from the new first observation.
	*now = now.Add(time.Hour)
	setAlive(777, false)
	_, since := m.ObserveOwner(info)
	if !since.Equal(*now) {
		t.Fatalf("a second disappearance restarts the clock: got %v want %v", since, *now)
	}
}

func TestConcurrentReadersWriteOneOrphanMarker(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()

	const readers = 16
	got := make([]time.Time, readers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rm := *m // an independent reader with its own clock: if each stamped its own moment they would all differ
			at := now.Add(time.Duration(i) * time.Second)
			rm.now = func() time.Time { return at }
			<-start
			_, got[i] = rm.ObserveOwner(info)
		}(i)
	}
	close(start)
	wg.Wait()
	marks := orphanMarks(t, m)
	if len(marks) != 1 {
		t.Fatalf("concurrent readers must write ONE marker, found %v", marks)
	}
	want, ok := m.readOrphanMark(info.Epoch)
	if !ok {
		t.Fatal("the marker is unreadable")
	}
	for i, g := range got {
		if !g.Equal(want) {
			t.Fatalf("reader %d saw %v, the marker says %v: readers disagree about when the owner went", i, g, want)
		}
	}
}

func TestResumedSessionNewPidIsNotOrphaned(t *testing.T) {
	setAlive := fakeProcs(t, 100) // the first server (100) is up when the lease is taken
	m, _ := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	// The first server registered, the lease was taken (tracked), then the server died and
	// the session resumed in a new process under the SAME id.
	unreg100, err := registerOwner(m.leaseDir(), "sess-R", 100, func(int) (int64, bool) { return 4242, true })
	if err != nil {
		t.Fatal(err)
	}
	defer unreg100()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "sess-R", PID: 100, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	if info.Owner == nil || !info.Owner.Tracked {
		t.Fatalf("a session in the registry when the lease is taken is recorded as tracked: %+v", info.Owner)
	}
	setAlive(100, false) // the first server dies; the session has not resumed yet
	if st := m.Standing(info, 0); st.Owner != OwnerGone {
		t.Fatalf("with the old server dead and no new one the session is gone: %+v", st)
	}
	setAlive(200, true) // the resumed session's new server
	unreg200, err := registerOwner(m.leaseDir(), "sess-R", 200, func(int) (int64, bool) { return 4242, true })
	if err != nil {
		t.Fatal(err)
	}
	defer unreg200()
	st := m.Standing(info, time.Nanosecond)
	if st.Owner != OwnerAlive || st.Orphaned || len(orphanMarks(t, m)) != 0 {
		t.Fatalf("a resumed session under a new pid is the same owner: %+v marks=%v", st, orphanMarks(t, m))
	}
}

func TestOwnerPidRecycledCountsAsGone(t *testing.T) {
	fakeProcs(t, 500) // pid 500 is alive, but it is another process now: it started at 4242, the owner at 111
	m, _ := newTestManager(t)
	o := &Owner{PID: 500, StartMs: 111}
	if got := m.OwnerState(o); got != OwnerGone {
		t.Fatalf("a recycled pid is a gone owner, got %v", got)
	}
	// The same pid with the start identity it really has is the owner.
	if got := m.OwnerState(&Owner{PID: 500, StartMs: 4242}); got != OwnerAlive {
		t.Fatalf("the real owner process is alive, got %v", got)
	}
	// With no start identity recorded, a live pid is trusted (no evidence of recycling).
	if got := m.OwnerState(&Owner{PID: 500}); got != OwnerAlive {
		t.Fatalf("got %v", got)
	}
}

func TestUnknownOwnerNeverOrphanedButOverdueEligible(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "legacy-shaped", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	*now = now.Add(26 * time.Hour)
	if err := l.Renew(); err != nil { // the holder is alive and heartbeating
		t.Fatal(err)
	}
	info := m.Inspect()
	if !info.Held {
		t.Fatal("a live, heartbeating holder keeps its lease past its window")
	}
	st := m.Standing(info, time.Nanosecond)
	if st.Owner != OwnerUnknown || st.Orphaned {
		t.Fatalf("an unknown owner is never orphaned: %+v", st)
	}
	if !st.Overdue || st.OverdueBy < 24*time.Hour {
		t.Fatalf("an unknown-owner lease is overdue by its declared window: %+v", st)
	}
	if len(orphanMarks(t, m)) != 0 {
		t.Fatal("no marker for an unknown owner")
	}
}

func TestUntrackedSessionIsUnknownNotGone(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	// A session id with no registry entry ever and no pid (an MCP server that predates the
	// registry): nothing can be said about it.
	if got := m.OwnerState(&Owner{Session: "never-registered"}); got != OwnerUnknown {
		t.Fatalf("got %v", got)
	}
	// The same session recorded as tracked at the time the lease was taken, and since
	// gone from the registry (it exited): gone.
	if got := m.OwnerState(&Owner{Session: "was-registered", Tracked: true}); got != OwnerGone {
		t.Fatalf("got %v", got)
	}
}

func TestRemoteOwnerIsUnattendedAndNeverOrphaned(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "dispatch", TTL: time.Hour, Owner: Owner{Remote: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	if !info.Unattended {
		t.Fatal("a remote owner implies an unattended lease")
	}
	*now = now.Add(time.Hour / 2)
	st := m.Standing(info, time.Nanosecond)
	if st.Owner != OwnerRemote || !st.Unattended || st.Orphaned {
		t.Fatalf("a remote owner is judged by its contract, never orphaned: %+v", st)
	}
}

func TestUnattendedNeedsTheBoundedClaimContract(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	for name, opts := range map[string]Options{
		"no window, no progress":    {Unattended: true},
		"window, no progress":       {Unattended: true, TTL: time.Hour},
		"progress, no window":       {Unattended: true, ProgressFile: prog, Stall: time.Hour},
		"file without stall window": {ProgressFile: prog},
		"stall window without file": {Stall: time.Hour},
	} {
		if _, err := m.TryAcquire(ClassMedia, opts); err == nil {
			t.Errorf("%s: acquisition must be refused", name)
		}
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Unattended: true, TTL: time.Hour}); !errors.Is(err, ErrUnattendedContract) {
		t.Fatalf("want ErrUnattendedContract, got %v", err)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: 2 * time.Hour,
		YieldGrace: 5 * time.Minute, OnYield: "touch STOP"})
	if err != nil {
		t.Fatalf("a complete contract is accepted: %v", err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	if !info.Unattended || info.Progress == nil || info.Progress.File != prog || info.Progress.StallMs != (2*time.Hour).Milliseconds() ||
		info.YieldGraceMs != (5*time.Minute).Milliseconds() || info.OnYield != "touch STOP" {
		t.Fatalf("the contract must be on the record: %+v", info)
	}
}

func TestUnattendedStalledWhenProgressFileDoesNotAdvance(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 3 of 17\"}\n{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	// 90 minutes in, the file last moved 10 minutes ago: advancing, with its last line.
	*now = now.Add(90 * time.Minute)
	_ = l.Renew()
	if err := os.Chtimes(prog, now.Add(-10*time.Minute), now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st := m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressAdvancing || st.Stalled || st.Progress.Detail != "clip 4 of 17" || st.Progress.Age != 10*time.Minute {
		t.Fatalf("a file that moved inside its window is advancing: %+v", st.Progress)
	}
	// Three hours in, the file last moved 2h30 ago: past the 2h window, stalled.
	*now = now.Add(90 * time.Minute)
	_ = l.Renew()
	if err := os.Chtimes(prog, now.Add(-150*time.Minute), now.Add(-150*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st = m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressStalled || !st.Stalled || st.Progress.Age != 150*time.Minute {
		t.Fatalf("a file that did not move inside its window is stalled: %+v", st.Progress)
	}
	if st.Orphaned {
		t.Fatal("an unattended lease is never orphaned: its progress contract is what judges it")
	}
}

func TestMissingProgressFileIsUnknownNotStalled(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "not-written-yet.jsonl")
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	*now = now.Add(5 * time.Hour)
	_ = l.Renew()
	st := m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressUnknown || st.Stalled || !st.Progress.Declared {
		t.Fatalf("a missing file is unknown, never a stall: %+v", st.Progress)
	}
}

func TestStaleProgressFileFromAnEarlierRunDoesNotStallAFreshLease(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("old run\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(prog, now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	*now = now.Add(10 * time.Minute)
	_ = l.Renew()
	if st := m.Standing(m.Inspect(), 0); st.Stalled || st.Progress.Age != 10*time.Minute {
		t.Fatalf("the stall window counts from the lease's start at the earliest: %+v", st.Progress)
	}
}

func TestReleaseClearsTheOrphanMarker(t *testing.T) {
	fakeProcs(t)
	for _, scoped := range []bool{false, true} {
		m, _ := newTestManager(t)
		opts := Options{Reason: "film", TTL: time.Hour, Owner: Owner{PID: 777, StartMs: 4242}}
		if scoped {
			m.SetCardScoped(true)
			opts.Devices = []string{"gpu-test-1"}
		}
		l, err := m.TryAcquire(ClassMedia, opts)
		if err != nil {
			t.Fatal(err)
		}
		m.ObserveOwner(m.Inspect())
		if len(orphanMarks(t, m)) != 1 {
			t.Fatalf("scoped=%v: marker not stamped", scoped)
		}
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}
		if n := orphanMarks(t, m); len(n) != 0 {
			t.Fatalf("scoped=%v: a released lease left its orphan marker behind: %v", scoped, n)
		}
	}
}

func TestSessionRegistryRoundTrip(t *testing.T) {
	fakeProcs(t, 10, 11)
	m, _ := newTestManager(t)
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	ps := func(int) (int64, bool) { return 4242, true }
	// An empty session registers nothing.
	un, err := registerOwner(m.leaseDir(), "", 10, ps)
	if err != nil || len(readRegs(m.ownersDir(), "")) != 0 {
		t.Fatalf("empty session must register nothing: %v", err)
	}
	un()
	// A session id is not a path: nothing it contains can name a file elsewhere.
	hostile := `../../x\y:z.w`
	unH, err := registerOwner(m.leaseDir(), hostile, 10, ps)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(m.ownersDir())
	if len(entries) != 1 || strings.ContainsAny(entries[0].Name(), `/\:`) || strings.Count(entries[0].Name(), "..") > 0 {
		t.Fatalf("an unsafe session id must be encoded in the file name: %v", entries)
	}
	if regs := readRegs(m.ownersDir(), hostile); len(regs) != 1 || regs[0].Session != hostile {
		t.Fatalf("round trip: %+v", regs)
	}
	unH()

	// Two processes under one session coexist; removing one leaves the other.
	u10, _ := registerOwner(m.leaseDir(), "S", 10, ps)
	u11, _ := registerOwner(m.leaseDir(), "S", 11, ps)
	if regs := readRegs(m.ownersDir(), "S"); len(regs) != 2 {
		t.Fatalf("two processes, one session: %+v", regs)
	}
	u10()
	regs := readRegs(m.ownersDir(), "S")
	if len(regs) != 1 || regs[0].PID != 11 {
		t.Fatalf("unregister removes only its own entry: %+v", regs)
	}
	// Registering sweeps entries of processes that are gone: pid 12 is not alive.
	if _, err := registerOwner(m.leaseDir(), "T", 12, ps); err != nil {
		t.Fatal(err)
	}
	u13, _ := registerOwner(m.leaseDir(), "U", 11, ps) // sweeps pid 12's entry (dead), keeps 11's
	defer u13()
	if regs := readRegs(m.ownersDir(), "T"); len(regs) != 0 {
		t.Fatalf("a dead process's entry must be swept on the next registration: %+v", regs)
	}
	if regs := readRegs(m.ownersDir(), "S"); len(regs) != 1 {
		t.Fatalf("a live process's entry must survive the sweep: %+v", regs)
	}
	u11()
	// The file is plain JSON a person can read.
	un2, _ := registerOwner(m.leaseDir(), "V", 10, ps)
	defer un2()
	b, _ := os.ReadFile(filepath.Join(m.ownersDir(), "V.10.json"))
	var r registration
	if json.Unmarshal(b, &r) != nil || r.Session != "V" || r.PID != 10 || r.StartMs != 4242 {
		t.Fatalf("registry file content: %s", b)
	}
}

// With card-scoped leases several are live at once. The standing a node publishes is the
// worst of them: a stalled lease must not hide behind a healthy sibling that happens to
// have the lower epoch.
func TestStandingAllReportsTheWorstLeaseNotTheLowestEpoch(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	m.SetCardScoped(true)
	healthy, err := m.TryAcquire(ClassMedia, Options{Reason: "past its window", TTL: 2 * time.Hour, Devices: []string{"gpu-test-0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = healthy.Release() }()
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	_ = os.WriteFile(prog, []byte("x\n"), 0o666)
	stalled, err := m.TryAcquire(ClassMedia, Options{Reason: "stalled", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: time.Hour, Devices: []string{"gpu-test-1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Release() }()
	*now = now.Add(3 * time.Hour)
	_ = healthy.Renew()
	_ = stalled.Renew()
	_ = os.Chtimes(prog, now.Add(-150*time.Minute), now.Add(-150*time.Minute)) // last moved 2h30m ago, window 1h
	info := m.Inspect()
	if len(info.Each()) != 2 || info.Epoch != healthy.Epoch() {
		t.Fatalf("the healthy lease has the lower epoch and is the one Info describes: %+v", info.Epoch)
	}
	if st := m.Standing(info, 0); st.Stalled || !st.Overdue {
		t.Fatalf("the lowest-epoch lease alone is overdue and not stalled: that is the bug StandingAll exists for: %+v", st)
	}
	all := m.StandingAll(info, 0)
	if !all.Stalled || all.Epoch != stalled.Epoch() {
		t.Fatalf("StandingAll must surface the stalled sibling: %+v", all)
	}
	if all.Orphaned || !all.Overdue {
		t.Fatalf("each flag is true if true of ANY lease (the sibling is overdue), and only then: %+v", all)
	}
}

// A relative progress file is a path against the READING process's working directory, and
// every reader that is not the job's own shell (the MCP server, the fleet node, another
// session's `gpu status`) has a different one: it would stat a file that does not exist and
// report progress unknown for a job that stalled hours ago. The record carries an absolute
// path or the acquisition is refused, so the contract can never be silently inert.
func TestRelativeProgressFileIsRefused(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"unattended", Options{Unattended: true, TTL: 20 * time.Hour, ProgressFile: filepath.Join("work", "log.jsonl"), Stall: 2 * time.Hour}},
		{"attended", Options{TTL: time.Hour, ProgressFile: "log.jsonl", Stall: time.Hour}},
	} {
		_, err := m.TryAcquire(ClassMedia, tc.opts)
		if err == nil {
			t.Errorf("%s: a relative progress file must refuse the acquisition (a reader in another directory cannot find it)", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("%s: the refusal must say the path has to be absolute: %v", tc.name, err)
		}
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("a refused acquisition leaves nothing behind: %+v", info)
	}
}

// --on-yield is a command the takeover runs later, from some other directory: the lease
// records where the job was started so a relative command ("touch work/STOP") still means
// the same file then.
func TestOnYieldDirIsRecordedWithTheOnYieldCommand(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	dir := t.TempDir()
	t.Chdir(dir)
	prog := filepath.Join(dir, "log.jsonl")
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: 2 * time.Hour, OnYield: "touch work/STOP"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	want, _ := os.Stat(dir)
	got, serr := os.Stat(info.OnYieldDir)
	if info.OnYieldDir == "" || serr != nil || !os.SameFile(want, got) {
		t.Fatalf("the lease must record the directory the on-yield command is relative to: %q (want %q)", info.OnYieldDir, dir)
	}
}

// A lease with no on-yield command has nothing to be relative to and records no directory.
func TestNoOnYieldRecordsNoDirectory(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if info := m.Inspect(); info.OnYieldDir != "" || info.OnYield != "" {
		t.Fatalf("no on-yield command, no recorded directory: %+v", info)
	}
}

// The on-yield command is recorded VERBATIM: the takeover will run it, and a command
// silently cut to the display length (ending in an ellipsis) is a corrupted command. One that
// is too long to record faithfully is refused, loudly, at acquisition.
func TestOnYieldIsStoredVerbatimOrRefused(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	long := "touch " + strings.Repeat("a", 400) + "/STOP" // past the display clip, inside the cap
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, ProgressFile: prog, Stall: time.Hour, OnYield: long})
	if err != nil {
		t.Fatalf("a long but recordable command is accepted: %v", err)
	}
	if got := m.Inspect().OnYield; got != long {
		t.Fatalf("the command must be recorded verbatim, not clipped:\n got %q\nwant %q", got, long)
	}
	_ = l.Release()
	tooLong := "touch " + strings.Repeat("b", MaxOnYieldBytes)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, OnYield: tooLong}); err == nil || !strings.Contains(err.Error(), "on-yield") {
		t.Fatalf("a command past the cap is refused, naming --on-yield: %v", err)
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("a refused acquisition leaves nothing behind: %+v", info)
	}
}

// A contract that can never be recorded must be refused BEFORE the caller queues for the
// card: waiting eight hours to be told the path was relative is a hang, not a refusal.
func TestBadOwnershipContractIsRefusedBeforeQueueing(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "other", TTL: 8 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	var slept int
	m.sleep = func(d time.Duration) { slept++; *now = now.Add(d) }
	m.pollEvery = time.Second
	_, err = m.Acquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Wait: time.Hour, ProgressFile: "rel.jsonl", Stall: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative progress file must be refused at once, not after the wait: %v", err)
	}
	if slept != 0 {
		t.Fatalf("the refusal must come before any queueing; it waited %d polls", slept)
	}
	if ws := m.Waiters(); len(ws) != 0 {
		t.Fatalf("a refused request must not leave a place in the line: %+v", ws)
	}
}

// A registry that cannot be READ is not a registry with nobody in it. A tracked owner whose
// registry entries are all gone is gone; one whose registry could not be listed at all
// (a wrong mount, a permission error) is UNKNOWN, and the reading says why. Reading the
// failure as "gone" stamped the orphan marker and would, after the grace, have labelled a
// healthy attended lease ORPHANED.
func TestUnreadableRegistryIsUnknownNotGone(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "owners", Err: fs.ErrPermission}
	for _, tc := range []struct {
		name string
		fail func(m *Manager)
	}{
		{"the directory cannot be listed", func(m *Manager) {
			old := readRegistryDir
			readRegistryDir = func(string) ([]os.DirEntry, error) { return nil, denied }
			t.Cleanup(func() { readRegistryDir = old })
		}},
		{"an entry cannot be read", func(m *Manager) {
			// A live session's entry exists (another session's process) but cannot be read.
			if _, err := registerOwner(m.leaseDir(), "sess-A", 4321, func(int) (int64, bool) { return 4242, true }); err != nil {
				t.Fatal(err)
			}
			old := readRegistryFile
			readRegistryFile = func(string) ([]byte, error) { return nil, denied }
			t.Cleanup(func() { readRegistryFile = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeProcs(t)
			m, now := newTestManager(t)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "sess-A", Tracked: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Release() }()
			tc.fail(m)
			if got := m.OwnerState(m.Inspect().Owner); got != OwnerUnknown {
				t.Fatalf("an unreadable registry cannot say the owner is gone: %v", got)
			}
			*now = now.Add(2 * time.Hour)
			_ = l.Renew()
			st := m.Standing(m.Inspect(), 0)
			if st.Owner != OwnerUnknown || st.Orphaned || !st.OrphanedSince.IsZero() {
				t.Fatalf("an unreadable registry never makes a lease orphaned: %+v", st)
			}
			if !strings.Contains(st.OwnerNote, "registry") {
				t.Fatalf("the reading must say the registry could not be read: %q", st.OwnerNote)
			}
			if marks := orphanMarks(t, m); len(marks) != 0 {
				t.Fatalf("no marker may be stamped on an unreadable registry: %v", marks)
			}
		})
	}
}

// A recorded process that is alive is positive evidence and stands even when the registry
// cannot be read; and a registry that is simply absent is still "nobody in it".
func TestRegistryFailureDoesNotOverrideAProcessThatIsAlive(t *testing.T) {
	fakeProcs(t, 777)
	m, _ := newTestManager(t)
	old := readRegistryDir
	readRegistryDir = func(string) ([]os.DirEntry, error) { return nil, &fs.PathError{Op: "open", Err: fs.ErrPermission} }
	t.Cleanup(func() { readRegistryDir = old })
	if got := m.OwnerState(&Owner{Session: "s", PID: 777, StartMs: 4242, Tracked: true}); got != OwnerAlive {
		t.Fatalf("a live recorded pid is alive whatever the registry says: %v", got)
	}
	readRegistryDir = old
	if got := m.OwnerState(&Owner{Session: "s", Tracked: true}); got != OwnerGone {
		t.Fatalf("an absent registry directory is an empty registry: a tracked owner is gone, got %v", got)
	}
}

// A marker that cannot be written must say so. The old code dropped every error on the
// way, so a reader that could not record the moment reported "gone for 0s (inside the
// grace)" on every call and could never reach orphaned, with no hint why.
func TestOrphanMarkerThatCannotBeWrittenIsReportedNotSwallowed(t *testing.T) {
	fakeProcs(t) // pid 777 is dead
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "s", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	// Something occupies the marker's name, so the write cannot land.
	if err := os.MkdirAll(filepath.Join(m.orphanMarkPath(info.Epoch), "occupied"), 0o777); err != nil {
		t.Fatal(err)
	}
	st := m.Standing(info, 0)
	if st.Owner != OwnerGone {
		t.Fatalf("the owner is gone: %+v", st)
	}
	if st.OrphanMarkErr == "" {
		t.Fatal("a marker that could not be recorded must be reported, not read as 'gone for 0s' forever")
	}
	if leftovers, _ := filepath.Glob(m.orphanMarkPath(info.Epoch) + ".tmp"); len(leftovers) != 0 {
		t.Fatalf("a failed write leaves no temp file: %v", leftovers)
	}
}

// A lease directory this process cannot write must not make a status call spin the epoch
// lock: ErrPermission reads as contention there, so each such call waited out the lock's full
// ~2 s (in offload_status and in the fleet health handler). The probe is made first and the
// lock is not taken.
func TestUnwritableLeaseDirDoesNotTakeTheEpochLock(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "s", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	m.writeProbe = func(string) error { return errors.New("read-only file system") }
	if err := os.WriteFile(m.epochLockPath(), nil, 0o666); err != nil { // another process holds the lock
		t.Fatal(err)
	}
	defer os.Remove(m.epochLockPath())
	start := time.Now()
	st := m.Standing(m.Inspect(), 0)
	if d := time.Since(start); d > 750*time.Millisecond {
		t.Fatalf("an unwritable directory must not spin the epoch lock (took %s)", d)
	}
	if !strings.Contains(st.OrphanMarkErr, "read-only") {
		t.Fatalf("the reading must say the marker could not be recorded and why: %q", st.OrphanMarkErr)
	}
	if st.Orphaned {
		t.Fatalf("with no marker the moment is unknown and the lease cannot read orphaned: %+v", st)
	}
}

// Clearing a stale marker is a write too: an owner that is back must not keep a marker an
// unwritable directory cannot clear without the reading saying so.
func TestMarkerThatCannotBeClearedIsReported(t *testing.T) {
	fakeProcs(t, 777) // the owner is alive
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "s", PID: 777, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	mark, _ := json.Marshal(orphanMark{SinceMs: m.now().Add(-time.Hour).UnixMilli(), ByPID: 1})
	if err := os.WriteFile(m.orphanMarkPath(info.Epoch), mark, 0o666); err != nil {
		t.Fatal(err)
	}
	m.writeProbe = func(string) error { return errors.New("read-only file system") }
	st := m.Standing(info, 0)
	if st.Owner != OwnerAlive || st.Orphaned {
		t.Fatalf("an owner that is alive is not orphaned: %+v", st)
	}
	if !strings.Contains(st.OrphanMarkErr, "read-only") {
		t.Fatalf("a marker that could not be cleared must be reported: %q", st.OrphanMarkErr)
	}
}

// readRegs is readRegistrations without the error, for tests that only look at entries.
func readRegs(dir, session string) []registration {
	r, _ := readRegistrations(dir, session)
	return r
}

// A progress file that never appears is unknown, never stalled (the job may not have
// written its first line), but the reading must not stay silent about it forever: past the
// stall window with the file still missing, it is a typo or a wrong path as likely as a slow
// job, and the reading says how long, whatever the verdict.
func TestMissingProgressFileIsSaidToBeMissingPastItsStallWindow(t *testing.T) {
	fakeProcs(t)
	m, now := newTestManager(t)
	prog := filepath.Join(t.TempDir(), "never-written.jsonl")
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: prog, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	*now = now.Add(30 * time.Minute)
	_ = l.Renew()
	st := m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressUnknown || st.Progress.Problem != "does not exist" || st.Progress.PastStall || st.Progress.Age != 30*time.Minute {
		t.Fatalf("inside the stall window a missing file is just not written yet: %+v", st.Progress)
	}

	*now = now.Add(4*time.Hour + 30*time.Minute)
	_ = l.Renew()
	st = m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressUnknown || st.Stalled {
		t.Fatalf("a missing file is unknown, never stalled, however long: %+v", st.Progress)
	}
	if !st.Progress.PastStall || st.Progress.Age != 5*time.Hour || st.Progress.Problem != "does not exist" {
		t.Fatalf("past the window the reading must say the file is still missing and for how long: %+v", st.Progress)
	}
}

// Any stat failure used to read as "does not exist yet". A path that is a directory, or one
// the reader is not allowed to stat, is a different problem and says so.
func TestUnreadableProgressFileCarriesItsReason(t *testing.T) {
	fakeProcs(t)
	m, _ := newTestManager(t)
	dir := t.TempDir()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Unattended: true, TTL: 20 * time.Hour, ProgressFile: dir, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	st := m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressUnknown || !strings.Contains(st.Progress.Problem, "directory") {
		t.Fatalf("a directory is not a progress file: %+v", st.Progress)
	}

	old := statProgress
	statProgress = func(string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "CreateFile", Path: "progress", Err: fs.ErrPermission}
	}
	t.Cleanup(func() { statProgress = old })
	st = m.Standing(m.Inspect(), 0)
	if st.Progress.State != ProgressUnknown || !strings.Contains(st.Progress.Problem, "cannot be read") || !strings.Contains(st.Progress.Problem, "permission") {
		t.Fatalf("a permission error is reported as one, not as 'does not exist': %+v", st.Progress)
	}
}

// Info describes only the LOWEST live lease; a consumer that reports a verdict about
// another one needs that lease's own record (its owner, its progress contract), not the
// lowest epoch's.
func TestInfoLeaseFindsTheRecordOfOneEpoch(t *testing.T) {
	fakeProcs(t)
	m, _ := scopedManager(t)
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "a", Devices: []string{"GPU-TEST-0"}, TTL: time.Hour, Owner: Owner{Session: "sess-A"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TryAcquire(ClassMedia, Options{Reason: "b", Devices: []string{"GPU-TEST-1"}, TTL: time.Hour, Owner: Owner{Session: "sess-B"}})
	if err != nil {
		t.Fatal(err)
	}
	info := m.Inspect()
	if info.Epoch != a.Epoch() || info.Owner == nil || info.Owner.Session != "sess-A" {
		t.Fatalf("Info is the lowest epoch's: %+v", info)
	}
	lb, ok := info.Lease(b.Epoch())
	if !ok || lb.Epoch != b.Epoch() || lb.Owner == nil || lb.Owner.Session != "sess-B" || lb.Reason != "b" {
		t.Fatalf("Lease(%d) must return that lease's own record: %+v ok=%v", b.Epoch(), lb, ok)
	}
	if _, ok := info.Lease(999); ok {
		t.Fatal("an epoch that is not live is not found")
	}
	// A single lease finds itself.
	_ = b.Release()
	one := m.Inspect()
	if l, ok := one.Lease(a.Epoch()); !ok || l.Epoch != a.Epoch() {
		t.Fatalf("a single lease finds itself: %+v ok=%v", l, ok)
	}
	_ = a.Release()
}

// An owner that merely cannot be told (an unreadable registry) must not erase the moment it
// was first seen gone: only positive evidence that it is back clears the marker.
func TestUnknownOwnerDoesNotEraseAnOrphanMarker(t *testing.T) {
	fakeProcs(t) // pid 777 is dead
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: 8 * time.Hour, Owner: Owner{Session: "sess-A", Tracked: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	_ = m.Standing(m.Inspect(), 0) // gone: the first observer stamps
	if len(orphanMarks(t, m)) != 1 {
		t.Fatalf("setup: the marker is stamped: %v", orphanMarks(t, m))
	}
	*now = now.Add(time.Hour)
	_ = l.Renew()
	old := readRegistryDir
	readRegistryDir = func(string) ([]os.DirEntry, error) { return nil, &fs.PathError{Op: "open", Err: fs.ErrPermission} }
	t.Cleanup(func() { readRegistryDir = old })
	if st := m.Standing(m.Inspect(), 0); st.Owner != OwnerUnknown {
		t.Fatalf("setup: an unreadable registry reads unknown: %+v", st)
	}
	if n := len(orphanMarks(t, m)); n != 1 {
		t.Fatalf("an owner that cannot be told must not erase the marker (marks: %d)", n)
	}
}
