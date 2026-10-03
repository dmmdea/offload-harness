package gpulease

// Process-tree containment: a dead wrapper with a live tree keeps its claim (plan P10).
//
// A wrapper that is gone no longer proves its job is gone. The wrapper records the members of
// the tree it started (pid + start identity) in the lease record on each tick; a reader treats
// a lease whose wrapper is dead or recycled as still held while any recorded member is alive.
// The claim is bounded by observation, never by a timer: every read checks the members again
// by pid AND start identity, so a recycled pid cannot hold a card and a card frees itself at
// the first read after its members are gone. Pids here are synthetic; liveness and start
// identity come through the package seams.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveSet makes exactly the listed pids read as alive; every other pid, the wrapper's included,
// is gone. The returned function changes the set.
func liveSet(t *testing.T, pids ...int) (set func(...int)) {
	t.Helper()
	var mu sync.Mutex
	alive := map[int]bool{}
	set = func(p ...int) {
		mu.Lock()
		defer mu.Unlock()
		alive = map[int]bool{}
		for _, x := range p {
			alive[x] = true
		}
	}
	set(pids...)
	orig := pidAliveFn
	pidAliveFn = func(pid int) bool {
		mu.Lock()
		defer mu.Unlock()
		return alive[pid]
	}
	t.Cleanup(func() { pidAliveFn = orig })
	return set
}

// A waiter is not handed a card whose wrapper died while the job it started is still running.
func TestWrapperDeadTreeAliveDoesNotFreeCard(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}, {PID: 9002, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	liveSet(t, 9001, 9002, 5000) // the wrapper (this process) reads as gone

	waiter := asPID(m, 5000)
	_, err := waiter.TryAcquire(ClassMedia, Options{Reason: "next", Devices: []string{card0}, TTL: time.Hour})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("the card of a dead wrapper with a live tree must stay held, got %v", err)
	}
	if !held.Info.TreeOrphan || len(held.Info.TreeLive) != 2 {
		t.Fatalf("the refusal must say the lease is tree-orphan with its live members, got %+v", held.Info)
	}
	info := m.InspectFor([]string{card0})
	if !info.Held || !info.TreeOrphan || info.Epoch != l.Epoch() {
		t.Fatalf("Inspect must show the lease held and tree-orphan: %+v", info)
	}
	if claims := claimFiles(t, m); claims[card0] == "" {
		t.Fatalf("the claim of a tree-orphan lease must stay on disk, claims: %v", claims)
	}
	// Only the cards of the orphan stay claimed: a waiter that wants another card gets it.
	if _, err := waiter.TryAcquire(ClassMedia, Options{Reason: "other card", Devices: []string{card1}, TTL: time.Hour}); err != nil {
		t.Fatalf("a card the orphan does not hold must stay free: %v", err)
	}
}

// The same rule for a whole-node lease (meta.json is its real record).
func TestWholeNodeLeaseOfADeadWrapperWithALiveTreeIsKept(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	liveSet(t, 9001, 5000)
	_, err = asPID(m, 5000).TryAcquire(ClassText, Options{Reason: "bench", TTL: time.Hour})
	var held *ErrHeld
	if !errors.As(err, &held) || !held.Info.TreeOrphan {
		t.Fatalf("a whole-node lease whose wrapper died over a live tree stays held and tree-orphan, got %v", err)
	}
}

// Nothing here is a timer: a lease whose window and heartbeat lapsed hours ago is still held
// while its members live, and frees at the first read after they are gone, with no clock change.
func TestTreeOrphanIsObservedByPidAndStartNotByTimer(t *testing.T) {
	m, now := scopedManager(t)
	l := mustDevices(t, m, card0)
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	set := liveSet(t, 9001, 5000)
	*now = now.Add(30 * time.Hour) // far past the declared window and any heartbeat
	waiter := asPID(m, 5000)
	if _, err := waiter.TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("hours past its window a tree-orphan is still held while its members live, got %v", err)
	}
	set(5000) // the members exit; the clock does not move
	got, err := waiter.TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("the card frees itself at the first read after its members are gone: %v", err)
	}
	_ = got.Release()
}

// A pid that was recycled for an unrelated process is not a member: the start identity differs.
func TestRecycledPidDoesNotHoldACard(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	liveSet(t, 9001, 5000)
	m.procStart = func(pid int) (int64, bool) {
		if pid == 9001 {
			return 777, true // the pid now names a process that began at another time
		}
		return 4242, true
	}
	waiter := asPID(m, 5000)
	got, err := waiter.TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("a recycled member pid must not hold the card: %v", err)
	}
	_ = got.Release()
}

