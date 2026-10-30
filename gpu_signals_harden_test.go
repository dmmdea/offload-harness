package main

// 0.180 hardening fix round: the stages of `gpu reserve` that the first pass of the signal work left unpinned or unworded.
//
//   - the checkpoint between the end of the drain and the command's start (a signal that lands there must not start the
//     command: it would be spawned and killed at once, and the card would read "exit status 1" instead of "never started");
//   - the re-queue after a lease lost during the drain (the cancel came back as "the wait for the card was cancelled by its
//     caller", naming neither the signal nor the likely causes);
//   - the unload of the seat (a signal that cancels the request can arrive after llama-swap has already unloaded, and a
//     seat unloaded with no debt on record stays cold: the warm-owed marker is written when the outcome is unknown too);
//   - the card text: every stage's wording fits the 140 characters the card keeps, whole.

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// driveWrapperAt runs `gpu reserve args` in a child (this test binary, TestHelperWrapperDriver) that delivers sig at the
// stage `at` names ("before-start"), and returns everything the child printed. The wrapper ends its own process with the
// command's exit code, so a wrapper that started its command cannot be observed from inside the test binary.
func driveWrapperAt(t *testing.T, args []string, at, sig string) string {
	t.Helper()
	cmd := winexec.Command(os.Args[0], "-test.run=^TestHelperWrapperDriver$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(),
		"LO_WRAP_ARGS="+strings.Join(args, "\n"),
		"LO_WRAP_SIGNAL="+sig,
		"LO_WRAP_AT="+at,
		"LO_HELPER_SLEEP_MS=60000",
	)
	done := make(chan struct{})
	var out []byte
	go func() {
		out, _ = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(80 * time.Second):
		t.Fatal("the wrapper never ended after the signal")
	}
	return string(out)
}

// BEFORE THE COMMAND STARTS: a signal that lands after the drain and before the command is spawned ends the reserve there.
// The command must not start (it would be killed at once, and the card would say it exited), the card says it never
// started, the seat the drain unloaded keeps its debt on SIGTERM (a console close gives seconds, a reload takes minutes) and
// is warmed back on Ctrl-C (which has no such deadline), and the lease is released.
func TestASignalBetweenTheDrainAndTheCommandStartsNothing(t *testing.T) {
	cases := map[string]struct {
		sig, want     string
		warms         int64
		warmStaysOwed bool
	}{
		"a console close (SIGTERM)": {sig: "term", want: "SIGTERM", warms: 0, warmStaysOwed: true},
		"Ctrl-C":                    {sig: "int", want: "interrupted", warms: 1, warmStaysOwed: false},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			r := newStageRig(t)
			args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "prestart"}, markerCmd()...)
			out := driveWrapperAt(t, args, "before-start", tc.sig)

			if r.commandStarted() {
				t.Errorf("the command must not start after the signal\n%s", out)
			}
			method, errText, methods := r.cardEnd(t)
			if method != "workload:errored" {
				t.Fatalf("the card must be closed (failed) with the cause, frames %v\n%s", methods, out)
			}
			for _, w := range []string{tc.want, "before its command started", "never started"} {
				if !strings.Contains(errText, w) {
					t.Errorf("the card's error %q lacks %q", errText, w)
				}
			}
			if strings.Contains(errText, "exit status") {
				t.Errorf("the card says the command exited, but it never started: %q", errText)
			}
			if n := r.swap.unloads.Load(); n != 1 {
				t.Errorf("the drain had finished and unloaded the seat before the signal: unloads=%d\n%s", n, out)
			}
			if n := r.swap.warms.Load(); n != tc.warms {
				t.Errorf("warm-backs = %d, want %d\n%s", n, tc.warms, out)
			}
			if owed := r.m.SeatWarmOwed(); (owed != "") != tc.warmStaysOwed {
				t.Errorf("warm owed = %q, want owed=%v\n%s", owed, tc.warmStaysOwed, out)
			}
			if tc.warmStaysOwed && !strings.Contains(out, "NOT warming the seat back") {
				t.Errorf("the wrapper must say it left the warm owed:\n%s", out)
			}
			// No command ran: what the signal ended is the wrapper, and the sentence must not say the command was ended.
			if tc.warmStaysOwed && (!strings.Contains(out, "the wrapper was ended on SIGTERM before its command started") || strings.Contains(out, "the command was ended")) {
				t.Errorf("the sentence must say the wrapper was ended before its command started, not that the command was ended:\n%s", out)
			}
			if info := r.m.Inspect(); info.Held {
				t.Errorf("the lease must be released: %+v", info)
			}
		})
	}
}

