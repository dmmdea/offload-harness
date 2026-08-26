package modelaffinity

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// armLease points the process-wide gate at a fresh state root and hands back a
// Manager on the SAME directory, so a test can hold a real lease against the real
// inspection path rather than a reconstruction of it. The gate is disarmed on
// cleanup: it is process-wide, and a leaked arming would silently change every
// later test in this package.
func armLease(t *testing.T) *gpulease.Manager {
	t.Helper()
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	if err := SetGPULease("", root); err != nil {
		t.Fatalf("arm gate: %v", err)
	}
	t.Cleanup(func() {
		leaseMu.Lock()
		leaseDir = ""
		leaseMu.Unlock()
	})
	return m
}

// admitFast asserts an admission was NOT gated: it came back well inside the poll
// interval, so it cannot have gone round the wait loop even once.
func admitFast(t *testing.T, base, model string) Ticket {
	t.Helper()
	start := time.Now()
	tk, err := Admit(context.Background(), base, model, 5*time.Second)
	if err != nil {
		t.Fatalf("Admit(%s) = %v, want admitted", model, err)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("Admit(%s) took %s — it waited on the card when it should not have", model, el)
	}
	return tk
}

// An armed gate over a card nobody holds must be invisible.
func TestAdmitProceedsWhenNoLeaseHeld(t *testing.T) {
	armLease(t)
	tk := admitFast(t, "http://gate-idle", "m")
	tk.Release()
}

// THE DEFECT, as a test: a media render owns the card, and a text admission that
// would make llama-swap load a different model must not get one.
func TestMediaLeaseBlocksAModelLoad(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "hero render", Origin: "pipeline"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	start := time.Now()
	_, aerr := Admit(context.Background(), "http://gate-media", "qwen3.8-27b", 300*time.Millisecond)
	if aerr == nil {
		t.Fatal("Admit succeeded while a media render held the card — the load gate is inert")
	}
	var le *LeaseError
	if !errors.As(aerr, &le) {
		t.Fatalf("Admit error = %v (%T), want a *LeaseError", aerr, aerr)
	}
	if le.Class != gpulease.ClassMedia || le.Reason != "hero render" || le.Want != "qwen3.8-27b" {
		t.Fatalf("LeaseError = %+v, want the hero render named as holder", le)
	}
	// pipeline.classifyErr buckets infra errors by substring, and a held card is
	// congestion — which that classifier spells "timeout". Pinned so a reword
	// cannot silently reclassify it in the ledger.
	if !strings.Contains(le.Error(), "timeout") {
		t.Fatalf("LeaseError.Error() = %q, must contain \"timeout\" for pipeline.classifyErr", le.Error())
	}
	if !errors.Is(aerr, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, DeadlineExceeded) = false; callers that branch on the cause would miss it")
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("Admit gave up after %s — it refused instead of waiting out its budget", el)
	}
}

// A render finishing mid-wait must admit the caller, not make it serve out the
// bound: waiting exists so short renders cost latency, not answers.
func TestMediaLeaseReleasedMidWaitAdmits(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "quick render"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = l.Release()
	}()
	tk, aerr := Admit(context.Background(), "http://gate-release", "m", 30*time.Second)
	if aerr != nil {
		t.Fatalf("Admit = %v, want admission once the render released the card", aerr)
	}
	tk.Release()
}

// A request joining the batch of the model llama-swap is ALREADY serving cannot
// move VRAM, so it must not be gated — that is what keeps a burst of the resident
// model from paying a lease read each, and what keeps in-flight work completing
// when a render starts beside it.
func TestJoiningTheResidentBatchIgnoresTheLease(t *testing.T) {
	m := armLease(t)
	const base = "http://gate-join"
	first := admitFast(t, base, "resident")
	defer first.Release()

	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	second := admitFast(t, base, "resident")
	second.Release()
}

// A ClassText reservation is a benchmark holding the text tier steady. Its holder
// unloads nothing, so a switch under it costs a measurement rather than the
// machine — and blocking every interactive call for the length of an eval would be
// a larger regression than the one it prevents. Deliberately not gated.
func TestTextReservationDoesNotBlockText(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "kv bench"})
	if err != nil {
		t.Fatalf("acquire text: %v", err)
	}
	defer func() { _ = l.Release() }()
	tk := admitFast(t, "http://gate-text", "m")
	tk.Release()
}

