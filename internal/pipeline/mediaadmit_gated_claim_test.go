package pipeline

// The media admission's claim on a card it found free is a GATED attempt (register D-1xx-3,
// 2026-10-09), not a bare TryAcquire. The allocation before it already reads the line
// (gpualloc.QueuedClaims), so the gate only closes the window between the allocation and the
// claim: a waiter that registers there is ahead of the claim, and the claim must say so.
//
// What the gate may NOT do is cost a call that resumed a place its place. Acquire consumes the
// token a waiter resumes, so an attempt that handed the token over and then lost would send the
// call to the queue below as a new arrival (the 0.178.0 review). These tests pin the gate itself
// (restoring the bare claim there lets the call win ahead of the waiter) and that a lost attempt
// leaves the place whole.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// afterTheAllocation runs fn once, from inside the allocator's last read of live state. That read
// comes after the claims the allocation counts (the lease directory and the line) have been read,
// so whatever fn does is exactly what lands BETWEEN the allocation and the claim. It returns how
// many times the allocator has read live state.
func (f *admitFixture) afterTheAllocation(fn func()) (reads *atomic.Int64) {
	f.t.Helper()
	reads = &atomic.Int64{}
	var once sync.Once
	f.p.alloc.Presence = func(config.Config) (bool, bool) {
		reads.Add(1)
		once.Do(fn)
		return true, f.away
	}
	return reads
}

// A waiter that registers between the allocation and the claim is ahead of the claim. The call
// that found the card free must queue behind it (a resumable gpu_queued answer) and start nothing:
// the bare claim it used to make wins the card ahead of a waiter registered before it.
func TestACallYieldsToAWaiterThatRegistersBetweenTheAllocationAndTheClaim(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	pinned := leaseIDOf(admitUUIDC)
	var leave func()
	f.afterTheAllocation(func() {
		leave = seatWaiterAt(t, f.root, "transcribe voice_es.wav", pinned)
		time.Sleep(5 * time.Millisecond) // the waiter's SinceMs strictly precedes the claim's
	})
	f.letRunnersGo() // were the call to win the card, it would run to completion at once

	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("a waiter was registered for the card before the claim: the call must queue, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("the call ran on %v ahead of a registered waiter — the bare-claim defect is back", got)
	}
	if leave != nil {
		leave()
	}
}

// A call that resumed a place and loses its first attempt keeps the place for the wait that
// follows: the waiter it queues as carries the token and the arrival time the token had, so the
// call is served ahead of everyone who arrived after it. The card here is taken between the
// allocation and the claim (the shape of a lost race); an explicit-card call sets no arrival time
// of its own, so the place is the token's or it is gone.
func TestAResumedCallThatLosesItsFirstAttemptKeepsItsPlaceInTheQueue(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
		c.ComfyCudaDevice = "2"
		c.GPUWaitMs = 3000
	}})
	pinned := leaseIDOf(admitUUIDC)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{pinned}}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var holder *gpulease.Lease
	took := make(chan struct{})
	f.afterTheAllocation(func() {
		defer close(took)
		l, herr := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "took the card first", TTL: time.Hour, Devices: []string{pinned}})
		if herr != nil {
			t.Errorf("could not take the card between the allocation and the claim: %v", herr)
			return
		}
		holder = l
	})
	f.letRunnersGo()

	ch := f.image(map[string]any{"waiter_token": tok.ID})
	select {
	case <-took:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached its claim")
	}
	// The lost attempt registers for an instant of its own and is gone; what is left on the card
	// after that is the place the call waits in.
	time.Sleep(300 * time.Millisecond)
	var queued gpulease.Waiter
	for _, w := range f.m.Waiters() {
		if len(w.Devices) == 1 && w.Devices[0] == pinned {
			queued = w
		}
	}
	if queued.PID == 0 {
		t.Fatal("the call that lost its first attempt never took a place in line")
	}
	if queued.Token != tok.ID || queued.SinceMs != tok.SinceMs {
		t.Fatalf("the call queued as a NEW arrival (token %q, since %d), but it had a place (token %q, since %d): the lost attempt spent it",
			queued.Token, queued.SinceMs, tok.ID, tok.SinceMs)
	}
	if holder == nil {
		t.Fatal("setup: the card was never taken in the window")
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	if r := f.await(ch); !r.OK {
		t.Fatalf("the card freed, the call must be served: class=%q %s", r.Meta.ErrClass, r.Reason)
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) after the call was served: its place is spent", n)
	}
}

// A call that resumed a place, on a free box, is served by its FIRST gated attempt. Its own token
// is older than the attempt, and a token blocks every waiter that arrived after it, so an attempt
// that did not carry the place's arrival time would be refused for the call's own place, and an
// auto call would burn all its re-allocations (each a card-table read) before it queued behind
// nothing at all.
func TestAResumedAutoCallIsServedByItsFirstGatedAttempt(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{leaseIDOf(admitUUIDA)}}, time.Now().Add(-5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reads := f.afterTheAllocation(func() {})
	f.letRunnersGo()

	if r := f.await(f.image(map[string]any{"waiter_token": tok.ID})); !r.OK {
		t.Fatalf("the box is free, the resumed call must be served: class=%q %s", r.Meta.ErrClass, r.Reason)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the allocator read live state %d times: the first attempt was refused for the call's own place and it chose again", n)
	}
}