// loseTheLeaseToAnIntruder takes away the lease the reserve holds once ready reads true of it: the epoch is released out from
// under it and another request takes the card. It returns once the reserve has noticed (at its next heartbeat) and queued again
// behind that holder; the lease returned is the intruder's, and the caller releases it.
func loseTheLeaseToAnIntruder(t *testing.T, r *stageRig, ready func(gpulease.Info) bool) *gpulease.Lease {
	t.Helper()
	var first uint64
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if info := r.m.Inspect(); info.Held && ready(info) {
			first = info.Epoch
			break
		}
	}
	if first == 0 {
		t.Fatal("the reserve never took the lease in the state the test waits for")
	}
	if _, rerr := r.m.ReleaseByEpoch(first); rerr != nil {
		t.Fatal(rerr)
	}
	intruder, ierr := r.m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "concurrent acquire", TTL: time.Hour})
	if ierr != nil {
		t.Fatal(ierr)
	}
	queued := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if len(r.m.Waiters()) == 1 {
			queued = true
			break
		}
	}
	if !queued {
		_ = intruder.Release()
		t.Fatal("the reserve never queued again after its lease was taken away")
	}
	return intruder
}

// THE RE-QUEUE: a lease lost during the drain is taken again in the same line, and a signal there ends that wait the way it
// ends the first one: the signal is named with the cause as likely, the waiter leaves the line, the holder that took the card
// is left alone, and the command never runs. It used to come back as "the lease was lost ... could not be taken again: the
// wait for the card was cancelled by its caller".
func TestASignalWhileQueuedAgainAfterALostLeaseNamesTheSignal(t *testing.T) {
	r := newStageRig(t)
	r.swap.inflight.Store(1) // a request that never ends: the drain waits, and its lease is the one the test takes away
	old := drainRenewEvery
	drainRenewEvery = 300 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })

	oldFeed := wrapperSignalFeed
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	t.Cleanup(func() { wrapperSignalFeed = oldFeed })

	var err error
	var stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "requeue"}, markerCmd()...)
		stderr = captureStderr(t, func() { err = runGPUReserve(args) })
	}()

	// The reserve notices the loss at its next heartbeat and queues again behind the intruder.
	intruder := loseTheLeaseToAnIntruder(t, r, func(info gpulease.Info) bool { return info.Draining })
	defer func() { _ = intruder.Release() }()
	<-ready
	feed <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the re-queue wait did not end on the signal")
	}

	if err == nil {
		t.Fatalf("a wait ended by a signal is not a success\n%s", stderr)
	}
	for _, w := range []string{"SIGTERM", "likely", "while queued", "never started"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error %q lacks %q", err, w)
		}
	}
	if strings.Contains(err.Error(), "cancelled by its caller") {
		t.Errorf("the signal is the cause, not the cancel's own wording: %v", err)
	}
	if n := len(r.m.Waiters()); n != 0 {
		t.Errorf("the waiter must leave the line, %d record(s) remain", n)
	}
	if cerr := intruder.Check(); cerr != nil {
		t.Errorf("the signal disturbed the lease that took the card: %v", cerr)
	}
	if r.commandStarted() {
		t.Error("the command must not start after the signal")
	}
	// Ended on SIGTERM inside the grace: the old lease is gone and the seat was never unloaded, so the wrapper must not
	// try a warm-back under a lease that is not its own.
	if strings.Contains(stderr, "NOT warming") || strings.Contains(stderr, "warming ") {
		t.Errorf("a wrapper ended on SIGTERM with nothing unloaded says nothing about a warm-back:\n%s", stderr)
	}
	method, errText, methods := r.cardEnd(t)
	if method != "workload:errored" {
		t.Fatalf("the card must be closed with the cause, frames %v", methods)
	}
	if !strings.Contains(errText, "SIGTERM") || !strings.Contains(errText, "likely") {
		t.Errorf("the card names the signal and the cause as likely: %q", errText)
	}
}

