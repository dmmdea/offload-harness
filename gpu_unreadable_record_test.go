package main

// Round 4, items 1 and 2: a record that EXISTS but cannot be read is a lease that is held, never one that is gone.
//
// Inspect() and Leases() read through the reader, which treats an unreadable record as absent, and the release paths believed it:
// a holder whose own release failed over such a record exited 0 and silently left the cards held, and `gpu release` said "no lease
// is held" over one. Reproduced by replacing the record with a directory that cannot be removed as a file.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// unreadableRecord replaces the file at path with a non-empty directory (it exists, cannot be read, cannot be removed as a file) and
// returns the function that puts the file back.
func unreadableRecord(t *testing.T, path string) (restore func()) {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o777); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = os.RemoveAll(path)
		if err := os.WriteFile(path, saved, 0o666); err != nil {
			t.Fatal(err)
		}
	}
}

// ITEM 1. The holder's release over a record it cannot read is a failure it retries and reports; it used to believe Inspect() ("not
// held") and return nil, so the holder exited 0 over a held lease.
func TestTheHoldersReleaseOverARecordItCannotReadIsAFailureNeverSuccess(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	_ = cfgPath
	fastHolderRetries(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	restore := unreadableRecord(t, filepath.Join(m.Dir(), "meta.json"))
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	rerr := releaseHolderLease(m, l)
	if rerr == nil {
		t.Fatal("the record exists and cannot be read: the lease is held, and a release that cannot touch it did not happen")
	}
	if !gpulease.IsRecordUnreadable(rerr) && !strings.Contains(rerr.Error(), "cannot be read") && !strings.Contains(rerr.Error(), "still held") {
		t.Errorf("the failure says why: %v", rerr)
	}

	// The operator's verb takes the same line: with --epoch it never says "no lease is held" over it.
	var verbErr error
	out := captureStdout(t, func() { verbErr = runGPURelease([]string{"--config", cfgPath, "--epoch", "1"}) })
	if verbErr == nil || strings.Contains(out, "no lease is held") {
		t.Errorf("gpu release --epoch over a record it cannot read is an error, not \"no lease is held\" (err=%v, stdout=%q)", verbErr, out)
	}
	var ru *gpulease.RecordUnreadableError
	_ = errors.As(rerr, &ru)
}

// ITEM 2. Bare `gpu release` (no --epoch) on a card-scoped host enumerated the card leases through the reader, which skips a record
// it cannot read, and printed "no lease is held" with exit 0. An entry that exists and cannot be read is an error that names it.
func TestBareGPUReleaseNeverSaysNoLeaseIsHeldOverAnUnreadableCardRecord(t *testing.T) {
	cfgPath, m, _ := holderFixture(t, false)
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{"gpu-test-0"}})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	restore := unreadableRecord(t, filepath.Join(m.Dir(), "e", "1.json"))
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	var verbErr error
	out := captureStdout(t, func() { verbErr = runGPURelease([]string{"--config", cfgPath}) })
	if verbErr == nil || strings.Contains(out, "no lease is held") {
		t.Fatalf("bare gpu release over a card record it cannot read is an error, not \"no lease is held\" with exit 0 (err=%v, stdout=%q)", verbErr, out)
	}
	for _, want := range []string{"epoch 1", "cannot be read", "--epoch 1"} {
		if !strings.Contains(verbErr.Error(), want) {
			t.Errorf("the error must say %q: %v", want, verbErr)
		}
	}
	_ = epoch
}

// ITEM 3 (the CLI half). releaseAfterRAMWait returned early on !Inspect().Held, which an unreadable record satisfies: the detached
// reserve then reported the lease as no longer its own and left it held. An unreadable record is a held lease: it proceeds to stop
// the instances, warm the seat back and release, and reports the release that fails.
func TestTheDetachedReleaseProceedsOverARecordItCannotRead(t *testing.T) {
	_, m, _ := holderFixture(t, false)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	restore := unreadableRecord(t, filepath.Join(m.Dir(), "meta.json"))
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	cause := errors.New("the bound's failure")
	var got error
	stderr := captureStderr(t, func() { got = releaseAfterRAMWait(m, epoch, loadCfgPath(t.TempDir()+"/none.json"), cause) })
	if strings.Contains(got.Error(), "no longer this reservation's") {
		t.Errorf("a record that cannot be read is not a lease that is no longer ours: %v", got)
	}
	if !strings.Contains(stderr, "LEASE NOT RELEASED (epoch 1)") {
		t.Errorf("the release was attempted and its failure is reported:\n%s", stderr)
	}
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), "STILL HELD") {
		t.Errorf("the cause stays and the error ends by saying the lease is still held: %v", got)
	}
}