// `gpu reserve --class media -- local-offload …` runs the harness as a CHILD of the
// holder. A text call in that child must not queue behind its own parent.
func TestInheritedLeaseEpochExemptsThisProcess(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "reserved shell"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()
	info := m.Inspect()
	if !info.Held || info.Epoch == 0 {
		t.Fatalf("Inspect = %+v, want a held lease with a fencing epoch", info)
	}
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch, 10))
	tk := admitFast(t, "http://gate-inherit", "m")
	tk.Release()

	// A STALE epoch from a lease that has since been handed on must not exempt
	// anything — presence of the variable is not the test, equality is.
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch+1, 10))
	if _, aerr := Admit(context.Background(), "http://gate-inherit-stale", "m", 200*time.Millisecond); aerr == nil {
		t.Fatal("a stale GPU_LEASE_EPOCH exempted an admission; presence must not stand in for equality")
	}
}

// THE HOLE THIS CLOSES. A waiter parks while the card is free and is promoted a
// batch later — by which time a render may own it. The promotion IS the model
// switch, so the card is re-read at wake rather than trusted from park time.
func TestPromotedSwitchRechecksTheCard(t *testing.T) {
	m := armLease(t)
	const base = "http://gate-promote"
	held := admitFast(t, base, "model-a")

	type outcome struct {
		tk  Ticket
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		tk, err := Admit(context.Background(), base, "model-b", time.Second)
		done <- outcome{tk, err}
	}()
	waitForQueue(t, base, 1)

	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render that started mid-park"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	held.Release() // promotes model-b: a switch, and the card is now taken

	select {
	case got := <-done:
		if got.err == nil {
			got.tk.Release()
			t.Fatal("a promoted switch was admitted while a render held the card — the recheck at wake is missing")
		}
		var le *LeaseError
		if !errors.As(got.err, &le) {
			t.Fatalf("promoted waiter error = %v (%T), want a *LeaseError", got.err, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("promoted waiter never returned")
	}
	// The refused admission must be GIVEN BACK, or the base is wedged for good.
	if n := inFlight(base); n != 0 {
		t.Fatalf("inFlight(%s) = %d after a refused promotion, want 0 — the admission leaked", base, n)
	}
}

// A PROMOTED batch has incremented inflight but has not sent anything yet, so while
// it is still waiting for the card its model is NOT loaded. A newcomer naming that
// model must therefore not be handed a join: it would slip past the machine-wide gate
// and force exactly the load the promoted waiter is waiting to avoid.
func TestJoinIsRefusedWhileAPromotedBatchWaitsForTheCard(t *testing.T) {
	m := armLease(t)
	const base = "http://gate-pending"
	held := admitFast(t, base, "model-a")

	done := make(chan error, 1)
	go func() {
		_, err := Admit(context.Background(), base, "model-b", 4*time.Second)
		done <- err
	}()
	waitForQueue(t, base, 1)

	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	held.Release() // promotes model-b, which now blocks in admitPromoted
	waitForPending(t, base, 1)

	// The newcomer names the SAME model as the promoted batch. Without the pending
	// guard tryJoin hands it an admission instantly and it never reads the lease.
	start := time.Now()
	tk, jerr := Admit(context.Background(), base, "model-b", 300*time.Millisecond)
	if jerr == nil {
		tk.Release()
		t.Fatalf("a newcomer joined a promoted batch after %s while a render held the card — "+
			"inflight > 0 was treated as proof of residency", time.Since(start))
	}
	var le *LeaseError
	if !errors.As(jerr, &le) {
		t.Fatalf("newcomer error = %v (%T), want a *LeaseError", jerr, jerr)
	}
	if err := <-done; err == nil {
		t.Fatal("the promoted waiter itself was admitted while the render held the card")
	}
	if n := pendingLoads(base); n != 0 {
		t.Fatalf("pendingLoads(%s) = %d after both waits ended, want 0 — the counter leaked", base, n)
	}
}

// An unarmed gate (no config.Load in this process) is inert by construction.
func TestUnarmedGateIsInert(t *testing.T) {
	leaseMu.Lock()
	leaseDir = ""
	leaseMu.Unlock()
	if err := awaitCard(context.Background(), "http://gate-unarmed", "m", time.Now().Add(time.Millisecond)); err != nil {
		t.Fatalf("awaitCard on an unarmed gate = %v, want nil", err)
	}
}

// waitForPending blocks until base has n admissions granted but still waiting for
// the card, so a test can act at exactly that moment instead of sleeping and hoping.
func waitForPending(t *testing.T, base string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pendingLoads(base) < n {
		if time.Now().After(deadline) {
			t.Fatalf("pendingLoads(%s) never reached %d", base, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForQueue blocks until base has n parked waiters, so a test advances on the
// gate's OWN state instead of sleeping and hoping.
func waitForQueue(t *testing.T, base string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for queueDepth(base) < n {
		if time.Now().After(deadline) {
			t.Fatalf("queueDepth(%s) never reached %d", base, n)
		}
		time.Sleep(time.Millisecond)
	}
}
