package main

// Bounded terms at the two holders that tick (plan P9, ADR 0070): the wrapper form and the
// detached holder. A term that ends is renewed or labelled; it is never a release. Every test
// holds a lease on a scratch state root (leaseFixture); none touches a real lease directory,
// a GPU or nvidia-smi. The tests that wait for a real deadline are TIMING-SENSITIVE (a 1 s
// window and a 40 ms poll); they are bounded by generous ceilings, not by sleeps.

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// termFastHolder makes the detached holder poll and renew quickly enough for a test to see a
// deadline pass, and keeps the session environment out of the lease's owner.
func termFastHolder(t *testing.T) {
	t.Helper()
	oldPoll, oldRenew := holdPollEvery, holdRenewEvery
	holdPollEvery, holdRenewEvery = 40*time.Millisecond, 120*time.Millisecond
	t.Cleanup(func() { holdPollEvery, holdRenewEvery = oldPoll, oldRenew })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
}

// termFixedUtil swaps the look the term check takes at the cards for a fixed answer (busy or
// idle), and counts how often it was asked.
func termFixedUtil(t *testing.T, busy bool) *int {
	t.Helper()
	if busy {
		return termFixedCards(t, gpulease.CardsWorking)
	}
	return termFixedCards(t, gpulease.CardsIdle)
}

// termFixedCards is termFixedUtil for any of the three answers the cards can give.
func termFixedCards(t *testing.T, r gpulease.CardsReading) *int {
	t.Helper()
	asked := new(int)
	old := termCardsFn
	termCardsFn = func(devices []string) gpulease.CardsReading { *asked++; return r }
	t.Cleanup(func() { termCardsFn = old })
	return asked
}

func termHoldFor(t *testing.T, cfg, dur string, extra ...string) chan error {
	t.Helper()
	done := make(chan error, 1)
	args := append([]string{"--config", cfg, "--class", "media", "--for", dur, "--wait", "0", "--reason", "hold"}, extra...)
	go func() { done <- runGPUHold(args) }()
	return done
}

func termWaitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

func termExited(done chan error) (error, bool) {
	select {
	case err := <-done:
		return err, true
	default:
		return nil, false
	}
}

// THE REQUEST IS NEVER SHORTENED: a detached holder asked for 20 hours holds 20 hours, not the
// 6 hour cap, and its record says what was asked.
func TestDetachHolderNotShortenedByCap(t *testing.T) {
	termFastHolder(t)
	cfg, m := leaseFixture(t)
	done := termHoldFor(t, cfg, "20h")
	l := waitForLeases(t, m, 1)[0]
	if got := l.ExpiresAt.Sub(l.AcquiredAt); got < 20*time.Hour-time.Second || got > 20*time.Hour+time.Second {
		t.Fatalf("the holder was given a %s window, want the whole 20h it asked for", got)
	}
	if l.Requested != 20*time.Hour || l.Term != 6*time.Hour {
		t.Fatalf("the record must carry the request and the term it renews by: requested %s term %s", l.Requested, l.Term)
	}
	if _, err := m.ReleaseByEpoch(l.Epoch); err != nil {
		t.Fatal(err)
	}
	termWaitFor(t, "the holder to see its lease released and exit", 10*time.Second, func() bool { _, ok := termExited(done); return ok })
}