// ITEM 1a. A card lease whose record removal succeeds but whose claim removal fails: Release returns the error, the record is gone,
// and releaseHolderLease took "the record is absent" for released and dropped the error (the holder then exited 0 over a lease
// half released; 3769f89f printed LEASE NOT RELEASED there). The error is kept unless the record is verifiably absent AND no claim of
// the epoch remains.
func TestTheHoldersReleaseKeepsTheErrorWhenTheRecordIsGoneButAClaimRemains(t *testing.T) {
	_, m, _ := holderFixture(t, false)
	m.SetCardScoped(true)
	fastHolderRetries(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{"gpu-test-0"}})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()
	restore := gpulease.UseRemoveFault(func(path string) error {
		if strings.HasSuffix(path, ".claim") {
			return errors.New("access denied")
		}
		return nil
	})
	t.Cleanup(func() { restore(); releaseAll(t, m) })

	rerr := releaseHolderLease(m, l)
	if rerr == nil || !strings.Contains(rerr.Error(), "access denied") {
		t.Fatalf("the record is gone but a claim of the epoch is not: the release did not complete, and the error says why: %v", rerr)
	}
	if st, _ := m.RecordState(epoch); st != gpulease.RecordAbsent {
		t.Errorf("the scenario is a removed record: %v", st)
	}
	if claims := m.ClaimsOf(epoch); len(claims) != 1 {
		t.Errorf("the claim that could not be removed is still there: %v", claims)
	}

	// A lease that moved on is the one error that clears it: the epoch is not this caller's any more.
	if !gpulease.IsLeaseMovedOn((&gpulease.ErrLeaseMovedOn{Asked: 1, Current: 2})) || gpulease.IsLeaseMovedOn(errors.New("access denied")) {
		t.Error("IsLeaseMovedOn tells the moved-on refusal from any other error")
	}
}

// 1a, the other half: stopInstancesOfLease over a record it cannot read does not say the lease is no longer ours.
func TestStopInstancesOverAnUnreadableRecordSaysSoAndNotThatTheLeaseIsNoLongerOurs(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, []comfyinst.Outcome{{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true}})
	l := cardLease(t, m, "ours", "gpu-test-0")
	restore := gpulease.UseCheckFault(func(uint64) error {
		return &gpulease.RecordUnreadableError{Epoch: l.Epoch(), Err: errors.New("access denied")}
	})
	t.Cleanup(restore)
	var sb strings.Builder
	stopInstancesOfLease(cfgOf(t, cfgPath), l, &sb)
	if got := calls(); len(got) != 0 {
		t.Fatalf("a record that cannot be read does not show the lease is ours: nothing is stopped: %+v", got)
	}
	out := sb.String()
	if strings.Contains(out, "no longer ours") || !strings.Contains(out, "its record cannot be read") || !strings.Contains(out, "access denied") {
		t.Errorf("the line says the record cannot be read, not that the lease is no longer ours: %q", out)
	}
}

// ITEM 8. On Windows under load, a heartbeat Renew/Check that races an atomic replace of the lease record reads the lease as gone. The
// wrapper's tick then printed LEASE LOST and KILLED the wrapped command, renewWhile cancelled the drain, and the drain loop released
// the lease and queued again: a queue or yield write racing a long holder's Renew could kill a long GPU job. A record that cannot be
// read is not a verdict: the wrapper looks again, waits out a short window, and never kills, releases or requeues on it.
//
// An out-of-process wrapper (as the tree tests run it), its record replaced by a directory for a few ticks (150 ms each) and then put
// back: the command must be alive afterwards, the wrapper still running, and no LEASE LOST said.
func TestAWrapperWhoseRecordReadFailsForAMomentDoesNotKillItsCommand(t *testing.T) {
	run := startTreeRun(t, "")
	path := filepath.Join(run.m.Dir(), "meta.json")
	restore := unreadableRecord(t, path)
	time.Sleep(1200 * time.Millisecond) // several wrapper ticks fail to read the record
	restore()
	time.Sleep(900 * time.Millisecond) // and several succeed again

	select {
	case err := <-run.exited:
		t.Fatalf("the wrapper ended over a record read that failed for a moment: %v\n%s", err, run.stderrText())
	default:
	}
	if !gpulease.PIDAlive(run.cmdPID) {
		t.Errorf("the wrapped command (pid %d) was killed over a record read that failed for a moment:\n%s", run.cmdPID, run.stderrText())
	}
	if !gpulease.PIDAlive(run.grand) {
		t.Errorf("what the command started (pid %d) was killed with it:\n%s", run.grand, run.stderrText())
	}
	if strings.Contains(run.stderrText(), "LEASE LOST") {
		t.Errorf("a record that could not be read for a moment is not a lost lease:\n%s", run.stderrText())
	}
	if !run.m.Inspect().Held {
		t.Errorf("the lease is still held: %+v", run.m.Inspect())
	}
}
