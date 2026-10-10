package pipeline

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// seatWaiterAt registers a refreshed ClassSeat waiter under the fixture's lease dir and
// returns the function that makes it leave. The waiter never claims; it only holds the
// front of the line, exactly as a blocked seat/text-load admission does. With no devices
// it is the whole node; with some it is queued for those cards only.
func seatWaiterAt(t *testing.T, root, reason string, devices ...string) (leave func()) {
	t.Helper()
	refresh, unregister := gpulease.RegisterSeatWaiter(filepath.Join(root, "gpu", "lease"), reason, devices)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				refresh()
			}
		}
	}()
	var once sync.Once
	leave = func() {
		once.Do(func() {
			close(stop)
			<-done
			unregister()
		})
	}
	t.Cleanup(leave)
	// The waiter's SinceMs has millisecond resolution and must strictly precede whatever the test
	// claims next: inside the SAME millisecond the order falls to the waiter files' random token
	// (waiterBefore), a coin flip, and a claim that wins it goes ahead of the waiter. Measured on the
	// whole-node test below without this gap: one run in five failed, on the branch's first commit too.
	time.Sleep(5 * time.Millisecond)
	return leave
}

// THE INCIDENT (register D-1xx-3, 2026-10-09): the media admission's first claim was a
// bare TryAcquire, so a call arriving while a waiter was already registered for the card
// won it in the gap before that waiter's next poll tick. Here every card is FREE and a
// seat waiter is in line: the call must queue (a resumable gpu_queued answer) and never
// start a render.
func TestMediaAdmissionQueuesBehindARegisteredWaiterOnFreeCards(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	seatWaiterAt(t, f.root, "transcribe voice_es.wav")
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("every card is free but a waiter is ahead in line: the call must queue, got ok=%v class=%q %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if got := f.started(); len(got) != 0 {
		t.Fatalf("the call ran on %v ahead of a registered waiter — the bare-claim defect is back", got)
	}
}

// The pipeline's whole-node lease with no wait reads a waiter ahead as gpu busy (the work
// is intact and queued), never as a broken call; once the waiter leaves, it is granted.
func TestWholeNodeLeaseWithNoWaitIsBusyBehindARegisteredWaiter(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	leave := seatWaiterAt(t, f.root, "transcribe voice_es.wav")
	devs, release, err := f.p.acquireWholeNode(context.Background(), "compose", time.Hour, 0, "")
	if err == nil {
		release()
		t.Fatalf("a no-wait whole-node lease won the free node ahead of a registered waiter (devices %v)", devs)
	}
	if !IsGPUBusy(err) {
		t.Fatalf("a waiter ahead on a free node must read as gpu busy, got: %v", err)
	}
	leave()
	_, release, err = f.p.acquireWholeNode(context.Background(), "compose", time.Hour, 0, "")
	if err != nil {
		t.Fatalf("once the waiter left, the no-wait whole-node lease must be granted: %v", err)
	}
	release()
}