// THE 2026-09-07 INCIDENT: a detached holder released the card at its --for deadline with the
// job still running behind it. Past the deadline it now goes overdue and HOLDS.
func TestDetachHolderGoesOverdueNotReleased(t *testing.T) {
	termFastHolder(t)
	termFixedUtil(t, false)
	cfg, m := leaseFixture(t)
	done := termHoldFor(t, cfg, "1s")
	l := waitForLeases(t, m, 1)[0]
	hb0 := l.HeartbeatAt

	termWaitFor(t, "the term to end and the lease to be labelled expired", 15*time.Second, func() bool {
		in := m.Inspect()
		return in.Held && in.Expired
	})
	if err, gone := termExited(done); gone {
		t.Fatalf("the holder termExited at its deadline (%v): it must stay and hold the card", err)
	}
	// It keeps holding well past the deadline, and keeps heartbeating (the label is not a release).
	time.Sleep(700 * time.Millisecond)
	in := m.Inspect()
	if !in.Held || in.Epoch != l.Epoch || !in.Expired {
		t.Fatalf("an expired lease must still be the held one: %+v", in)
	}
	if !in.HeartbeatAt.After(hb0) {
		t.Fatalf("the heartbeat stopped at the deadline: %s, was %s", in.HeartbeatAt, hb0)
	}
	if st := m.StandingReadOnly(in, 0); !st.Overdue {
		t.Fatalf("an expired lease reads overdue: %+v", st)
	}
	if err, gone := termExited(done); gone {
		t.Fatalf("the holder termExited after the deadline (%v)", err)
	}
	// Only a release ends it, and the holder notices and exits quietly, as it always did.
	if _, err := m.ReleaseByEpoch(l.Epoch); err != nil {
		t.Fatal(err)
	}
	termWaitFor(t, "the holder to see its lease released and exit", 10*time.Second, func() bool { _, ok := termExited(done); return ok })
}

