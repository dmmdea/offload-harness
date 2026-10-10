package main

// What `gpu reserve` SAYS while it stands in line, and how it words a refusal (register D-1xx-3,
// 2026-10-09). The entry line used to come from a failed bare probe; it now comes from a read of
// the line (queueLine), and a change to that read can silently turn both lines off while every
// behaviour test stays green: the hold and the grant do not depend on them. These tests replay a
// device-scoped reserve through the verb and read what it printed.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func reserveCard2(cfg string, wait string, reason string) []string {
	return append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", wait, "--reason", reason}, helperCmd()...)
}

// Queued behind a HOLDER: one line on entry naming the holder and the wait, one on acquire.
func TestAReserveBehindAHolderPrintsItsEntryAndExitLines(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	holder := holdCard(t, m, boxCard2)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	var err error
	stderr := captureStderr(t, func() {
		done := make(chan error, 1)
		go func() { done <- runGPUReserve(reserveCard2(cfg, "20s", "behind the holder")) }()
		waitForWaiter(t, m)
		_ = holder.Release()
		err = <-done
	})
	if err != nil {
		t.Fatalf("a held card is a place in line: %v", err)
	}
	for _, want := range []string{"gpu reserve: queued behind GPU held by media", "waiting up to 20s", "gpu reserve: acquired after"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the reserve must print %q while it queues behind a holder; stderr was:\n%s", want, stderr)
		}
	}
}

// Queued behind a WAITER on a card that is FREE (the incident's shape): the entry line names the
// waiter that is ahead, not a holder that does not exist, and the exit line follows when it leaves.
func TestAReserveBehindAWaiterOnAFreeCardPrintsItsEntryAndExitLines(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	leave := seatWaiterOn(t, cfg, "transcribe voice_es.wav", boxCard2)
	time.Sleep(5 * time.Millisecond)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	var err error
	stderr := captureStderr(t, func() {
		done := make(chan error, 1)
		go func() { done <- runGPUReserve(reserveCard2(cfg, "20s", "behind the waiter")) }()
		waitForWaiters(t, m, 2) // the seat waiter and this reserve's own place
		leave()
		err = <-done
	})
	if err != nil {
		t.Fatalf("a waiter ahead on a free card is a place in line: %v", err)
	}
	for _, want := range []string{
		`gpu reserve: queued behind pid `,
		`(seat, reason "transcribe voice_es.wav"), already in line for the card`,
		"waiting up to 20s",
		"gpu reserve: acquired after",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the reserve must print %q while it queues behind a waiter; stderr was:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "GPU held by") {
		t.Errorf("the card is free: no holder may be named; stderr was:\n%s", stderr)
	}
}

// Nobody ahead: the reserve says nothing about a queue it never stood in.
func TestAReserveWithNobodyAheadPrintsNoQueueLines(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(reserveCard2(cfg, "20s", "uncontended")) })
	if err != nil {
		t.Fatalf("a free card is granted: %v", err)
	}
	for _, unwanted := range []string{"queued behind", "acquired after"} {
		if strings.Contains(stderr, unwanted) {
			t.Errorf("an uncontended reserve must not print %q; stderr was:\n%s", unwanted, stderr)
		}
	}
}

// --wait 0 with a waiter ahead on a free card: refused at once, saying who is ahead and which flag
// queues, and NOT saying it gave up waiting — it never waited. No entry line: it stands in no line.
func TestAFailFastReserveBehindAWaiterNamesTheFlagAndDoesNotClaimItWaited(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "transcribe voice_es.wav", boxCard2)
	time.Sleep(5 * time.Millisecond)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(reserveCard2(cfg, "0", "fail fast")) })
	if err == nil {
		t.Fatal("a --wait 0 reserve won a free card ahead of a registered waiter")
	}
	if !errors.Is(err, gpulease.ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued, got: %v", err)
	}
	for _, want := range []string{"transcribe voice_es.wav", "pass --wait <duration>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "gave up") || strings.Contains(err.Error(), "waiting") {
		t.Errorf("a refusal on the spot must not claim it waited: %v", err)
	}
	if strings.Contains(stderr, "queued behind") {
		t.Errorf("a --wait 0 reserve stands in no line and prints no entry line; stderr was:\n%s", stderr)
	}
}