// THE RE-QUEUE AFTER A CANCELLED UNLOAD: the only way a wrapper that queues again can owe a warm. The lease is taken away while
// the unload request is in flight; the reserve cancels the request, which may or may not have taken the seat down, and records
// the debt (the unload test above); it queues again behind the holder that took the card, and a SIGTERM there ends the wrapper
// with that debt on record. What it says then: the wrapper was ended while queued, before its command started (no command ever
// ran), and the warm stays owed.
func TestASignalWhileQueuedAgainAfterACancelledUnloadSaysTheWrapperWasEndedAndLeavesTheWarmOwed(t *testing.T) {
	r := newStageRig(t) // the seat is resident, nothing in flight: the drain ends at once and the unload is asked for
	old := drainRenewEvery
	drainRenewEvery = 300 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })

	oldFeed := wrapperSignalFeed
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	t.Cleanup(func() { wrapperSignalFeed = oldFeed })
	unloading := make(chan struct{})
	var once sync.Once
	r.swap.onUnload = func() {
		once.Do(func() { close(unloading) })
		time.Sleep(2500 * time.Millisecond) // the request is cancelled when the lease is taken away; the server finishes it after
	}

	var err error
	var stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "cancelled-unload"}, markerCmd()...)
		stderr = captureStderr(t, func() { err = runGPUReserve(args) })
	}()
	select {
	case <-unloading:
	case <-time.After(10 * time.Second):
		t.Fatal("the unload request never arrived")
	}
	intruder := loseTheLeaseToAnIntruder(t, r, func(gpulease.Info) bool { return true })
	defer func() { _ = intruder.Release() }()
	<-ready
	feed <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the re-queue wait did not end on the signal")
	}

	if err == nil {
		t.Fatalf("a wait ended by a signal is not a success\n%s", stderr)
	}
	for _, w := range []string{"SIGTERM", "likely", "while queued", "never started"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error %q lacks %q", err, w)
		}
	}
	if owed := r.m.SeatWarmOwed(); owed == "" {
		t.Errorf("the cancelled unload left the debt, and the lease that owed it is gone: the marker is all that records it\n%s", stderr)
	}
	for _, want := range []string{"the wrapper was ended on SIGTERM while queued, before its command started", "NOT warming the seat back", "the warm stays owed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the wrapper's output lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "the command was ended") {
		t.Errorf("no command ever ran, so none was ended:\n%s", stderr)
	}
	if n := r.swap.warms.Load(); n != 0 {
		t.Errorf("a wrapper ended on SIGTERM does not warm the seat back inside the grace: warms=%d", n)
	}
	if r.commandStarted() {
		t.Error("the command must not start after the signal")
	}
	if n := len(r.m.Waiters()); n != 0 {
		t.Errorf("the waiter must leave the line, %d record(s) remain", n)
	}
	if cerr := intruder.Check(); cerr != nil {
		t.Errorf("the signal disturbed the lease that took the card: %v", cerr)
	}
}

// THE UNLOAD: a signal that cancels the unload request can arrive after llama-swap has taken the seat down (the server
// finishes what it was asked, the client only stops waiting). The wrapper cannot tell, so it records the debt: "could not
// tell must not cost a warm seat". Without the marker the seat stays cold and nothing owes its reload.
func TestASignalDuringTheUnloadRequestLeavesTheWarmOwed(t *testing.T) {
	r := newStageRig(t) // the seat is resident, nothing in flight: the drain finishes at once

	oldFeed := wrapperSignalFeed
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	t.Cleanup(func() { wrapperSignalFeed = oldFeed })
	r.swap.onUnload = func() {
		<-ready
		feed <- syscall.SIGTERM
		time.Sleep(1500 * time.Millisecond) // the client's request is cancelled meanwhile; the seat is unloaded after it
	}

	args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "unload"}, markerCmd()...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "SIGTERM") {
		t.Fatalf("a signal during the unload ends the reserve naming it: %v\n%s", err, stderr)
	}
	if r.commandStarted() {
		t.Error("the command must not start after the signal")
	}
	if owed := r.m.SeatWarmOwed(); owed == "" {
		t.Errorf("the unload was asked for and its outcome is unknown, so the warm stays owed (the marker is the debt)\n%s", stderr)
	}
	if n := r.swap.warms.Load(); n != 0 {
		t.Errorf("a wrapper ended on SIGTERM does not warm the seat back inside the grace: warms=%d", n)
	}
	if !strings.Contains(stderr, "the warm stays owed") {
		t.Errorf("the wrapper must say the warm stays owed:\n%s", stderr)
	}
	// No command ran either: the signal ended the wrapper while it drained the seat.
	if !strings.Contains(stderr, "the wrapper was ended on SIGTERM while draining the seat, before its command started") || strings.Contains(stderr, "the command was ended") {
		t.Errorf("the sentence must say the wrapper was ended while draining the seat, not that the command was ended:\n%s", stderr)
	}
	if info := r.m.Inspect(); info.Held {
		t.Errorf("the lease must be released: %+v", info)
	}
}

