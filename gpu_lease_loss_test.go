package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Register C-59: a DRAINING reserve whose lease is taken away while it drains
// must not exit with "stamping the lease after the drain". The wrapper form
// waited out the whole drain on a card it no longer owned, died at the restamp,
// and its command never started; the log read
//
//	gpu: NOT warming <seat> back: the card is no longer ours (lease is gone (epoch N); ...)
//	error: stamping the lease after the drain: gpulease: restamp: the lease is gone (epoch N)
//
// A reserve that loses its lease says so, takes its place in the line again
// inside the queue budget it already had, and only then drains, clears the seat
// and runs its command. It never unloads the seat under the lease that took
// its card.

// TestHelperRecordEpoch is the wrapped command of the loss tests: a no-op unless
// the parent names a file, then it records the lease epoch it inherited, so a
// test can tell that the command ran and under WHICH lease.
func TestHelperRecordEpoch(t *testing.T) {
	path := os.Getenv("LO_HELPER_EPOCH_FILE")
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(os.Getenv("GPU_LEASE_EPOCH")), 0o644)
}

// lossRun is one wrapper reserve (--drain --unload-seat) started against a seat
// that has a request in flight, so its lease sits DRAINING until the test lets
// the request finish.
type lossRun struct {
	f          *orderedSwap
	m          *gpulease.Manager
	epochFile  string
	firstEpoch uint64
	done       chan error
}

func startDrainingReserve(t *testing.T) *lossRun {
	t.Helper()
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1)
	cfgPath, m := handoffFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 300 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	run := &lossRun{f: f, m: m, epochFile: epochFile, done: make(chan error, 1)}
	go func() {
		args := append([]string{"--config", cfgPath, "--wait", "30s", "--drain", "--unload-seat", "--reason", "drainer"}, helperCmdFor("TestHelperRecordEpoch")...)
		run.done <- runGPUReserve(args)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info := m.Inspect(); info.Held && info.Draining {
			run.firstEpoch = info.Epoch
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the reserve never took a draining lease")
	return nil
}

// wait returns the reserve's own result, or fails the test when it never ends.
func (r *lossRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("the reserve never returned")
		return nil
	}
}

// assertRanUnderANewLease is what a reserve that lost its lease and queued
// again must show: its command ran under a LATER epoch, the seat was cleared
// once (after the re-acquire, never under the lease that took the card), the
// warm-back was paid at the end, and the card is free.
func (r *lossRun) assertRanUnderANewLease(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(r.epochFile)
	if err != nil {
		t.Fatalf("the wrapped command never ran: %v", err)
	}
	got, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if perr != nil || got <= r.firstEpoch {
		t.Fatalf("the command must run under a lease taken AFTER the lost one (lost %d), recorded %q", r.firstEpoch, string(b))
	}
	if n := r.f.unloads.Load(); n != 1 {
		t.Errorf("the seat must be cleared exactly once, after the re-acquire: unloads=%d events=%v", n, r.f.log())
	}
	if n := r.f.warms.Load(); n != 1 || r.m.SeatWarmOwed() != "" {
		t.Errorf("the last holder pays the warm once and clears the marker: warms=%d owed=%q", n, r.m.SeatWarmOwed())
	}
	if info := r.m.Inspect(); info.Held {
		t.Errorf("the reserve must release on exit; still held: %+v", info)
	}
}

// THE REPORTED SHAPE: the lease record is simply gone while the drain waits
// ("the lease is gone"). On main the reserve waited the drain out and died.
func TestADrainingReserveThatLosesItsLeaseQueuesAgainInsteadOfDyingAtTheRestamp(t *testing.T) {
	run := startDrainingReserve(t)
	if released, err := run.m.ReleaseByEpoch(run.firstEpoch); err != nil || !released {
		t.Fatalf("could not take the lease away: released=%v err=%v", released, err)
	}
	time.Sleep(700 * time.Millisecond) // a couple of heartbeats: the holder can notice
	run.f.inflight.Store(0)            // the request the drain waited on finishes
	if err := run.wait(t); err != nil {
		t.Fatalf("a drainer that lost its lease must queue again, not exit: %v", err)
	}
	run.assertRanUnderANewLease(t)
}

// THE CONCURRENT ACQUIRE: another process takes the card the moment the record
// goes ("fenced out"). The reserve must leave that holder's lease and seat
// alone, queue behind it, and run after it.
func TestADrainingReserveQueuesBehindTheAcquirerThatTookItsCard(t *testing.T) {
	run := startDrainingReserve(t)
	if _, err := run.m.ReleaseByEpoch(run.firstEpoch); err != nil {
		t.Fatal(err)
	}
	intruder, err := run.m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "concurrent acquire", TTL: time.Hour})
	if err != nil {
		t.Fatalf("the concurrent acquire could not take the free card: %v", err)
	}
	run.f.inflight.Store(0)
	var unloadsUnderIt int64
	var intruderStillOurs error
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(3 * time.Second) // outlasts the drain's two idle reads
		unloadsUnderIt = run.f.unloads.Load()
		intruderStillOurs = intruder.Check()
		_ = intruder.Release()
	}()
	err = run.wait(t)
	<-released
	if unloadsUnderIt != 0 {
		t.Errorf("the seat was unloaded %d time(s) while another lease held the card", unloadsUnderIt)
	}
	if intruderStillOurs != nil {
		t.Errorf("the reserve disturbed the lease that took its card: %v", intruderStillOurs)
	}
	if err != nil {
		t.Fatalf("a drainer fenced out by another acquire must queue behind it, not exit: %v", err)
	}
	run.assertRanUnderANewLease(t)
}

