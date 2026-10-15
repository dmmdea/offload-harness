package main

// Round 5, item 2. A lease record that EXISTS and cannot be read (a scanner holds it open, a sharing violation: an I/O failure, not a
// corrupt file) is held for as long as its keeper is alive, and the keeper gives up first.
//
// Before: the acquirers swept such a record, with its claims, once it was older than claimGrace (ten seconds), whatever its holder was
// doing, while the wrapper in its retry window kept running its command and wrote no heartbeat. The next reserve got the cards with
// the first command still running on them.
//
// Now the two sides agree. An acquirer leaves an I/O-unreadable record alone while its holder's heartbeat is fresh (the TTL the sweep
// uses for every record). Every keeper (the wrapper, its drain heartbeat, its drain loop, the hidden holder) fails closed once its
// bounded read-retry window passes with the record still unreadable, and every window plus its tick is shorter than that TTL, so a
// keeper has always stopped before the sweep can hand its cards out.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// unreadableWhileFlagged is a read fault for the lease records (meta.json and e/<epoch>.json) that is on while the flag file exists:
// the record is still there and can be removed, but cannot be read. The wrapper process and the test both install it; scope, when not
// empty, is the lease directory it applies to (the in-process installs name theirs, so no other lease in the test binary is touched).
func unreadableWhileFlagged(flag, scope string) func(path string) error {
	return func(path string) error {
		if scope != "" && !strings.HasPrefix(path, scope) {
			return nil
		}
		if _, err := os.Stat(flag); err != nil {
			return nil
		}
		base := filepath.Base(path)
		if base == "meta.json" || (strings.HasSuffix(base, ".json") && filepath.Base(filepath.Dir(path)) == "e") {
			return &fs.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		}
		return nil
	}
}

func TestAnUnreadableRecordWithALiveWrapperIsNotSweptAndTheWrapperStopsBeforeAnyoneElseGetsTheCards(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "unreadable.flag")
	t.Setenv("LO_WRAPPER_FENCE_MS", "1500") // the wrapper's window for a record it cannot read, short for the test
	t.Setenv("LO_WRAPPER_UNREADABLE_FLAG", flag)
	run := startTreeRun(t, "")
	defer gpulease.UseReadFault(unreadableWhileFlagged(flag, run.m.Dir()))() // the second reserve, in this process, cannot read it either
	record := filepath.Join(run.m.Dir(), "meta.json")
	// The record is older than the grace the sweep gives a claim in progress, as a long job's is.
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(record, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte("on"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(flag)

	var killedAt time.Time
	granted := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !granted {
		if killedAt.IsZero() && !gpulease.PIDAlive(run.cmdPID) {
			killedAt = time.Now()
		}
		// A second reserve tries for the cards the whole time.
		if l, err := run.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "second reserve", TTL: time.Minute}); err == nil {
			granted = true
			t.Errorf("a second reserve was granted the cards over a record whose holder is alive (the wrapped command alive: %v):\n%s", gpulease.PIDAlive(run.cmdPID), run.stderrText())
			_ = l.Release()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("the record of a lease whose holder is alive must not be swept: %v", err)
	}
	if killedAt.IsZero() {
		t.Fatalf("the wrapper kept its command running past its window over a record it cannot read; it must fail closed:\n%s", run.stderrText())
	}
	said := run.stderrText()
	for _, want := range []string{"cannot be read", "stopping the command"} {
		if !strings.Contains(said, want) {
			t.Errorf("the wrapper says why it stopped its command (%q):\n%s", want, said)
		}
	}
}

// renewWhile is the drain's heartbeat: past its window over a record it cannot read it ends the drain, like a lost lease, and says why.
func TestTheDrainHeartbeatEndsTheDrainOnceTheRecordHasBeenUnreadableForItsWindow(t *testing.T) {
	_, m := leaseFixture(t)
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "drain", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	flag := filepath.Join(t.TempDir(), "unreadable.flag")
	defer gpulease.UseReadFault(unreadableWhileFlagged(flag, m.Dir()))()
	oldWindow, oldPoll := wrapperFenceWindow, wrapperFencePoll
	wrapperFenceWindow, wrapperFencePoll = 400*time.Millisecond, 50*time.Millisecond
	defer func() { wrapperFenceWindow, wrapperFencePoll = oldWindow, oldPoll }()

	lost := make(chan error, 1)
	stop := renewWhile(lease, 50*time.Millisecond, func(err error) { lost <- err })
	defer stop()
	if err := os.WriteFile(flag, []byte("on"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(flag)
	select {
	case err := <-lost:
		if !gpulease.IsRecordUnreadable(err) && !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the drain is ended with the cause (an unreadable record), not as a verdict: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the drain heartbeat went on for ever over a record it cannot read; it must fail closed once its window passes")
	}
}

// The rule that makes the two sides agree: from its last heartbeat to the moment it stops, no keeper is silent for as long as the
// heartbeat TTL the sweep judges a holder alive by, so a keeper has always stopped before an acquirer can be handed its cards.
func TestEveryKeepersSilenceIsShorterThanTheHeartbeatStaleThreshold(t *testing.T) {
	ttl := gpulease.DefaultHeartbeatTTL
	confirm := 3 * holdConfirmPause // the looks before a read is called unreadable
	silence := map[string]time.Duration{
		"the wrapper (a tick, then its window)":                           wrapperTickEvery + wrapperFenceWindow + confirm,
		"the drain heartbeat, then the drain loop (each its window)":      drainRenewEvery + 2*(wrapperFenceWindow+confirm),
		"the hidden holder (a renewal, its window, its release attempts)": holdRenewEvery + holdIORetryFor + confirm + time.Duration(holdReleaseTries)*holdReleasePause,
	}
	for who, d := range silence {
		if d >= ttl {
			t.Errorf("%s is silent for up to %s before it stops, which is not shorter than the heartbeat TTL of %s: an acquirer could be handed its cards while it runs", who, d, ttl)
		}
	}
}