// A full window spent behind a waiter that never claimed says how long it waited and names the flag
// that waits longer, as the held-card refusal always did.
func TestAReserveThatOutwaitedItsWindowBehindAWaiterNamesTheLongerWait(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "transcribe voice_es.wav", boxCard2)
	time.Sleep(5 * time.Millisecond)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	var err error
	_ = captureStderr(t, func() { err = runGPUReserve(reserveCard2(cfg, "300ms", "outwaited")) })
	if err == nil {
		t.Fatal("a reserve won a free card ahead of a registered waiter")
	}
	for _, want := range []string{"gave up after waiting 300ms", "transcribe voice_es.wav", "pass a longer --wait"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q: %v", want, err)
		}
	}
}

// heldHint is the one place a refusal gets the flag that would have changed it.
func TestHeldHintNamesTheFlagThatChangesTheOutcome(t *testing.T) {
	still := fmt.Errorf("%w: the card is free, but this request is not first in line", gpulease.ErrStillQueued)
	if got := heldHint(still, 0).Error(); !strings.Contains(got, "pass --wait <duration>") || !strings.Contains(got, "instead of failing") {
		t.Errorf("--wait 0: the hint must name the flag that queues: %s", got)
	}
	if got := heldHint(still, 5*time.Second).Error(); !strings.Contains(got, "pass a longer --wait") {
		t.Errorf("a window that ran out behind a waiter: the hint must name the longer wait: %s", got)
	}
	plain := errors.New("a configuration fault")
	if got := heldHint(plain, 0); got != plain {
		t.Errorf("an error that is not a place in line passes through untouched: %v", got)
	}
	if heldHint(nil, 0) != nil {
		t.Error("no error, no hint")
	}
}

// The detached holder's own last words reach the parent. A hidden child that gives up wrote its
// reason to a temp log, and the parent printed only the log's name: the one sentence that said
// what to do (`--wait` on a free card with a waiter ahead) sat in a file nobody was pointed at.
func TestTheParentRepeatsTheDetachedHoldersLastWords(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "hold.log")
	body := "WARNING: config file NOT FOUND\n" +
		"gpu reserve: holding cards gpu-cccc0000-x (--devices 2)\n" +
		"error: gpulease: still queued: the card is free, but this request is not first in line, behind pid 7 (seat, reason \"load\"), which has not claimed the card; pass --wait <duration> (default 8h0m0s) to queue behind it instead of failing\n"
	if err := os.WriteFile(log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	err := detachGaveUpError(4321, errors.New("exit status 1"), log)
	for _, want := range []string{"detached holder (pid 4321) gave up before taking the lease", "exit status 1", "still queued", "pass --wait <duration>", log} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the parent's message must contain %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "error: ") {
		t.Errorf("the holder's `error:` prefix is repeated once, as words, not as a second error label: %v", err)
	}
}

func TestChildReasonIsEmptyWhenThereIsNothingToRepeat(t *testing.T) {
	dir := t.TempDir()
	if got := childReason(filepath.Join(dir, "no-such.log")); got != "" {
		t.Errorf("an unreadable log repeats nothing, got %q", got)
	}
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := childReason(empty); got != "" {
		t.Errorf("a blank log repeats nothing, got %q", got)
	}
	long := filepath.Join(dir, "long.log")
	if err := os.WriteFile(long, []byte("error: "+strings.Repeat("x", 5*childReasonMax)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := childReason(long); len([]rune(got)) > childReasonMax+10 || !strings.HasSuffix(got, "...") {
		t.Errorf("a long last line is clipped (%d runes): %q...", len([]rune(got)), got[:40])
	}
}

// The child itself: `gpu hold --wait 0` on a free card with a waiter ahead exits with the hint, so
// there is something to repeat. The holder is the process that queues; the parent never saw the line.
func TestTheDetachedHolderRefusesWithTheWaitHintWhenAWaiterIsAhead(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	seatWaiterOn(t, cfg, "transcribe voice_es.wav", boxCard2)
	time.Sleep(5 * time.Millisecond)
	err := runGPUHold([]string{"--config", cfg, "--class", "media", "--for", "3s", "--wait", "0", "--devices", boxCard2, "--reason", "hold"})
	if err == nil {
		t.Fatal("a --wait 0 holder won a free card ahead of a registered waiter")
	}
	if !errors.Is(err, gpulease.ErrStillQueued) || !strings.Contains(err.Error(), "pass --wait <duration>") {
		t.Fatalf("the holder's last words must carry the --wait hint: %v", err)
	}
}
