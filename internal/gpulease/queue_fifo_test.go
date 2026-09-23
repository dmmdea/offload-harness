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