// --release-at-expiry restores the pre-terms behaviour for a caller that wants exactly that.
func TestDetachHolderReleaseAtExpiryRestoresTheOldBehaviour(t *testing.T) {
	termFastHolder(t)
	termFixedUtil(t, false)
	cfg, m := leaseFixture(t)
	done := termHoldFor(t, cfg, "1s", "--release-at-expiry")
	waitForLeases(t, m, 1)
	var err error
	termWaitFor(t, "the holder to release at its deadline and exit", 15*time.Second, func() bool {
		var ok bool
		err, ok = termExited(done)
		return ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if in := m.Inspect(); in.Held {
		t.Fatalf("--release-at-expiry must free the card at the deadline: %+v", in)
	}
}

// A detached holder is a lease like any other: while its owner is alive and the cards are
// working, its term is renewed, one term at a time, and the holder carries on.
func TestDetachHolderRenewsItsTermWhileTheOwnerIsAliveAndTheCardsWork(t *testing.T) {
	termFastHolder(t)
	asked := termFixedUtil(t, true)
	cfg, m := leaseFixture(t)
	pid := os.Getpid()
	start, _ := gpulease.ProcessStart(pid)
	done := termHoldFor(t, cfg, "1s", "--owner-pid", strconv.Itoa(pid), "--owner-start-ms", strconv.FormatInt(start, 10))
	l := waitForLeases(t, m, 1)[0]
	end0 := l.ExpiresAt
	termWaitFor(t, "the first renewal", 15*time.Second, func() bool {
		return m.Inspect().ExpiresAt.After(end0)
	})
	in := m.Inspect()
	if in.Expired || !in.Held {
		t.Fatalf("a renewed lease is not expired: %+v", in)
	}
	if got := in.ExpiresAt.Sub(end0); got != time.Second {
		t.Fatalf("renewed by %s, want one 1s term", got)
	}
	if *asked == 0 {
		t.Fatal("an attended lease with a live owner and no progress contract is judged by its cards")
	}
	if _, err := m.ReleaseByEpoch(l.Epoch); err != nil {
		t.Fatal(err)
	}
	termWaitFor(t, "the holder to exit", 10*time.Second, func() bool { _, ok := termExited(done); return ok })
}

// The wrapper form ticks the same check: a lease whose term ends with nobody vouching for it is
// labelled, and the COMMAND KEEPS RUNNING: the label is not a lease loss.
func TestWrapperLabelsAnExpiredLeaseAndNeverKillsTheCommand(t *testing.T) {
	termFixedUtil(t, false)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	old := wrapperTickEvery
	wrapperTickEvery = 150 * time.Millisecond
	t.Cleanup(func() { wrapperTickEvery = old })
	cfg, m := leaseFixture(t)
	t.Setenv("LO_HELPER_SLEEP_MS", "3500")
	type outcome struct {
		err    error
		stderr string
	}
	done := make(chan outcome, 1)
	go func() {
		var err error
		stderr := captureStderr(t, func() {
			err = runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--for", "1s", "--wait", "0", "--reason", "wrapped"}, helperCmd()...))
		})
		done <- outcome{err, stderr}
	}()
	waitForLeases(t, m, 1)
	termWaitFor(t, "the wrapper's tick to label the lease", 15*time.Second, func() bool { return m.Inspect().Expired })
	select {
	case o := <-done:
		t.Fatalf("the wrapped command was cut short when its term ended: %v\n%s", o.err, o.stderr)
	default:
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("the wrapped command never finished")
	}
	if o.err != nil {
		t.Fatalf("the command must complete normally under an expired lease: %v", o.err)
	}
	stderr := o.stderr
	for _, want := range []string{"term ended", "not renewed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the wrapper says once that the term ended unrenewed (%q missing):\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "LEASE LOST") {
		t.Errorf("an expired lease is not a lost one:\n%s", stderr)
	}
}

// The over-cap request is warned about where it is made, once.
func TestReserveWarnsOnAnOverCapRequest(t *testing.T) {
	w := reserveTermWarning(20*time.Hour, gpulease.Options{})
	for _, want := range []string{"20h0m0s", "6h0m0s", "never shortened"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %q", want, w)
		}
	}
	if w := reserveTermWarning(2*time.Hour, gpulease.Options{}); w != "" {
		t.Errorf("a request under the cap says nothing: %q", w)
	}
	if w := reserveTermWarning(20*time.Hour, gpulease.Options{ProgressFile: "/p/log.jsonl", Stall: time.Hour}); w != "" {
		t.Errorf("a progress contract may declare 20h at the start: %q", w)
	}
}

func TestReleaseAtExpiryIsForTheDetachedHolderOnly(t *testing.T) {
	cfg, _ := leaseFixture(t)
	err := runGPUReserve(append([]string{"--config", cfg, "--wait", "0", "--release-at-expiry"}, helperCmd()...))
	if err == nil || !strings.Contains(err.Error(), "--release-at-expiry") || !strings.Contains(err.Error(), "--detach") {
		t.Fatalf("the wrapper form holds exactly as long as its command; the flag is for --detach: %v", err)
	}
}

func TestDetachedHolderIsToldToReleaseAtExpiryOnlyWhenAsked(t *testing.T) {
	fs := flag.NewFlagSet("gpu reserve", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.Bool("release-at-expiry", false, "")
	opts := gpulease.Options{Reason: "x"}
	if args := detachHoldArgs(fs, "media", time.Hour, 0, opts, nil, ""); strings.Contains(strings.Join(args, " "), "--release-at-expiry") {
		t.Errorf("not asked, not passed: %v", args)
	}
	if err := fs.Set("release-at-expiry", "true"); err != nil {
		t.Fatal(err)
	}
	args := detachHoldArgs(fs, "media", time.Hour, 0, opts, nil, "")
	if !strings.Contains(strings.Join(args, " "), "--release-at-expiry") || args[0] != "gpu" || args[1] != "hold" {
		t.Errorf("asked, passed to `gpu hold`: %v", args)
	}
}

// releaseAll releases every live lease the way an operator's `gpu release --epoch N` does: a
// hidden holder notices its lease gone and exits. Holders no longer release at their deadline
// (plan P9), so a test that wants one to end releases it.
func releaseAll(t *testing.T, m *gpulease.Manager) {
	t.Helper()
	for _, l := range m.Leases() {
		if _, err := m.ReleaseByEpoch(l.Epoch); err != nil {
			t.Fatalf("release epoch %d: %v", l.Epoch, err)
		}
	}
}

// `gpu status` says what a lease's term is, and for an expired one why it was not renewed.
func TestStatusSaysTheTermAndWhyAnExpiredLeaseExpired(t *testing.T) {
	hardEnd := time.Now().Add(30 * time.Hour).UTC().Format(time.RFC3339)
	h := &gpuactivity.Holder{Epoch: 41, OwnerState: "gone", Overdue: true, OverdueBySec: 3 * 3600,
		Expired: true, ExpiredWhy: "its owner is gone", TermSec: 6 * 3600, RequestedSec: 20 * 3600, HardEnd: hardEnd}
	got := strings.Join(ownershipStatusLines(h, gpulease.Info{Epochs: []uint64{41}}), "\n")
	for _, want := range []string{"EXPIRED 3h0m0s ago", "its owner is gone", "nothing is reclaimed or killed", "6h0m0s terms", "asked for 20h0m0s", "accepted whole"} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	// A lease that is overdue but was never labelled keeps today's line and claims no expiry.
	h2 := &gpuactivity.Holder{Epoch: 41, OwnerState: "unknown", Overdue: true, OverdueBySec: 600}
	got2 := strings.Join(ownershipStatusLines(h2, gpulease.Info{Epochs: []uint64{41}}), "\n")
	if !strings.Contains(got2, "window: past its declared end by 10m0s") || strings.Contains(got2, "EXPIRED") || strings.Contains(got2, "term:") {
		t.Errorf("an unlabelled, term-less lease prints no term line:\n%s", got2)
	}
	// Inside its term a lease says what renews it, and nothing about expiry.
	h3 := &gpuactivity.Holder{Epoch: 41, OwnerState: "alive", TermSec: 3600, HardEnd: hardEnd}
	got3 := strings.Join(ownershipStatusLines(h3, gpulease.Info{Epochs: []uint64{41}}), "\n")
	if !strings.Contains(got3, "term: renews in 1h0m0s terms") || strings.Contains(got3, "EXPIRED") || strings.Contains(got3, "asked for") {
		t.Errorf("a lease inside its term:\n%s", got3)
	}
}

// An expired lease is not sampled every heartbeat, and the holder says it ONCE: a wrapper that
// printed (or ran nvidia-smi) on every 15 s tick of a lease that stays expired for a day would be
// a notification loop in the session that wrapped it.
func TestExpiredLeaseIsAskedAgainOnlyEveryRecheckInterval(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	asked := termFixedUtil(t, false)
	old := termRecheckEvery
	termRecheckEvery = time.Hour
	t.Cleanup(func() { termRecheckEvery = old })
	_, m := leaseFixture(t)
	pid := os.Getpid()
	start, _ := gpulease.ProcessStart(pid)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", TTL: 30 * time.Millisecond, Owner: gpulease.Owner{PID: pid, StartMs: start}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	time.Sleep(80 * time.Millisecond) // the term has ended

	terms := newTermTicker(l)
	out := captureStderr(t, func() {
		for i := 0; i < 5; i++ {
			terms.tick()
		}
	})
	if *asked != 1 {
		t.Fatalf("five ticks of an expired lease asked about its cards %d times, want once (the label is re-asked only every termRecheckEvery)", *asked)
	}
	if n := strings.Count(out, "not renewed"); n != 1 {
		t.Fatalf("the holder must say once that the term ended unrenewed, said it %d times:\n%s", n, out)
	}
	if !m.Inspect().Expired {
		t.Fatal("the lease should be labelled")
	}

	// Once the interval has passed, it asks again (an owner that came back, progress that resumed).
	termRecheckEvery = 10 * time.Millisecond
	time.Sleep(30 * time.Millisecond)
	terms.tick()
	if *asked != 2 {
		t.Fatalf("after the recheck interval the lease must be asked again: asked %d", *asked)
	}
}

// A lease that stays expired is told about ONCE, however long its progress file stays silent. This
// is the shape that repeated: an unattended lease (the pepi stopgap's), a progress file nobody
// touches, a recheck every minute, and a reason that quoted how long the file had been silent, so
// every recheck was "a different reason" and the wrapper wrote another "not renewed" line into
// the stream the wrapping session reads as notifications. TIMING-SENSITIVE: it sleeps just over a
// second, because the silence was quoted to the second and a shorter gap rounds to the same text.
func TestStalledUnattendedLeaseIsToldOnceNotOncePerRecheck(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	asked := termFixedUtil(t, true) // a busy card must not matter to an unattended lease
	old := termRecheckEvery
	termRecheckEvery = 10 * time.Millisecond
	t.Cleanup(func() { termRecheckEvery = old })
	_, m := leaseFixture(t)
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte(`{"done":1,"total":9}`+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", TTL: 30 * time.Millisecond, Unattended: true, ProgressFile: prog, Stall: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	time.Sleep(80 * time.Millisecond) // the term has ended and the file is already past its stall window

	terms := newTermTicker(l)
	var whys []string
	out := captureStderr(t, func() {
		terms.tick()
		whys = append(whys, m.Inspect().ExpiredWhy)
		time.Sleep(1100 * time.Millisecond) // the silence, to the second, is now a different number
		terms.tick()
		whys = append(whys, m.Inspect().ExpiredWhy)
		time.Sleep(30 * time.Millisecond)
		terms.tick()
		whys = append(whys, m.Inspect().ExpiredWhy)
	})
	if n := strings.Count(out, "not renewed"); n != 1 {
		t.Fatalf("the holder said the term ended unrenewed %d times for one lease whose reason never changed, want once:\n%s", n, out)
	}
	// And the record was not rewritten with a new sentence either: what the label says is a fact
	// about the lease, not a reading of the clock.
	if whys[0] == "" || whys[1] != whys[0] || whys[2] != whys[0] {
		t.Fatalf("the label changed with time although nothing about the lease did:\n%q", whys)
	}
	if *asked != 0 {
		t.Fatalf("an unattended lease is judged by its progress file alone; the cards were asked %d times", *asked)
	}
	if !m.Inspect().Expired {
		t.Fatal("the lease should be labelled")
	}
}

// termExpiredFixture is a lease whose owner (this process) is alive, with no progress contract
// and a 30 ms term that has already ended: the case the cards decide.
func termExpiredFixture(t *testing.T) (*gpulease.Manager, *gpulease.Lease) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	_, m := leaseFixture(t)
	pid := os.Getpid()
	start, _ := gpulease.ProcessStart(pid)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", TTL: 30 * time.Millisecond, Owner: gpulease.Owner{PID: pid, StartMs: start}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	time.Sleep(80 * time.Millisecond) // the term has ended
	return m, l
}

// The holder speaks when a lease BECOMES expired, not whenever the label is rewritten. The label
// follows the evidence (a card that could not be read at one look and was quiet at the next is a
// different sentence), and each change is written to the record where every status surface reads
// it, but a wrapper whose stderr is the wrapping session's notification stream says it once.
func TestHolderSaysOnceThatALeaseExpiredEvenWhenItsReasonChanges(t *testing.T) {
	old := termRecheckEvery
	termRecheckEvery = 10 * time.Millisecond
	t.Cleanup(func() { termRecheckEvery = old })
	reading := gpulease.CardsIdle
	oldCards := termCardsFn
	termCardsFn = func([]string) gpulease.CardsReading { return reading }
	t.Cleanup(func() { termCardsFn = oldCards })
	m, l := termExpiredFixture(t)

	terms := newTermTicker(l)
	var whys []string
	out := captureStderr(t, func() {
		for _, r := range []gpulease.CardsReading{gpulease.CardsIdle, gpulease.CardsUnreadable, gpulease.CardsUnreadable, gpulease.CardsIdle} {
			reading = r
			time.Sleep(25 * time.Millisecond) // past the recheck interval
			terms.tick()
			whys = append(whys, m.Inspect().ExpiredWhy)
		}
	})
	if n := strings.Count(out, "not renewed"); n != 1 {
		t.Fatalf("a lease that stayed expired was announced %d times, want once:\n%s", n, out)
	}
	// The record still follows the evidence, tick by tick.
	if !strings.Contains(whys[0], "show work") || !strings.Contains(whys[1], "could not be read") || whys[2] != whys[1] || !strings.Contains(whys[3], "show work") {
		t.Fatalf("the label should follow what the cards showed (idle, unreadable, unreadable, idle):\n%q", whys)
	}
}

// And it speaks again when a lease becomes expired AGAIN: a renewal ends the episode.
func TestHolderSaysItAgainWhenARenewedLeaseExpiresAgain(t *testing.T) {
	old := termRecheckEvery
	termRecheckEvery = 10 * time.Millisecond
	t.Cleanup(func() { termRecheckEvery = old })
	reading := gpulease.CardsIdle
	oldCards := termCardsFn
	termCardsFn = func([]string) gpulease.CardsReading { return reading }
	t.Cleanup(func() { termCardsFn = oldCards })
	m, l := termExpiredFixture(t)

	terms := newTermTicker(l)
	out := captureStderr(t, func() {
		terms.tick() // expired
		reading = gpulease.CardsWorking
		time.Sleep(25 * time.Millisecond)
		terms.tick() // the cards are at work: renewed
		if m.Inspect().Expired {
			t.Error("a renewed lease carries no label")
		}
		reading = gpulease.CardsIdle
		time.Sleep(60 * time.Millisecond) // the renewed term (30 ms) ends
		terms.tick()                      // expired again
	})
	if n := strings.Count(out, "not renewed"); n != 2 {
		t.Fatalf("two separate expiries were announced %d times, want twice:\n%s", n, out)
	}
	if n := strings.Count(out, "was renewed to"); n != 1 {
		t.Fatalf("the renewal between them was announced %d times, want once:\n%s", n, out)
	}
}

// A failed nvidia-smi sample (a timeout under a saturated GPU, a box without the tool) reaches
// the label as what it is. Through the real look, with only the driver replaced.
func TestFailedCardSampleIsLabelledUnreadableNotIdle(t *testing.T) {
	oldSample := termSampleGPUsFn
	t.Cleanup(func() { termSampleGPUsFn = oldSample })
	termSampleGPUsFn = func(context.Context) ([]gpuactivity.GPU, error) { return nil, errors.New("nvidia-smi: signal: killed") }
	m, l := termExpiredFixture(t)

	out := captureStderr(t, func() { newTermTicker(l).tick() })
	why := m.Inspect().ExpiredWhy
	for _, s := range []string{out, why} {
		if !strings.Contains(s, "cards could not be read") || strings.Contains(s, "show work") {
			t.Fatalf("a failed sample must be worded as unreadable, never as idle cards:\nstderr %q\nlabel  %q", out, why)
		}
	}

	// A sample that works and shows quiet cards is the idle sentence, through the same path.
	termSampleGPUsFn = func(context.Context) ([]gpuactivity.GPU, error) {
		return []gpuactivity.GPU{{Index: 0, UUID: "GPU-AAAA", UtilPct: 2, UtilKnown: true}}, nil
	}
	m2, l2 := termExpiredFixture(t)
	_ = captureStderr(t, func() { newTermTicker(l2).tick() })
	if why := m2.Inspect().ExpiredWhy; !strings.Contains(why, "show work") || strings.Contains(why, "could not be read") {
		t.Fatalf("quiet cards that were read: %q", why)
	}
	// And one that shows work renews.
	termSampleGPUsFn = func(context.Context) ([]gpuactivity.GPU, error) {
		return []gpuactivity.GPU{{Index: 0, UUID: "GPU-AAAA", UtilPct: 90, UtilKnown: true}}, nil
	}
	m3, l3 := termExpiredFixture(t)
	before := m3.Inspect().ExpiresAt
	_ = captureStderr(t, func() { newTermTicker(l3).tick() })
	if in := m3.Inspect(); in.Expired || !in.ExpiresAt.After(before) {
		t.Fatalf("cards at work renew the term (end %s before, %s after): %+v", before, in.ExpiresAt, in)
	}
}
