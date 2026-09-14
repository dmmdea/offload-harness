package modelaffinity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A DRAINING text hold cordons the seat: no NEW run starts, while the requests
// of runs already in flight are admitted exactly as under a plain text lease.
// Both halves are the fix (0.117.0, register D-93): without the first a drain
// never converges under several sessions; without the second it blocks the
// very run it is waiting for.
func TestADrainingTextHoldRefusesNewRunsButAdmitsRunningWork(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "arm B", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	// Running work: a request on the seat is admitted at once.
	tk := admitFast(t, "http://127.0.0.1:1", "agent-pool")
	tk.Release()

	// New work: the run slot is refused after its deadline, naming the drain.
	start := time.Now()
	err = AwaitRunSlot(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(30*time.Millisecond))
	var le *LeaseError
	if !errors.As(err, &le) || !le.Draining {
		t.Fatalf("a new run under a draining hold must be refused with a draining LeaseError, got %v", err)
	}
	if el := time.Since(start); el < 30*time.Millisecond {
		t.Fatalf("the run slot returned after %s, before its deadline", el)
	}
	for _, want := range []string{"gpu-lease timeout", "draining the seat", "no new run starts", "running work is not interrupted", "arm B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must carry %q: %v", want, err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause must unwrap to the deadline: %v", err)
	}
}

// Once the drain completes the holder restamps EXCLUSIVE: then both a new run
// and a running request wait (the pre-0.117.0 exclusive rule, unchanged).
func TestAnExclusiveHoldRefusesBothNewRunsAndRequests(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "arm B", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if err := l.Restamp(func(meta *gpulease.Meta) { meta.Draining = false; meta.Exclusive = true }); err != nil {
		t.Fatal(err)
	}
	if err := AwaitRunSlot(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(10*time.Millisecond)); err == nil {
		t.Fatal("a new run under an exclusive hold must wait")
	}
	if _, err := Admit(context.Background(), "http://127.0.0.1:1", "agent-pool", 20*time.Millisecond); err == nil {
		t.Fatal("a request under an exclusive hold must wait")
	}
}

// A draining hold releases mid-wait: the queued run starts.
func TestARunSlotOpensWhenTheDrainingHoldReleases(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "short", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = l.Release()
	}()
	if err := AwaitRunSlot(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("the run slot must open once the hold releases: %v", err)
	}
}

// A plain text lease (no drain, no exclusive) gates nothing — as before.
func TestAPlainTextLeaseAdmitsNewRuns(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "bench that unloads nothing", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	start := time.Now()
	if err := AwaitRunSlot(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("a plain text lease must not gate a run slot; waited %s", el)
	}
}
