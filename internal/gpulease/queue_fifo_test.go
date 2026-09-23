package gpulease

// Queue fairness: register D-13x (2026-09-22), observed live on the Lenovo.
//
// `gpu status` showed a text waiter (pid 56364) sitting in line for 1h43m while two
// media reservations that queued LATER (pids 59816, 28452) each acquired the card ahead
// of it. Acquire's retry loop polled TryAcquire once a second from EVERY waiting
// process with no ordering between them: registerWaiter/Waiters() recorded who was
// queued, but nothing in the acquire path ever consulted that record before racing for
// the O_EXCL claim, so whichever process's poll tick landed first after a release won —
// arrival order was pure luck. These tests pin FIFO: the OLDEST live waiter gets the
// next grant, a dead waiter's record never blocks the line, an expired --wait leaves
// it, and class carries no priority (no ADR documents one for the queue itself).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// waitAllRegistered blocks until n live waiters are registered, or fails the test.
func waitAllRegistered(t *testing.T, m *Manager, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(m.Waiters()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("only %d of %d waiters registered within %s", len(m.Waiters()), n, within)
}

// The headline case: N waiters queue in a known order (staggered registration), the
// holder releases once, and each successive release must hand the card to the OLDEST
// remaining waiter — never a later arrival.
func TestQueuedWaitersAreServedInArrivalOrder(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	const n = 5
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Stagger registration so SinceMs strictly increases with i.
			time.Sleep(time.Duration(i) * 40 * time.Millisecond)
			l, aerr := m.Acquire(ClassText, Options{Reason: "job", Wait: 10 * time.Second})
			if aerr != nil {
				t.Errorf("waiter %d: %v", i, aerr)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond) // hold briefly so the next waiter must actually queue
			if rerr := l.Release(); rerr != nil {
				t.Errorf("waiter %d release: %v", i, rerr)
			}
		}(i)
	}

	waitAllRegistered(t, m, n, 3*time.Second)
	if err := holder.Release(); err != nil {
		t.Fatalf("release seed holder: %v", err)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != n {
		t.Fatalf("only %d of %d waiters acquired the card: %v", len(order), n, order)
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("acquisition order = %v, want strictly ascending arrival order 0..%d — "+
				"waiter %d was served at position %d, meaning a later arrival won the card first",
				order, n-1, got, i)
		}
	}
}

// Class must not reorder the queue: no ADR documents a queue-level class priority
// (0026 is the text-load admission gate, 0041 is the drain budget — neither says
// anything about acquisition order), so alternating text/media waiters are served in
// pure arrival order exactly like a same-class queue.
func TestMixedClassWaitersAreServedInArrivalOrder(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	const n = 6
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := ClassText
			if i%2 == 1 {
				class = ClassMedia
			}
			time.Sleep(time.Duration(i) * 40 * time.Millisecond)
			l, aerr := m.Acquire(class, Options{Reason: "job", Wait: 10 * time.Second})
			if aerr != nil {
				t.Errorf("waiter %d: %v", i, aerr)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			if rerr := l.Release(); rerr != nil {
				t.Errorf("waiter %d release: %v", i, rerr)
			}
		}(i)
	}

	waitAllRegistered(t, m, n, 3*time.Second)
	if err := holder.Release(); err != nil {
		t.Fatalf("release seed holder: %v", err)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != n {
		t.Fatalf("only %d of %d waiters acquired the card: %v", len(order), n, order)
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("mixed-class acquisition order = %v, want 0..%d — class reordered the queue", order, n-1)
		}
	}
}

// A dead waiter's on-disk record must never block the line behind it. A record left
// by a process that queued FIRST but no longer exists is pruned on read, so the live
// waiter behind it is served without waiting for the dead one to "take its turn".
func TestDeadWaiterRecordNeverBlocksTheLine(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	const deadPID = 999999
	orig := pidAliveFn
	pidAliveFn = func(pid int) bool {
		if pid == deadPID {
			return false
		}
		return orig(pid)
	}
	t.Cleanup(func() { pidAliveFn = orig })

	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		t.Fatalf("mkdir waiters: %v", err)
	}
	// Queued a full hour "before" any live waiter, so a rule that trusted the record
	// alone would defer to it forever.
	dead := Waiter{PID: deadPID, Class: ClassText, Reason: "crashed mid-queue", SinceMs: m.now().Add(-time.Hour).UnixMilli()}
	b, merr := json.Marshal(dead)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	deadPath := filepath.Join(m.waitersDir(), "999999.deadwaiter.json")
	if err := os.WriteFile(deadPath, b, 0o666); err != nil {
		t.Fatalf("seed dead waiter: %v", err)
	}

	live := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "live waiter", Wait: 5 * time.Second})
		if aerr == nil {
			_ = l.Release()
		}
		live <- aerr
	}()
	waitAllRegistered(t, m, 1 /* the live waiter; the dead one is pruned on read */, 2*time.Second)

	if err := holder.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-live; err != nil {
		t.Fatalf("live waiter never acquired behind a dead record: %v", err)
	}
	if _, statErr := os.Stat(deadPath); !os.IsNotExist(statErr) {
		t.Fatal("the dead waiter's record survived a read; it should have been pruned")
	}
}

