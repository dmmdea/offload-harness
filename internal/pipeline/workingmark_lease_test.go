package pipeline

// The lease path is how nearly every long-call lane marks its PAIR card working (core.MarkWorking, in the grant): a call
// queued behind another job's lease is waiting, not working, and its card must say queued until it holds the card. The
// reachability pin in internal/pairworkloads (TestOnlyTranscribeNeverMarksWorking) can only say a lane reaches a mark
// SOMEWHERE; this is the behaviour behind the lane that takes a lease, in both directions.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestAMediaCallMarksItsCardWorkingOnlyOnceItHoldsTheCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	holder := hold(t, f, "a bench render", leaseIDOf(admitUUIDA))
	var marks atomic.Int64
	ctx := core.WithWorkingMark(context.Background(), func() { marks.Add(1) })

	if g, err := f.p.acquireMediaLease(ctx, "image-gen", time.Hour, 0, singleCardNeed(f.cfg, "")); err == nil {
		g.Release()
		t.Fatal("control: the pinned card is held by another lease, so the call must not be granted")
	}
	if got := marks.Load(); got != 0 {
		t.Errorf("a call queued behind another lease is waiting, not working: its card was marked running %d time(s)", got)
	}

	_ = holder.Release()
	g, err := f.p.acquireMediaLease(ctx, "image-gen", time.Hour, 5*time.Second, singleCardNeed(f.cfg, ""))
	if err != nil {
		t.Fatalf("the card is free now: %v", err)
	}
	defer g.Release()
	if got := marks.Load(); got != 1 {
		t.Errorf("a call that holds its card marks its PAIR card running once, marked %d", got)
	}
}
