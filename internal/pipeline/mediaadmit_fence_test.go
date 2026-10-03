package pipeline

// A kept ComfyUI instance is stopped by the holder of the lease it was launched under, and stopped
// BY EPOCH: StopForLease(N) matches the markers that name N. A holder that has lost its lease (it
// was released from outside, or reclaimed after a suspend, and another lease holds the card now) is
// a straggler, and a straggler must not reach for the card: the instance a later lease reuses is
// that lease's (its marker is re-stamped on reuse, render/comfy-lifecycle.mjs), but the window
// before that is the straggler's to get wrong. This is the same rule Lease.Release already follows
// for the claim: a fenced-out holder leaves the current holder's things alone, and leaks rather
// than destroys.

import (
	"context"
	"testing"
	"time"
)

func TestAGrantStillHoldingItsLeaseStopsItsInstances(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	g, err := f.p.acquireMediaLease(context.Background(), "image-gen", time.Hour, 5*time.Second, singleCardNeed(f.cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	if got := f.stops.list(); len(got) != 1 || !got[0].held {
		t.Fatalf("stop calls = %+v, want one, made while the lease was still held", got)
	}
}

func TestAGrantWhoseLeaseWasTakenAwayStopsNoInstances(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	g, err := f.p.acquireMediaLease(context.Background(), "image-gen", time.Hour, 5*time.Second, singleCardNeed(f.cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	live := f.m.Leases()
	if len(live) != 1 {
		t.Fatalf("want one live lease, have %v", live)
	}
	if released, err := f.m.ReleaseByEpoch(live[0].Epoch); err != nil || !released { // taken away from outside
		t.Fatalf("release from outside: %v %v", released, err)
	}
	g.Release()
	if got := f.stops.list(); len(got) != 0 {
		t.Fatalf("a straggler stopped instances of a lease it no longer holds: %+v", got)
	}
	if n := mediaSlots.queued(); n != 0 {
		t.Errorf("%d parked waiter(s)", n)
	}
	// The slot is still given back: only the instance stop is skipped.
	if !mediaSlots.tryTake([]string{leaseIDOf(admitUUIDA)}) {
		t.Error("the card's in-process slot must be released even when the lease was lost")
	}
	mediaSlots.release([]string{leaseIDOf(admitUUIDA)})
}