// A waiter whose own --wait window expires must leave the line rather than continue
// to occupy a slot that blocks whoever queued behind it.
func TestExpiredWaitLeavesTheLineForTheNextWaiter(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	var held *ErrHeld
	if _, err := m.Acquire(ClassText, Options{Reason: "impatient", Wait: 30 * time.Millisecond}); !errors.As(err, &held) {
		t.Fatalf("short waiter: err = %v, want *ErrHeld after its wait expired", err)
	}
	if n := len(m.Waiters()); n != 0 {
		t.Fatalf("an expired waiter left %d record(s) behind: %v", n, m.Waiters())
	}

	// A second, more patient waiter must not be blocked by the first one's ghost.
	second := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "patient", Wait: 3 * time.Second})
		if aerr == nil {
			_ = l.Release()
		}
		second <- aerr
	}()
	waitAllRegistered(t, m, 1, 2*time.Second)
	if err := holder.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second waiter did not acquire: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lead review before ship (2026-09-22): alive-but-stuck waiters, mixed
// versions, in-process concurrency, and the Windows delete-retry path.
// ---------------------------------------------------------------------------

// A waiter can be genuinely ALIVE (same pid, same start time — not recycled)
// and still have stopped actually polling: suspended by the OS, wedged in
// another goroutine, paused in a debugger, or an older binary whose Acquire
// loop exited without ever unregistering. Under "oldest LIVE waiter wins"
// alone that record is the permanent, unbreakable head of the line — pid
// liveness can never distinguish it from a legitimately queued waiter.
// registerWaiter is called directly (bypassing Acquire) to build exactly that
// record and then, deliberately, never refreshed.
func TestAliveButNotPollingWaiterDoesNotBlockTheQueue(t *testing.T) {
	m := realClockManager(t)
	m.waiterHeartbeatTTL = 30 * time.Millisecond // fast staleness window for the test
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	stuck, stuckUnregister := m.registerWaiter(ClassText, Options{Reason: "stuck"})
	t.Cleanup(stuckUnregister)
	if stuck.path == "" {
		t.Fatal("the stuck waiter failed to register — test cannot proceed")
	}

	// Outlive the (shortened) staleness window without ever refreshing —
	// simulating a suspended process or an old binary that registers once and
	// never calls refreshWaiter.
	time.Sleep(100 * time.Millisecond)

	live := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "live waiter", Wait: 5 * time.Second})
		if aerr == nil {
			_ = l.Release()
		}
		live <- aerr
	}()
	// The stuck record is already on disk with an EARLIER SinceMs than the
	// live waiter will get, so len(Waiters()) briefly includes it until a read
	// prunes it; wait for the LIVE one specifically to show up by polling
	// until the count is nonzero and the stuck one is gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ws := m.Waiters()
		found := false
		for _, w := range ws {
			if w.path == stuck.path {
				found = true
			}
		}
		if !found && len(ws) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := holder.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-live; err != nil {
		t.Fatalf("live waiter never acquired behind an alive-but-stuck record: %v", err)
	}
	if _, statErr := os.Stat(stuck.path); !os.IsNotExist(statErr) {
		t.Fatal("the stuck waiter's record survived a read after going heartbeat-stale; it should have been pruned")
	}
}

// The dead-pid case (TestDeadWaiterRecordNeverBlocksTheLine) and the
// heartbeat-stale case (above) are different code paths; pid RECYCLING is a
// third: the pid is alive, but it now belongs to an unrelated process.
// Waiters() must catch this the same way Reclaimable does for the lease
// holder itself — by comparing the recorded process-start identity against
// the current one — so a recycled pid can neither hold the lease nor jump the
// waiter queue.
func TestRecycledPidWaiterRecordNeverBlocksTheLine(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	const recycledPID = 999998
	origAlive := pidAliveFn
	pidAliveFn = func(pid int) bool {
		if pid == recycledPID {
			return true // a process with this pid IS running — just not the original one
		}
		return origAlive(pid)
	}
	t.Cleanup(func() { pidAliveFn = origAlive })
	origStart := m.procStart
	m.procStart = func(pid int) (int64, bool) {
		if pid == recycledPID {
			return 424242, true // different start time than the record below: recycled
		}
		return origStart(pid)
	}

	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		t.Fatalf("mkdir waiters: %v", err)
	}
	old := Waiter{PID: recycledPID, StartTimeMs: 111111, Class: ClassText,
		Reason: "original holder of this pid, long gone", SinceMs: m.now().Add(-time.Hour).UnixMilli()}
	b, merr := json.Marshal(old)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	oldPath := filepath.Join(m.waitersDir(), "999998.recycled.json")
	if err := os.WriteFile(oldPath, b, 0o666); err != nil {
		t.Fatalf("seed recycled-pid waiter: %v", err)
	}

	live := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "live waiter", Wait: 5 * time.Second})
		if aerr == nil {
			_ = l.Release()
		}
		live <- aerr
	}()
	waitAllRegistered(t, m, 1, 2*time.Second)

	if err := holder.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-live; err != nil {
		t.Fatalf("live waiter never acquired behind a recycled-pid record: %v", err)
	}
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatal("the recycled-pid waiter record survived a read; it should have been pruned")
	}
}

