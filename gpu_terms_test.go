package main

// Bounded terms at the two holders that tick (plan P9, ADR 0070): the wrapper form and the
// detached holder. A term that ends is renewed or labelled; it is never a release. Every test
// holds a lease on a scratch state root (leaseFixture); none touches a real lease directory,
// a GPU or nvidia-smi. The tests that wait for a real deadline are TIMING-SENSITIVE (a 1 s
// window and a 40 ms poll); they are bounded by generous ceilings, not by sleeps.

import (
	"flag"
	"os"
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

// noCards swaps the utilisation reading the term check uses for a fixed answer, and counts
// how often it was asked.
func termFixedUtil(t *testing.T, busy bool) *int {
	t.Helper()
	asked := new(int)
	old := termUtilWorkingFn
	termUtilWorkingFn = func(devices []string) bool { *asked++; return busy }
	t.Cleanup(func() { termUtilWorkingFn = old })
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