// THE UNLOAD OF A COLD SEAT: the debt a cancelled unload leaves is written only for a seat that was resident. A seat that was
// cold owes no warm, and a marker for it would load it at the release with nothing asking (a lease over a cold seat once brought
// a three-card model up on all its cards that way).
func TestASignalDuringTheUnloadRequestOfAColdSeatOwesNoWarm(t *testing.T) {
	r := newStageRig(t)
	r.swap.loaded.Store(false) // the seat is not resident: nothing to unload, nothing to warm back

	oldFeed := wrapperSignalFeed
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	t.Cleanup(func() { wrapperSignalFeed = oldFeed })
	r.swap.onUnload = func() {
		<-ready
		feed <- syscall.SIGTERM
		time.Sleep(1500 * time.Millisecond) // the client's request is cancelled meanwhile
	}

	args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "cold"}, markerCmd()...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "SIGTERM") {
		t.Fatalf("a signal during the unload ends the reserve naming it: %v\n%s", err, stderr)
	}
	if owed := r.m.SeatWarmOwed(); owed != "" {
		t.Errorf("a seat that was not resident owes no warm, but the marker reads %q\n%s", owed, stderr)
	}
	if strings.Contains(stderr, "NOT warming") || strings.Contains(stderr, "the warm stays owed") {
		t.Errorf("nothing was owed, so nothing is said about a warm:\n%s", stderr)
	}
	if n := r.swap.warms.Load(); n != 0 {
		t.Errorf("warms = %d, want 0", n)
	}
	if r.commandStarted() {
		t.Error("the command must not start after the signal")
	}
	if info := r.m.Inspect(); info.Held {
		t.Errorf("the lease must be released: %+v", info)
	}
}

// THE DEBT CALLBACK: the debt is recorded through a callback, and the callers that have none (the drain's own tests call the
// function with none) must not be called through nil when a request is cancelled: the cancel comes back as an error, as it does
// for any other, and nothing is recorded.
func TestACancelledUnloadWithNoDebtCallbackReturnsTheErrorAndRecordsNothing(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onUnload = func() {
		cancel() // the caller stops waiting while the server holds the request
		time.Sleep(500 * time.Millisecond)
	}
	cfgPath, m := handoffFixture(t, f)
	var err error
	stderr := captureStderr(t, func() {
		err = maintainSeat(ctx, loadCfgPath(cfgPath), maintainOpts{unload: true})
	})
	if err == nil {
		t.Fatalf("a cancelled unload is an error\n%s", stderr)
	}
	if owed := m.SeatWarmOwed(); owed != "" {
		t.Errorf("no callback, so nothing is recorded, but the marker reads %q\n%s", owed, stderr)
	}
}

// THE REFUSED UNLOAD: the debt is for a request the CALLER cancelled, whose outcome nobody knows. A request the server answered
// with a refusal left the seat as it was, so no debt is written for it and no warm-back runs for a seat that never went down.
func TestARefusedUnloadOfAResidentSeatWritesNoDebt(t *testing.T) {
	r := newStageRig(t) // the seat is resident, nothing in flight
	r.swap.unloadStatus = 500

	args := append([]string{"--config", r.cfg, "--wait", "30s", "--drain", "--unload-seat", "--reason", "refused"}, markerCmd()...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "unload") {
		t.Fatalf("a refused unload ends the reserve as a seat fault: %v\n%s", err, stderr)
	}
	if owed := r.m.SeatWarmOwed(); owed != "" {
		t.Errorf("a refused unload owes no warm, but the marker reads %q\n%s", owed, stderr)
	}
	if n := r.swap.warms.Load(); n != 0 {
		t.Errorf("a warm-back ran for a seat that never went down: warms=%d\n%s", n, stderr)
	}
	if r.commandStarted() {
		t.Error("the command must not start after a failed unload")
	}
	if info := r.m.Inspect(); info.Held {
		t.Errorf("the lease must be released: %+v", info)
	}
}

// THE CARD TEXT: the card keeps 140 characters (pairworkloads.CardErrorMax) and cuts the rest, so a stage text longer than
// that loses exactly the end that says what happened ("... never start..."). Every stage text, for every signal the wrapper
// asks for, fits whole.
func TestEveryStageTextFitsTheCardWhole(t *testing.T) {
	stages := map[string]string{"before the card": stageBeforeCard, "queued": stageQueued, "draining": stageDraining, "host RAM": stageHostRAM, "before the start": stageBeforeStart}
	for sname, stage := range stages {
		for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
			text := endedBySignal(sig, stage).Error()
			if n := len([]rune(text)); n > pairworkloads.CardErrorMax {
				t.Errorf("%s / %v: %d characters, the card keeps %d: %q", sname, sig, n, pairworkloads.CardErrorMax, text)
			}
			if got := pairworkloads.CardError(text); got != text {
				t.Errorf("%s / %v: the card would show %q, not the whole text %q", sname, sig, got, text)
			}
		}
	}
}