// MIXED VERSIONS: an older harness binary's waiter record is the OLD file
// name shape ("pid.sincems.json" — two segments, no random token) and, since
// old code only ever raced TryAcquire and never knew about ordering, it is
// NEVER refreshed after creation. The new reader must still parse it (no
// schema change) and must not let its presence — nor its earlier SinceMs —
// wedge a new-version waiter behind it forever: it goes heartbeat-stale on
// the same clock as any other stuck record and stops affecting anyone's
// order.
func TestOldVersionWaiterRecordIsReadableAndCannotWedgeTheLine(t *testing.T) {
	m := realClockManager(t)
	m.waiterHeartbeatTTL = 30 * time.Millisecond
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		t.Fatalf("mkdir waiters: %v", err)
	}
	oldPid := os.Getpid() // a real, alive pid; old code carried no random token either
	old := Waiter{PID: oldPid, Class: ClassText, Reason: "pre-upgrade waiter", SinceMs: m.now().Add(-time.Minute).UnixMilli()}
	if st, ok := m.procStart(oldPid); ok {
		old.StartTimeMs = st
	}
	b, merr := json.Marshal(old)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	// The OLD two-segment name, exactly what a pre-D-13x registerWaiter wrote.
	oldPath := filepath.Join(m.waitersDir(), strconv.Itoa(oldPid)+"."+strconv.FormatInt(old.SinceMs, 10)+".json")
	if err := os.WriteFile(oldPath, b, 0o666); err != nil {
		t.Fatalf("seed old-format waiter: %v", err)
	}

	// Outlive the staleness window; old code never rewrites this file.
	time.Sleep(100 * time.Millisecond)

	live := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassText, Options{Reason: "new-version waiter", Wait: 5 * time.Second})
		if aerr == nil {
			_ = l.Release()
		}
		live <- aerr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ws := m.Waiters()
		found := false
		for _, w := range ws {
			if w.path == oldPath {
				found = true
			}
		}
		if !found && len(ws) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := holder.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-live; err != nil {
		t.Fatalf("a new-version waiter never acquired behind an old-format record: %v", err)
	}
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatal("the old-format waiter record survived a read; it should go stale and be pruned like any other")
	}
}

// A deterministic tie: two waiters registered at the EXACT same instant (a
// fixed fake clock, so SinceMs is forced identical) must resolve to exactly
// one front-of-queue — never both, never neither. Both/neither is a livelock:
// "both front" races them against each other again (the original bug),
// "neither front" means nobody ever attempts the claim.
func TestTiedWaitersResolveToExactlyOneFrontOfQueue(t *testing.T) {
	m, _ := newTestManager(t) // fixed clock: both registrations land on the same millisecond
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	a, unregA := m.registerWaiter(ClassText, Options{Reason: "a"})
	defer unregA()
	b, unregB := m.registerWaiter(ClassMedia, Options{Reason: "b"})
	defer unregB()

	if a.SinceMs != b.SinceMs {
		t.Fatalf("test setup did not produce a tie: SinceMs %d vs %d", a.SinceMs, b.SinceMs)
	}
	if a.path == b.path {
		t.Fatal("two registrations collided on the same file — the random token did not disambiguate a same-millisecond tie")
	}

	aFront := m.isFrontOfQueue(a)
	bFront := m.isFrontOfQueue(b)
	if aFront == bFront {
		t.Fatalf("exactly one of two tied waiters must be front-of-queue, got a=%v b=%v (both or neither is a livelock)", aFront, bFront)
	}
}