func TestTreeOrphanFreesWhenMembersGone(t *testing.T) {
	for _, whole := range []bool{false, true} {
		m, _ := scopedManager(t)
		var l *Lease
		var err error
		if whole {
			l, err = m.TryAcquire(ClassMedia, Options{Reason: "whole", TTL: time.Hour})
		} else {
			l, err = m.TryAcquire(ClassMedia, Options{Reason: "cards", Devices: []string{card0}, TTL: time.Hour})
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
			t.Fatal(err)
		}
		set := liveSet(t, 9001, 5000)
		if info := m.Inspect(); !info.Held || !info.TreeOrphan {
			t.Fatalf("whole=%v: held and tree-orphan while the member lives: %+v", whole, info)
		}
		set(5000)
		if info := m.Inspect(); info.Held {
			t.Fatalf("whole=%v: a lease whose wrapper and members are all gone is free: %+v", whole, info)
		}
		got, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: l.Devices(), TTL: time.Hour})
		if err != nil {
			t.Fatalf("whole=%v: the freed card is acquirable: %v", whole, err)
		}
		_ = got.Release()
	}
}

// The reclaim rule itself, as a pure function over seams. A recorded member keeps the claim
// only on the wrapper-gone path: the heartbeat-and-window rule for a wrapper that is alive is
// untouched.
func TestReclaimableTreeMemberRule(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	const wrapper = 100
	base := func() *Meta {
		return &Meta{
			Holder:       Holder{PID: wrapper, StartTimeMs: 50},
			AcquiredAtMs: now.Add(-time.Hour).UnixMilli(),
			RenewedAtMs:  now.Add(-time.Minute).UnixMilli(),
			ExpiresAtMs:  now.Add(time.Hour).UnixMilli(),
			Tree:         []TreeMember{{PID: 200, StartMs: 60}},
		}
	}
	type tc struct {
		name    string
		mutate  func(*Meta)
		alive   map[int]bool
		starts  map[int]int64
		want    bool
		unknown bool // procStart cannot say for any pid
	}
	cases := []tc{
		{name: "wrapper dead, member alive, same start: kept", alive: map[int]bool{200: true}, starts: map[int]int64{200: 60}, want: false},
		{name: "wrapper dead, member alive, another start (recycled): free", alive: map[int]bool{200: true}, starts: map[int]int64{200: 61}, want: true},
		{name: "wrapper dead, member gone: free", alive: map[int]bool{}, want: true},
		{name: "wrapper dead, member alive, start unreadable: kept (a safety check does not turn itself off)", alive: map[int]bool{200: true}, unknown: true, want: false},
		{name: "wrapper dead, member recorded without a start: held by pid alone", mutate: func(m *Meta) { m.Tree[0].StartMs = 0 }, alive: map[int]bool{200: true}, starts: map[int]int64{200: 99}, want: false},
		{name: "wrapper dead, no tree recorded: free (unchanged)", mutate: func(m *Meta) { m.Tree = nil }, alive: map[int]bool{}, want: true},
		{name: "wrapper recycled, member alive: kept", alive: map[int]bool{wrapper: true, 200: true}, starts: map[int]int64{wrapper: 51, 200: 60}, want: false},
		{name: "wrapper recycled, member gone: free", alive: map[int]bool{wrapper: true}, starts: map[int]int64{wrapper: 51}, want: true},
		{name: "wrapper alive and heartbeating: held", alive: map[int]bool{wrapper: true, 200: true}, starts: map[int]int64{wrapper: 50, 200: 60}, want: false},
		{
			name: "wrapper alive, heartbeat stale AND window over: the conjunction still frees it (unchanged), tree or not",
			mutate: func(m *Meta) {
				m.RenewedAtMs = now.Add(-time.Hour).UnixMilli()
				m.ExpiresAtMs = now.Add(-time.Minute).UnixMilli()
			},
			alive: map[int]bool{wrapper: true, 200: true}, starts: map[int]int64{wrapper: 50, 200: 60}, want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := base()
			if c.mutate != nil {
				c.mutate(meta)
			}
			orig := pidAliveFn
			pidAliveFn = func(pid int) bool { return c.alive[pid] }
			t.Cleanup(func() { pidAliveFn = orig })
			procStart := func(pid int) (int64, bool) {
				if c.unknown {
					return 0, false
				}
				v, ok := c.starts[pid]
				return v, ok
			}
			if got := Reclaimable(meta, now, DefaultHeartbeatTTL, procStart); got != c.want {
				t.Fatalf("Reclaimable = %v, want %v", got, c.want)
			}
		})
	}
}

