package main

// `gpu reserve --cards N` allocates AND claims. Since the claim became a gated attempt
// (register D-1xx-3, 2026-10-09) it can be refused for a waiter registered ahead of it, not
// only for a lease already held, and the loop has to treat that as what it is: a card taken
// from under the allocation, to be chosen around. The pre-ship review of D-1xx-3 found it ending the whole
// reserve instead, which broke every fan-out of `--cards 1` reserves over free cards.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	boxCard0 = "gpu-aaaa0000-x"
	boxCard2 = "gpu-cccc0000-x"
)

// seatWaiterOn registers a seat waiter for the given cards under the fixture's lease dir, the way
// a blocked text-load admission does, and returns the func that makes it leave (also run at the
// end of the test). It never claims; it only stands in line.
func seatWaiterOn(t *testing.T, cfg, reason string, cards ...string) (leave func()) {
	t.Helper()
	root := filepath.Dir(cfg)
	_, unregister := gpulease.RegisterSeatWaiter(filepath.Join(root, "gpu", "lease"), reason, cards)
	t.Cleanup(unregister)
	return unregister
}

// waitForWaiters blocks until n live waiters are registered, or fails the test.
func waitForWaiters(t *testing.T, m *gpulease.Manager, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Waiters()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d of %d waiters registered", len(m.Waiters()), n)
}

var oneCard = devicePlan{Auto: true, Min: 1, Max: 1, Source: "--cards 1"}

// The allocator reads the line. A card a waiter is registered for is not free to a newcomer even
// though nobody holds it, so a `--cards 1` reserve takes the card NOBODY is ahead on instead of
// finding out at the claim: one allocation, one claim, no retries.
func TestAReserveForOneCardTakesACardNoWaiterIsAheadOn(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "load seat on card 0", boxCard0)
	time.Sleep(5 * time.Millisecond) // the waiter's SinceMs strictly precedes the request

	builds := 0
	build := func() (gpulease.AllocInput, error) {
		builds++
		return buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
	}
	var out bytes.Buffer
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "fresh", TTL: time.Hour}, oneCard,
		0, build, &out, func(time.Duration) { t.Error("must not poll") }, time.Now)
	if err != nil {
		t.Fatalf("card 2 is free and nobody is ahead on it: the request must take it, not be turned away for card 0: %v (%s)", err, out.String())
	}
	defer func() { _ = lease.Release() }()
	if got := strings.Join(lease.Devices(), ","); got != boxCard2 {
		t.Fatalf("card 0 has a waiter ahead; the request must hold card 2, got %s", got)
	}
	if builds != 1 {
		t.Errorf("the allocator already saw the waiter: one read, not a claim loop (%d reads)", builds)
	}
}

// With a waiter ahead on every usable card the answer is the queue's, at once: fail-fast names who
// is ahead and the flag that queues, after ONE read of the host, not the sixteen a claim loop that
// cannot see the line spends before giving up (each read is a card-table exec and a seat probe).
func TestAReserveWithNoWaitFailsFastWhenEveryUsableCardHasAWaiterAhead(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "load seat on card 0", boxCard0)
	seatWaiterOn(t, cfg, "load seat on card 2", boxCard2)
	time.Sleep(5 * time.Millisecond)

	builds := 0
	build := func() (gpulease.AllocInput, error) {
		builds++
		return buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
	}
	var out bytes.Buffer
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "fresh", TTL: time.Hour}, oneCard,
		0, build, &out, func(time.Duration) {}, time.Now)
	if err == nil {
		_ = lease.Release()
		t.Fatal("a --wait 0 reserve won a card with a waiter registered ahead of it on every usable card")
	}
	if !errors.Is(err, gpulease.ErrStillQueued) || !strings.Contains(err.Error(), "--wait") || !strings.Contains(err.Error(), "load seat on card") {
		t.Fatalf("the refusal must be ErrStillQueued naming a waiter ahead and the --wait hint: %v", err)
	}
	if builds != 1 {
		t.Errorf("one read of the host, then the queue's answer: %d reads", builds)
	}
}

// A waiter that registers between the allocation and the claim is a claim lost, like another
// reserve's: the request allocates again with the waiter visible and takes the other card. The
// pre-ship review (D-1xx-3) found it ending the reserve instead (return on ErrStillQueued where the ErrHeld
// arm beside it continued), the shape every fan-out of `--cards 1` reserves over free cards hits.
func TestAReserveThatLosesTheClaimToAWaiterAllocatesAgain(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	builds := 0
	build := func() (gpulease.AllocInput, error) {
		in, err := buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
		builds++
		if builds == 1 {
			// The snapshot is taken (card 0 reads free); now a waiter registers for it.
			seatWaiterOn(t, cfg, "load seat on card 0", boxCard0)
			time.Sleep(5 * time.Millisecond)
		}
		return in, err
	}
	var out bytes.Buffer
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "fresh", TTL: time.Hour}, oneCard,
		0, build, &out, func(time.Duration) { t.Error("must re-pick, not poll") }, time.Now)
	if err != nil {
		t.Fatalf("card 2 is free: a request that lost card 0 to a waiter must allocate again, not fail: %v", err)
	}
	defer func() { _ = lease.Release() }()
	if got := strings.Join(lease.Devices(), ","); got != boxCard2 {
		t.Fatalf("the waiter is ahead on card 0, so the request takes card 2, got %s", got)
	}
	if builds < 2 {
		t.Errorf("the allocator must have run again after the lost claim: %d reads", builds)
	}
}

// The retries are bounded when the allocator CANNOT see the line (a view of live state that never
// shows the waiter): after maxAutoClaimRetries re-allocations the request queues on the set it
// last picked, which with no wait is the refusal naming the waiter, never a spin and never a win.
func TestAReserveThatNeverSeesTheWaiterEndsInTheQueuesAnswer(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "load seat on card 0", boxCard0)
	time.Sleep(5 * time.Millisecond)
	builds := 0
	blind := func() (gpulease.AllocInput, error) {
		builds++
		in, err := buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
		if err != nil {
			return in, err
		}
		delete(in.Claimed, boxCard0) // the view never shows the waiter's claim
		return in, nil
	}
	var out bytes.Buffer
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "fresh", TTL: time.Hour}, oneCard,
		0, blind, &out, func(time.Duration) {}, time.Now)
	if err == nil {
		_ = lease.Release()
		t.Fatal("a request that never saw the waiter won the card ahead of it")
	}
	if !errors.Is(err, gpulease.ErrStillQueued) {
		t.Fatalf("expected the queue's answer, got: %v", err)
	}
	if builds != maxAutoClaimRetries+1 {
		t.Errorf("allocate, then at most %d re-allocations, then queue: %d reads", maxAutoClaimRetries, builds)
	}
}