// The in-process slot path aside (which never calls registerWaiter — it
// arbitrates purely in-process, see docs/systems/gpu-lease.md "Two jobs in
// the SAME process"), nothing stops two goroutines in ONE process from each
// calling Acquire directly, and doing so shares one real pid across two
// waiter records. With zero stagger, several of them are likely to tie on
// SinceMs at once. None of that may deadlock: every one of them must
// eventually acquire and release in turn.
func TestSameProcessConcurrentWaitersDoNotDeadlock(t *testing.T) {
	m := realClockManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "seed", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}

	const n = 8
	var mu sync.Mutex
	completed := 0
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := ClassText
			if i%2 == 1 {
				class = ClassMedia
			}
			// Deliberately NO stagger: every goroutine calls Acquire from the
			// same real pid at once, maximizing SinceMs ties.
			l, aerr := m.Acquire(class, Options{Reason: "job", Wait: 15 * time.Second})
			if aerr != nil {
				t.Errorf("waiter %d: %v", i, aerr)
				return
			}
			time.Sleep(time.Millisecond)
			if rerr := l.Release(); rerr != nil {
				t.Errorf("waiter %d release: %v", i, rerr)
				return
			}
			mu.Lock()
			completed++
			mu.Unlock()
		}(i)
	}

	waitAllRegistered(t, m, n, 3*time.Second)
	if err := holder.Release(); err != nil {
		t.Fatalf("release seed holder: %v", err)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if completed != n {
		t.Fatalf("only %d of %d same-process waiters completed — the rest deadlocked", completed, n)
	}
}

// The expired-wait test earlier in this file proves the record is REMOVED
// (its content is gone by the time Acquire returns). It does not prove that
// removal SURVIVES a concurrent reader — and every OTHER waiter's Waiters()
// call does exactly that (a plain os.ReadFile) against this same path, on
// every one of its poll ticks. On Windows a reader blocks a delete; this
// pins that registerWaiter's unregister (removeClaim, not a bare os.Remove)
// actually retries through it rather than merely being reviewed as if it
// did. Modelled on TestRenameReplacingSurvivesAConcurrentReader's tight
// reader-loop pattern, the proven way to reproduce the Windows race reliably
// without a sleep-based wait.
func TestWaiterUnregisterSurvivesAConcurrentReader(t *testing.T) {
	m := realClockManager(t)
	self, unregister := m.registerWaiter(ClassText, Options{Reason: "expiring"})
	if self.path == "" {
		t.Fatal("failed to register — test cannot proceed")
	}

	stop := make(chan struct{})
	var readerRunning sync.WaitGroup
	readerRunning.Add(1)
	go func() {
		defer readerRunning.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = os.ReadFile(self.path)
		}
	}()
	defer func() {
		close(stop)
		readerRunning.Wait()
	}()

	time.Sleep(5 * time.Millisecond) // let the reader actually start spinning
	unregister()

	if _, statErr := os.Stat(self.path); !os.IsNotExist(statErr) {
		t.Fatal("the waiter record survived removal under a concurrent reader — the Windows delete-retry path did not cover this call site")
	}
}

// FOUND VIA -count=10 (lead review point 1 verification, 2026-09-22): once
// refreshWaiter started rewriting a waiter's own file every poll tick, and
// every OTHER waiter reads that same file at the same cadence,
// TestMixedClassWaitersAreServedInArrivalOrder started failing intermittently
// — NOT from timing jitter (a diagnostic run confirmed the recorded SinceMs
// values stayed perfectly ordered every time) but because Waiters()'s
// os.ReadFile/os.Stat had no retry: a transient failure racing a concurrent
// rename silently excluded a live, correctly-refreshing, genuinely-earlier
// waiter from ONE reader's view — enough for a later waiter to wrongly
// compute itself as front-of-queue for that one tick and win the race. This
// pins the fix (readWaiterFile/statWaiterFile) directly, mirroring
// TestRenameReplacingSurvivesAConcurrentReader's own reader-vs-writer stress
// pattern but with the roles reversed: here a tight concurrent RENAMER must
// never make a READ give up.
func TestWaiterReadSurvivesAConcurrentRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "waiter.json")
	if err := os.WriteFile(path, []byte(`{"pid":1,"class":"text","since_ms":1}`), 0o666); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stop := make(chan struct{})
	var writerRunning sync.WaitGroup
	writerRunning.Add(1)
	go func() {
		defer writerRunning.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte(strconv.Itoa(i)), 0o666); err == nil {
				_ = renameReplacing(tmp, path)
			}
			i++
		}
	}()
	defer func() {
		close(stop)
		writerRunning.Wait()
	}()

	const iterations = 300
	for i := 0; i < iterations; i++ {
		if _, err := readWaiterFile(path); err != nil {
			t.Fatalf("readWaiterFile failed under a concurrent renamer (iteration %d): %v", i, err)
		}
		if _, err := statWaiterFile(path); err != nil {
			t.Fatalf("statWaiterFile failed under a concurrent renamer (iteration %d): %v", i, err)
		}
	}
}