// The record carries the tree: pid + start identity, additive and omitempty, so a record with
// none reads exactly as before.
func TestTreeRecordRoundTripsAndIsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Meta{Epoch: 3})
	if err != nil || strings.Contains(string(b), "tree") {
		t.Fatalf("a record without a tree must not carry the key: %s %v", b, err)
	}
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	want := []TreeMember{{PID: 9001, StartMs: 4242}, {PID: 9002, StartMs: 4243}}
	if err := l.RecordTree(want); err != nil {
		t.Fatal(err)
	}
	rec, err := readEpochRecord(m.leaseDir(), l.Epoch())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Tree) != 2 || rec.Tree[0] != want[0] || rec.Tree[1] != want[1] {
		t.Fatalf("recorded tree = %+v, want %+v", rec.Tree, want)
	}
}

// The wrapper calls RecordTree on every tick; the epoch lock is taken only when the set changed.
func TestRecordTreeWritesOnlyWhenTheSetChanges(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	set := []TreeMember{{PID: 9001, StartMs: 4242}}
	if err := l.RecordTree(set); err != nil {
		t.Fatal(err)
	}
	path := epochRecordPath(m.leaseDir(), l.Epoch())
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Fatal("an unchanged tree rewrote the record (and took the epoch lock)")
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}, {PID: 9003, StartMs: 4244}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.ModTime().Equal(old) {
		t.Fatal("a changed tree must be recorded")
	}
	// Order is not a change.
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9003, StartMs: 4244}, {PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Fatal("the same members in another order rewrote the record")
	}
}

func TestRecordTreeIsRefusedForAFencedOutEpoch(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassText, Options{Reason: "x", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseByEpoch(l.Epoch()); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err == nil {
		t.Fatal("a lease that is no longer ours must not take a tree record")
	}
}

// markerFixture is a comfy directory with one launch marker, armed as the directory the readers
// look in.
func markerFixture(t *testing.T, file string, rec map[string]any) {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
		t.Fatal(err)
	}
	SetComfyDir(dir)
	t.Cleanup(func() { SetComfyDir("") })
	// The process-start conversion is the platform's; a test pins it so the recycled-pid check
	// compares the unix-ms values written here.
	orig := startUnixMsFn
	startUnixMsFn = func(v int64) (int64, bool) { return v, true }
	t.Cleanup(func() { startUnixMsFn = orig })
}

// A ComfyUI instance can be outside the wrapper's job (a launcher that broke away, a legacy
// wrapper): its launch marker names the lease epoch it was launched under, which is the tie.
func TestComfyUILikeGrandchildOutsideJobIsFoundViaMarker(t *testing.T) {
	m, now := scopedManager(t)
	l := mustDevices(t, m, card0)
	// The recorded tree is the launcher, which has exited; the render server it started is not in it.
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	markerFixture(t, ".offload-launch-p8189.json", map[string]any{
		"startedAt": now.UnixMilli(), "pid": 9100, "ownerPid": 9001, "args": []string{"main.py", "--port", "8189"},
		"key": "p8189", "port": 8189, "leaseEpoch": l.Epoch(),
	})
	m.procStart = func(pid int) (int64, bool) { return now.UnixMilli() - 5000, true } // began before its marker
	liveSet(t, 9100, 5000)                                                            // the launcher is gone, the server lives
	_, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
	var held *ErrHeld
	if !errors.As(err, &held) || !held.Info.TreeOrphan {
		t.Fatalf("an instance its marker ties to the lease keeps the claim, got %v", err)
	}
	if len(held.Info.TreeLive) != 1 || held.Info.TreeLive[0].PID != 9100 || !strings.Contains(held.Info.TreeWhy, "launch marker") {
		t.Fatalf("the live member and its evidence are named: %+v", held.Info)
	}
}

// A marker that names ANOTHER lease, or whose pid began after the marker was written (recycled),
// ties nothing to this lease.
func TestMarkerThatIsNotThisLeasesHoldsNothing(t *testing.T) {
	cases := map[string]struct {
		epochDelta int64
		startDelta int64 // process start minus marker startedAt (ms)
	}{
		"names another lease":            {epochDelta: 1, startDelta: -5000},
		"pid began after the marker was": {epochDelta: 0, startDelta: 60_000},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, now := scopedManager(t)
			l := mustDevices(t, m, card0)
			if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
				t.Fatal(err)
			}
			markerFixture(t, ".offload-launch-p8189.json", map[string]any{
				"startedAt": now.UnixMilli(), "pid": 9100, "args": []string{"main.py"},
				"key": "p8189", "port": 8189, "leaseEpoch": int64(l.Epoch()) + c.epochDelta,
			})
			m.procStart = func(pid int) (int64, bool) { return now.UnixMilli() + c.startDelta, true }
			liveSet(t, 9100, 5000)
			got, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
			if err != nil {
				t.Fatalf("a marker that is not this lease's must not hold its card: %v", err)
			}
			_ = got.Release()
		})
	}
}

