package pipeline

// A warm batch that must wait for a busy lane keeps its place in line (P0 plan S3, fix round; ADR 0083 decision 13): when no
// roster node renders the recipe the batch cannot be relieved by the fleet, so it waits for the lane instead of ending
// `queued` after the pipeline's own 90 s window. The wait is the lease queue's, so the place is kept, and it ends the moment
// the caller's context does. Everything runs on the card-scoped host fixture of the media admission tests.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func heldCardFixture(t *testing.T) (*admitFixture, *gpulease.Lease) {
	t.Helper()
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
		c.ComfyCudaDevice = "2" // the default binding renders on card C
		c.ImageGenFamily = "krea2"
	}})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	f.letRunnersGo()
	return f, holder
}

func oneJob(f *admitFixture, name string) []ImageBatchJob {
	return []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, name)}}
}

// The fixture's window is 300 ms. The plain batch ends busy at it; the waiting one is still in line when the holder lets go
// 900 ms in, and then runs: the renderer is launched under the lease it was granted.
func TestRunImageBatchWaitingKeepsItsPlaceUntilTheCardFrees(t *testing.T) {
	f, holder := heldCardFixture(t)

	if _, err := f.p.RunImageBatchAs(context.Background(), "", oneJob(f, "control.png")); err == nil || !IsGPUBusy(err) {
		t.Fatalf("control: the plain batch must end busy at the pipeline's own window, got %v", err)
	}
	if n := len(f.started()); n != 0 {
		t.Fatalf("control: %d renderer(s) started under a held card", n)
	}

	go func() {
		time.Sleep(900 * time.Millisecond)
		_ = holder.Release()
	}()
	start := time.Now()
	_, err := f.p.RunImageBatchWaiting(context.Background(), "", oneJob(f, "held.png"), 30*time.Second)
	if IsGPUBusy(err) {
		t.Fatalf("the waiting batch gave up as busy after %v: it must keep its place", time.Since(start))
	}
	if d := time.Since(start); d < 600*time.Millisecond {
		t.Errorf("returned after %v, before the holder released: it did not wait", d)
	}
	if n := len(f.started()); n != 1 {
		t.Errorf("%d renderer(s) started, want exactly one once the card freed", n)
	}
}

// A wait of zero is the pipeline's own window, so the plain call is the one it always was.
func TestRunImageBatchWaitingWithNoWaitIsThePlainBatch(t *testing.T) {
	f, _ := heldCardFixture(t)
	if _, err := f.p.RunImageBatchWaiting(context.Background(), "", oneJob(f, "zero.png"), 0); err == nil || !IsGPUBusy(err) {
		t.Fatalf("a zero wait must end busy at the pipeline's window, got %v", err)
	}
}

// An interrupt ends the wait at once, says so, and leaves no lease behind: the abandoned wait that gets the card later gives
// it straight back.
func TestRunImageBatchWaitingEndsOnInterruptAndLeavesNoLease(t *testing.T) {
	f, holder := heldCardFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	errc := make(chan error, 1)
	go func() {
		_, err := f.p.RunImageBatchWaiting(ctx, "", oneJob(f, "interrupted.png"), time.Hour)
		errc <- err
	}()
	var err error
	select {
	case err = <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupt did not end the wait")
	}
	if !errors.Is(err, context.Canceled) || IsGPUBusy(err) {
		t.Fatalf("an interrupted wait must say it was interrupted (not busy), got %v", err)
	}
	if n := len(f.started()); n != 0 {
		t.Errorf("%d renderer(s) started by a batch that was interrupted", n)
	}

	// Let go of the card: the abandoned wait may take it, and must release it at once.
	_ = holder.Release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "after", TTL: time.Minute, Devices: []string{leaseIDOf(admitUUIDC)}})
		if err == nil {
			_ = l.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card was never free again after the interrupted batch: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(f.started()); n != 0 {
		t.Errorf("%d renderer(s) started after the interrupt", n)
	}
}

// The wait runs beside the caller, so a card the queue grants after the caller left has nobody to release it: it is given
// back at once. A grant the caller is still waiting for is returned to it, and nothing is released.
func TestHoldAcquireGivesBackAGrantThatArrivesAfterTheCallerLeft(t *testing.T) {
	released := make(chan struct{}, 2)
	grant := mediaGrant{Release: func() { released <- struct{}{} }}

	// the caller leaves first
	ctx, cancel := context.WithCancel(context.Background())
	arrive := make(chan struct{})
	got := make(chan error, 1)
	go func() {
		_, err := holdAcquire(ctx, func() (mediaGrant, error) { <-arrive; return grant, nil })
		got <- err
	}()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the caller must be told it was interrupted, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller was kept waiting for a card after its context ended")
	}
	select {
	case <-released:
		t.Fatal("the grant was released before it arrived")
	default:
	}
	close(arrive)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("a grant that arrived after the caller left was never given back: the card stays held with nobody to release it")
	}

	// the caller is still there: it gets the grant, and nothing is released
	g, err := holdAcquire(context.Background(), func() (mediaGrant, error) { return grant, nil })
	if err != nil || g.Release == nil {
		t.Fatalf("a grant the caller waited for must reach it, got %v", err)
	}
	select {
	case <-released:
		t.Fatal("a grant handed to its caller was released")
	case <-time.After(100 * time.Millisecond):
	}
	// an error from the acquire reaches the caller as it is
	boom := errors.New("busy")
	if _, err := holdAcquire(context.Background(), func() (mediaGrant, error) { return mediaGrant{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("the acquire's own error must reach the caller, got %v", err)
	}
}