// One failed heartbeat write is not a lost lease. The drain's heartbeat used to
// end for good on the FIRST Renew error — a write that failed once, with the
// record still ours — and a multi-hour drain then sat un-heartbeated until the
// stale heartbeat and the expired --for window let the next acquirer reclaim
// it. Only a lease that is actually gone ends the loop.
func TestTheDrainHeartbeatSurvivesATransientWriteFailure(t *testing.T) {
	_, m := leaseFixture(t)
	lease, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "long drain", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	// A directory in the heartbeat's scratch name fails the write and leaves the
	// record alone.
	blocker := filepath.Join(lease.Dir(), "hb."+strconv.FormatUint(lease.Epoch(), 10)+".tmp")
	if err := os.Mkdir(blocker, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := lease.Renew(); err == nil {
		t.Fatal("precondition: the heartbeat write must fail while the scratch name is blocked")
	}
	if err := lease.Check(); err != nil {
		t.Fatalf("precondition: the lease must still be ours: %v", err)
	}
	before := m.Inspect().HeartbeatAt
	stop := renewWhile(lease, 20*time.Millisecond)
	defer stop()
	time.Sleep(200 * time.Millisecond) // several failing ticks
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Inspect().HeartbeatAt.After(before) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the heartbeat never resumed after one failed write: the loop ended on the first failed tick")
}

// --wait 0 asks to fail fast, and a reserve that lost its lease keeps that
// word: one try for the card, and when another holder has it the error says so.
// It is not the restamp failure, the seat is untouched, and the holder that
// took the card is left alone.
func TestADrainingReserveThatLosesItsLeaseFailsLoudlyWhenAskedNotToQueue(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1)
	cfgPath, m := handoffFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 300 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	done := make(chan error, 1)
	go func() {
		args := append([]string{"--config", cfgPath, "--wait", "0", "--drain", "--unload-seat", "--reason", "drainer"}, helperCmdFor("TestHelperRecordEpoch")...)
		done <- runGPUReserve(args)
	}()
	var first uint64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info := m.Inspect(); info.Held && info.Draining {
			first = info.Epoch
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if first == 0 {
		t.Fatal("the reserve never took a draining lease")
	}
	if _, err := m.ReleaseByEpoch(first); err != nil {
		t.Fatal(err)
	}
	intruder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "concurrent acquire", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = intruder.Release() }()
	f.inflight.Store(0)
	var rerr error
	select {
	case rerr = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the reserve never returned")
	}
	if rerr == nil {
		t.Fatal("--wait 0 against a held card must fail, not run the command")
	}
	if strings.Contains(rerr.Error(), "stamping the lease after the drain") {
		t.Fatalf("a lost lease must not surface as the restamp failure: %v", rerr)
	}
	for _, want := range []string{"the lease was lost during the drain", "concurrent acquire"} {
		if !strings.Contains(rerr.Error(), want) {
			t.Errorf("the error must say %q; got: %v", want, rerr)
		}
	}
	if err := intruder.Check(); err != nil {
		t.Errorf("the reserve disturbed the lease that took its card: %v", err)
	}
	if f.unloads.Load() != 0 {
		t.Errorf("the seat was unloaded %d time(s) with no lease of this reserve's", f.unloads.Load())
	}
	if _, err := os.Stat(epochFile); err == nil {
		t.Error("the wrapped command ran without a lease")
	}
}

// A seat fault is not a lost lease: a drain that misses its deadline while the
// lease is still ours returns the drain's own error, releases, and does not
// queue again.
func TestAWrapperDrainThatMissesItsDeadlineStillFailsWithoutRequeueing(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	f.inflight.Store(1) // never finishes
	cfgPath, m := handoffFixture(t, f)
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfgPath, "--wait", "30s", "--drain", "--drain-timeout", "300ms", "--unload-seat", "--reason", "stuck"}, helperCmdFor("TestHelperRecordEpoch")...)
	err := runGPUReserve(args)
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("a drain that misses its deadline must fail with the drain's error, got %v", err)
	}
	if strings.Contains(err.Error(), "the lease was lost") {
		t.Fatalf("the lease was never lost: %v", err)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the failed reserve must release the card: %+v", info)
	}
	if f.unloads.Load() != 0 {
		t.Errorf("a failed drain must not unload the seat: unloads=%d", f.unloads.Load())
	}
	if _, err := os.Stat(epochFile); err == nil {
		t.Error("the wrapped command ran after a failed drain")
	}
}