// A lease with NO recorded tree (a wrapper that predates this change) can be tied to the default
// instance's marker (no lease epoch in it) only when the marker began at or after the lease and no
// other lease exists in the directory to claim it; a marker left from before proves nothing.
func TestLegacyLeaseWithNoTreeIsHeldByATiedDefaultMarkerOnly(t *testing.T) {
	cases := []struct {
		name      string
		startedAt func(acquired time.Time) int64
		other     bool
		held      bool
	}{
		{"marker began after the lease, alone: tied", func(a time.Time) int64 { return a.Add(time.Minute).UnixMilli() }, false, true},
		{"marker from before the lease: leftover", func(a time.Time) int64 { return a.Add(-time.Hour).UnixMilli() }, false, false},
		{"marker after the lease but another lease is live: not tied", func(a time.Time) int64 { return a.Add(time.Minute).UnixMilli() }, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, now := scopedManager(t)
			l := mustDevices(t, m, card0) // acquired at the fake clock
			if c.other {
				if _, err := m.TryAcquire(ClassMedia, Options{Devices: []string{card1}, TTL: time.Hour}); err != nil {
					t.Fatal(err)
				}
			}
			started := c.startedAt(*now)
			markerFixture(t, ".offload-launch.json", map[string]any{
				"startedAt": started, "pid": 9100, "ownerPid": 9099, "args": []string{"main.py"},
			})
			m.procStart = func(pid int) (int64, bool) { return started - 500, true } // began just before its marker
			*now = now.Add(2 * time.Minute)
			// The other lease's own wrapper (this process) must stay "alive" in that variant: only
			// the lease under test is judged against a dead wrapper.
			dead := os.Getpid()
			orig := pidAliveFn
			pidAliveFn = func(pid int) bool { return pid == 9100 || (c.other && pid == dead) }
			t.Cleanup(func() { pidAliveFn = orig })
			// With one process acting as both holders the wrapper cannot be dead for one lease and
			// alive for the other; record the lease under test as held by another pid.
			if c.other {
				rewriteHolder(t, m, l.Epoch(), 7777)
			}
			info := m.InspectFor([]string{card0})
			if info.Held != c.held || info.TreeOrphan != c.held {
				t.Fatalf("held=%v tree-orphan=%v, want both %v: %+v", info.Held, info.TreeOrphan, c.held, info)
			}
		})
	}
}

// rewriteHolder points a v2 record at another holder pid (a test's way of having two wrappers).
func rewriteHolder(t *testing.T, m *Manager, epoch uint64, pid int) {
	t.Helper()
	rec, err := readEpochRecord(m.leaseDir(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	rec.Holder.PID = pid
	if err := writeEpochRecord(m.leaseDir(), rec); err != nil {
		t.Fatal(err)
	}
}

// A legacy wrapper's children are found by walking the dead wrapper's pid in the process table:
// Windows keeps the parent link of a child whose parent died. Only children that began at or
// after the lease count, and a wrapper pid that is alive but recycled is never walked (its
// children are another process's).
func TestLegacyLeaseTreeIsFoundByWalkingTheDeadWrappersChildren(t *testing.T) {
	leaseStart := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		walk       []TreeProc
		wrapperUp  bool // the wrapper pid is alive, but another process (recycled)
		wantMember int
	}{
		{"a child that began after the lease", []TreeProc{{PID: 100, StartMs: 1}, {PID: 9300, PPID: 100, StartMs: leaseStart.Add(time.Minute).UnixMilli()}}, false, 9300},
		{"a child older than the lease is not ours", []TreeProc{{PID: 100, StartMs: 1}, {PID: 9300, PPID: 100, StartMs: leaseStart.Add(-time.Hour).UnixMilli()}}, false, 0},
		{"the wrapper pid was recycled: not walked", []TreeProc{{PID: 100, StartMs: 1}, {PID: 9300, PPID: 100, StartMs: leaseStart.Add(time.Minute).UnixMilli()}}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, now := newTestManager(t)
			*now = leaseStart
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "legacy-like", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			_ = l
			rewriteWholeNodeHolder(t, m, 100)
			orig := startUnixMsFn
			startUnixMsFn = func(v int64) (int64, bool) { return v, true }
			t.Cleanup(func() { startUnixMsFn = orig })
			origWalk := processTreeFn
			processTreeFn = func(root int) ([]TreeProc, error) { return c.walk, nil }
			t.Cleanup(func() { processTreeFn = origWalk })
			set := []int{9300}
			if c.wrapperUp {
				set = append(set, 100)
			}
			liveSet(t, set...)
			m.procStart = func(pid int) (int64, bool) {
				if pid == 100 {
					return 999_999, true // not the start the record holds (4242): recycled when alive
				}
				return leaseStart.Add(time.Minute).UnixMilli(), true
			}
			info := m.Inspect()
			if c.wantMember == 0 {
				if info.Held {
					t.Fatalf("nothing to hold the claim: %+v", info)
				}
				return
			}
			if !info.Held || !info.TreeOrphan || len(info.TreeLive) != 1 || info.TreeLive[0].PID != c.wantMember {
				t.Fatalf("a child of the dead wrapper keeps the claim: %+v", info)
			}
			if !strings.Contains(info.TreeWhy, "process table") {
				t.Fatalf("the evidence is named: %q", info.TreeWhy)
			}
		})
	}
}

func rewriteWholeNodeHolder(t *testing.T, m *Manager, pid int) {
	t.Helper()
	meta, err := m.readMeta()
	if err != nil {
		t.Fatal(err)
	}
	meta.Holder.PID = pid
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(m.metaPath(), b, 0o666); err != nil {
		t.Fatal(err)
	}
}

// Cross-version honesty (invariant I7): a reader that predates this change ignores the recorded
// tree and reclaims a dead wrapper's lease at once, exactly as it always did. The tree-orphan rule
// holds only where every reader is this build; `gpu doctor` lists the older ones.
func TestOldReaderStillReclaimsATreeOrphanRecord(t *testing.T) {
	m, now := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	liveSet(t, 9001, 5000) // the wrapper is gone, the job lives
	if info := m.Inspect(); !info.Held || !info.TreeOrphan {
		t.Fatalf("this build keeps the claim: %+v", info)
	}
	info, _, reclaimable := legacyInspect(m.leaseDir(), *now, m.heartbeatTTL, m.procStart)
	if info.Held || !reclaimable {
		t.Fatalf("the pre-change reader frees a dead wrapper at once (if this fails the fixture no longer models it): held=%v reclaimable=%v", info.Held, reclaimable)
	}
}

// A waiter refused by a tree-orphan is told which lease, why it is held although its wrapper is
// gone, what still runs and the exact command that frees it: "held by pid N" with N dead sent
// readers away from the one fact that mattered, the card is in use.
func TestErrHeldNamesATreeOrphanAndTheReleaseCommand(t *testing.T) {
	m, _ := scopedManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Devices: []string{card0}, TTL: 8 * time.Hour, Command: "python film.py --spec film.json"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	liveSet(t, 9001, 5000)
	_, err = asPID(m, 5000).TryAcquire(ClassMedia, Options{Reason: "another render", Devices: []string{card0}})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("want ErrHeld, got %v", err)
	}
	msg := held.Error()
	for _, want := range []string{
		"tree-orphan", "wrapper (pid " + strconv.Itoa(os.Getpid()) + ") is gone", "pid 9001", "film.py",
		"local-offload gpu release --epoch " + strconv.FormatUint(l.Epoch(), 10), "nothing reclaims or kills it",
		"frees itself", "the rest of the job keeps running",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	// The takeover verb does not exist in this build: a tree-orphan's waiter is told the one command that frees it today.
	if strings.Contains(msg, "gpu takeover") {
		t.Errorf("a tree-orphan is not sent to a command that is not in this build:\n%s", msg)
	}
}

// The standing of a tree-orphan: it leads the escalation words, and the flags of a sibling
// lease do not hide it.
func TestStandingCarriesTheTreeOrphanAndLeadsTheEscalation(t *testing.T) {
	if got := (Standing{TreeOrphan: true, Stalled: true, Orphaned: true, Overdue: true}).Escalation(); got != "tree-orphan" {
		t.Fatalf("tree-orphan outranks stalled, orphaned and overdue, got %q", got)
	}
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	healthy, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Reason: "healthy", Devices: []string{card1}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = healthy.Release() }()
	liveSet(t, 9001, 5000)
	infos := m.Leases()
	if len(infos) != 2 {
		t.Fatalf("both leases are live: %+v", infos)
	}
	st := m.StandingAll(m.Inspect(), 0)
	if !st.TreeOrphan || st.Escalation() != "tree-orphan" || len(st.TreeLive) != 1 {
		t.Fatalf("the worst standing across the leases is the tree-orphan: %+v", st)
	}
}
