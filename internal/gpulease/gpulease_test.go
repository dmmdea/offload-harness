package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager builds a Manager rooted in t.TempDir with injectable clock and
// process-identity, so no test depends on real pids, real time, or the machine's
// actual state root.
func newTestManager(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	m := &Manager{
		root:         root,
		heartbeatTTL: DefaultHeartbeatTTL,
		now:          func() time.Time { return now },
		procStart:    func(int) (int64, bool) { return 4242, true },
		pid:          os.Getpid(),
	}
	return m, &now
}

func TestAcquireReleaseRoundTrip(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if info := m.Inspect(); !info.Held || info.Class != ClassMedia || info.Reason != "render" {
		t.Fatalf("Inspect after acquire = %+v, want held media/render", info)
	}
	if err := l.Check(); err != nil {
		t.Fatalf("Check while held: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("still held after release: %+v", info)
	}
}

// A live, unexpired holder must block a second acquisition — and the error must
// carry the holder so a caller can report a real ETA instead of a bare failure.
func TestSecondAcquireIsRefusedWithHolderDetail(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassText, Options{Reason: "H3 bench"}); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "hero render"})
	if err == nil {
		t.Fatal("second acquire succeeded while text reservation was held")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("error = %T (%v), want *ErrHeld", err, err)
	}
	if held.Info.Class != ClassText || held.Info.Reason != "H3 bench" {
		t.Fatalf("ErrHeld.Info = %+v, want the text reservation", held.Info)
	}
}

// THE INCIDENT, as a test: a text reservation is held and a media job arrives.
// Media must be refused, because acquiring is what would let it call freeLlamaSwap
// and tear the tier down mid-benchmark.
func TestTextReservationBlocksMedia(t *testing.T) {
	m, _ := newTestManager(t)
	bench, err := m.TryAcquire(ClassText, Options{Reason: "kv bench", TTL: 45 * time.Minute})
	if err != nil {
		t.Fatalf("bench reserve: %v", err)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "dispatched render"}); err == nil {
		t.Fatal("media acquired the GPU while a text reservation was held — this is the incident")
	}
	// And once the bench releases, the render proceeds.
	if err := bench.Release(); err != nil {
		t.Fatalf("bench release: %v", err)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "dispatched render"}); err != nil {
		t.Fatalf("media could not acquire after the bench released: %v", err)
	}
}

func TestReclaimsDeadHolder(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "crashed"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return false } // holder is provably gone
	t.Cleanup(func() { pidAliveFn = orig })

	if info := m.Inspect(); info.Held {
		t.Fatalf("dead holder still reported held: %+v", info)
	}
	if _, err := m.TryAcquire(ClassText, Options{Reason: "next"}); err != nil {
		t.Fatalf("could not reclaim a dead holder's lease: %v", err)
	}
}

// A recycled pid reads as alive forever; only the start-time comparison catches it.
func TestReclaimsRecycledPID(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "old process"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// pid still "alive", but it is a DIFFERENT process now.
	m.procStart = func(int) (int64, bool) { return 9999, true }
	if info := m.Inspect(); info.Held {
		t.Fatalf("recycled pid still reported held: %+v", info)
	}
}

// ---------------------------------------------------------------------------
// The reclaim conjunction — the highest-value behaviour in this package.
// ---------------------------------------------------------------------------

// A stale heartbeat ALONE must not reclaim. This is the case that matters most:
// under the saturating benchmark a text reservation exists to protect, the holder
// can be descheduled well past any short heartbeat TTL. Reclaiming there would hand
// the card to a render mid-measurement — i.e. reproduce the very incident.
func TestStaleHeartbeatAloneDoesNotReclaim(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	meta := &Meta{
		Holder:       Holder{PID: os.Getpid(), StartTimeMs: 4242},
		AcquiredAtMs: now.Add(-40 * time.Minute).UnixMilli(),
		RenewedAtMs:  now.Add(-30 * time.Minute).UnixMilli(), // very stale heartbeat
		ExpiresAtMs:  now.Add(15 * time.Minute).UnixMilli(),  // but window NOT expired
	}
	alive := func(int) bool { return true }
	orig := pidAliveFn
	pidAliveFn = alive
	t.Cleanup(func() { pidAliveFn = orig })

	if Reclaimable(meta, now, DefaultHeartbeatTTL, func(int) (int64, bool) { return 4242, true }) {
		t.Fatal("reclaimed on a stale heartbeat alone; a descheduled bench would lose the GPU mid-run")
	}
}

// An expired window ALONE must not reclaim either, so long as the holder is
// heartbeating — a job legitimately running longer than declared is still alive.
func TestExpiredWindowAloneDoesNotReclaim(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	meta := &Meta{
		Holder:       Holder{PID: os.Getpid(), StartTimeMs: 4242},
		AcquiredAtMs: now.Add(-90 * time.Minute).UnixMilli(),
		RenewedAtMs:  now.Add(-5 * time.Second).UnixMilli(),  // fresh heartbeat
		ExpiresAtMs:  now.Add(-10 * time.Minute).UnixMilli(), // window expired
	}
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return true }
	t.Cleanup(func() { pidAliveFn = orig })

	if Reclaimable(meta, now, DefaultHeartbeatTTL, func(int) (int64, bool) { return 4242, true }) {
		t.Fatal("reclaimed a still-heartbeating holder just because its declared window lapsed")
	}
}

// BOTH stale AND expired => reclaim. This is the dead-detached-bench case: the
// proxy holder is alive so pid-liveness cannot help, but nothing is renewing and
// the declared window is over.
func TestStaleAndExpiredReclaims(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	meta := &Meta{
		Holder:       Holder{PID: os.Getpid(), StartTimeMs: 4242},
		AcquiredAtMs: now.Add(-90 * time.Minute).UnixMilli(),
		RenewedAtMs:  now.Add(-30 * time.Minute).UnixMilli(),
		ExpiresAtMs:  now.Add(-10 * time.Minute).UnixMilli(),
	}
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return true }
	t.Cleanup(func() { pidAliveFn = orig })

	if !Reclaimable(meta, now, DefaultHeartbeatTTL, func(int) (int64, bool) { return 4242, true }) {
		t.Fatal("a dead detached holder held the card past both its heartbeat and its window")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat — kept in a PER-EPOCH file, not in the claim record
// ---------------------------------------------------------------------------

// Renewing by read-modify-writing meta.json is a read-then-write race: a reclaim plus a
// fresh acquisition landing between the read and the write lets the OLD holder stamp its
// stale record over the LIVE one. Keying the heartbeat by epoch makes a stale writer
// harmless — it updates a file nothing reads any more.
func TestRenewDoesNotRewriteTheClaimRecord(t *testing.T) {
	m, _ := newTestManager(t)
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "render"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before, err := os.ReadFile(m.metaPath())
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if err := lease.Renew(); err != nil {
		t.Fatalf("renew: %v", err)
	}
	after, err := os.ReadFile(m.metaPath())
	if err != nil {
		t.Fatalf("re-read record: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("Renew rewrote the claim record; a stale holder could then stamp over a live one")
	}
}

// A holder that keeps renewing must stay held even PAST its declared window — the
// conjunction requires a stale heartbeat too. If the heartbeat were still read from the
// frozen record field, this holder would look abandoned the moment its window lapsed.
func TestRenewingHolderSurvivesItsDeclaredWindow(t *testing.T) {
	m, now := newTestManager(t)
	// Use REAL process identity here: this test compares the Manager's view against
	// InspectDir, and InspectDir necessarily uses the real processStart. With the
	// harness's stub the record would carry a fake start time, InspectDir would read a
	// mismatch, and the "disagreement" would be an artifact of the test rather than of
	// the code.
	m.procStart = ProcessStart
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "long render", TTL: time.Minute})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Jump past the declared window, renewing as a live holder would.
	*now = now.Add(30 * time.Minute)
	if err := lease.Renew(); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if info := m.Inspect(); !info.Held {
		t.Fatal("a live, renewing holder past its declared window reported as FREE")
	}
	// And the read-only view must agree — a second reader that reconstructs the
	// judgement from the record alone is how this diverges.
	if info := inspectDirAt(m.leaseDir(), *now, DefaultHeartbeatTTL); !info.Held {
		t.Fatal("InspectDir disagrees with the Manager about a renewing holder")
	}
}

// A heartbeat for a SUPERSEDED epoch must not keep the current lease alive, and must not
// be mistaken for the current holder's.
func TestStaleEpochHeartbeatIsIgnored(t *testing.T) {
	m, now := newTestManager(t)
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "first", TTL: time.Minute})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	staleEpoch := lease.Epoch()
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return false }
	fresh, err := m.TryAcquire(ClassText, Options{Reason: "second", TTL: time.Minute})
	pidAliveFn = orig
	if err != nil {
		t.Fatalf("reclaim acquire: %v", err)
	}

	// The old holder keeps heartbeating its own (now superseded) epoch.
	*now = now.Add(30 * time.Minute)
	_ = os.WriteFile(m.heartbeatPath(staleEpoch),
		[]byte(strconv.FormatInt(now.UnixMilli(), 10)), 0o666)

	// The CURRENT lease has not renewed and its window has lapsed, so it is reclaimable
	// regardless of the stale writer's heartbeat.
	if info := m.Inspect(); info.Held {
		t.Fatalf("a superseded epoch's heartbeat kept the current lease alive: %+v", info)
	}
	_ = fresh
}

// ---------------------------------------------------------------------------
// Fencing
// ---------------------------------------------------------------------------

// The lid-close scenario: a holder is reclaimed and someone else takes the card.
// The original holder must FAIL Check() rather than resume and unload models on
// top of the new holder.
func TestFencedOutHolderFailsCheck(t *testing.T) {
	m, _ := newTestManager(t)
	stale, err := m.TryAcquire(ClassMedia, Options{Reason: "suspended"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Holder disappears; a second party reclaims and acquires.
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return false }
	fresh, err := m.TryAcquire(ClassText, Options{Reason: "new owner"})
	pidAliveFn = orig
	if err != nil {
		t.Fatalf("reclaim acquire: %v", err)
	}
	if fresh.Epoch() <= stale.Epoch() {
		t.Fatalf("epoch did not advance: stale=%d fresh=%d", stale.Epoch(), fresh.Epoch())
	}
	if err := stale.Check(); err == nil {
		t.Fatal("fenced-out holder passed Check(); it would proceed to unload models under the new holder")
	}
}

// A fenced-out straggler calling Release() must NOT delete the current holder's
// lease. Leaking a lease is recoverable; silently handing the GPU to a third party
// is not.
func TestFencedOutReleaseDoesNotStealCurrentLease(t *testing.T) {
	m, _ := newTestManager(t)
	stale, err := m.TryAcquire(ClassMedia, Options{Reason: "suspended"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return false }
	if _, err := m.TryAcquire(ClassText, Options{Reason: "new owner"}); err != nil {
		t.Fatalf("reclaim acquire: %v", err)
	}
	pidAliveFn = orig

	if err := stale.Release(); err != nil {
		t.Fatalf("stale release returned error: %v", err)
	}
	info := m.Inspect()
	if !info.Held || info.Reason != "new owner" {
		t.Fatalf("stale Release() destroyed the current holder's lease: %+v", info)
	}
}

// The epoch must never repeat or go backwards, even as leases are reclaimed —
// that is what makes it usable as a fencing token. It lives OUTSIDE the lease dir
// precisely so removing a lease cannot reset it.
//
// Every round's predecessor is dead, so each acquisition is a reclaim. pidAliveFn is
// stubbed dead for the WHOLE loop rather than toggled per round: toggling it back to
// alive before the next TryAcquire makes the previous lease legitimately held, and
// the acquisition is then correctly refused instead of reclaiming.
func TestEpochIsMonotonicAcrossReclaims(t *testing.T) {
	m, _ := newTestManager(t)
	orig := pidAliveFn
	pidAliveFn = func(int) bool { return false }
	t.Cleanup(func() { pidAliveFn = orig })

	var last uint64
	for i := 0; i < 4; i++ {
		l, err := m.TryAcquire(ClassMedia, Options{})
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if l.Epoch() <= last {
			t.Fatalf("epoch did not advance on reclaim %d: %d after %d", i, l.Epoch(), last)
		}
		last = l.Epoch()
	}
	if last < 4 {
		t.Fatalf("expected at least 4 distinct epochs, ended at %d", last)
	}
}

// ---------------------------------------------------------------------------
// State-root validation — refuse, never degrade
// ---------------------------------------------------------------------------

// Both separator styles are asserted ON EVERY PLATFORM, deliberately.
//
// This suite used to list only backslash paths, and the matcher split segments with
// filepath.ToSlash — a no-op on Linux. So on Linux every one of these was a single
// segment, nothing matched, the refusal did nothing, and the test failed there while
// passing on Windows. It stayed red in CI for a day because all local verification is
// Windows. A guard that only works on the OS it was written on is worse than no guard,
// since it is trusted everywhere.
func TestResolveStateRootRefusesCloudSyncedPaths(t *testing.T) {
	for _, p := range []string{
		`G:\My Drive\AI Ecosystem\state`,
		`C:\Users\x\OneDrive\state`,
		`/home/x/Dropbox/state`,
		`D:\My Drive\state`,
		// The real-world spellings that exact-segment matching alone accepted.
		`C:\Users\x\OneDrive - Contoso\state`,
		`C:\Users\x\Dropbox (Personal)\state`,
		`C:\Users\x\iCloudDrive\state`,
		`G:\Shared drives\team\state`,
		// The same roots spelled POSIX-style, so neither separator can regress unseen.
		`/mnt/g/My Drive/AI Ecosystem/state`,
		`/home/x/OneDrive/state`,
		`/home/x/OneDrive - Contoso/state`,
		`/home/x/Dropbox (Personal)/state`,
		`/home/x/iCloudDrive/state`,
		`/mnt/g/Shared drives/team/state`,
	} {
		if _, err := ResolveStateRoot(p); err == nil {
			t.Errorf("ResolveStateRoot(%q) accepted a cloud-synced root; a replicated LOCK FILE "+
				"would hand one GPU to two machines", p)
		}
	}
}

// The marker match must be segment-aware, not substring — a legitimate directory
// whose name merely contains a marker must still be accepted.
func TestResolveStateRootAcceptsLookalikePaths(t *testing.T) {
	for _, p := range []string{
		filepath.Join(t.TempDir(), "dropbox-exporter"),
		filepath.Join(t.TempDir(), "onedrive-backup-tool"),
	} {
		if _, err := ResolveStateRoot(p); err != nil {
			t.Errorf("ResolveStateRoot(%q) refused a legitimate path: %v", p, err)
		}
	}
}

func TestResolveStateRootPrecedence(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit")
	envRoot := filepath.Join(t.TempDir(), "fromenv")
	t.Setenv("LOCAL_OFFLOAD_STATE_DIR", envRoot)

	got, err := ResolveStateRoot(explicit)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if got != explicit {
		t.Errorf("explicit override lost: got %q want %q", got, explicit)
	}
	got, err = ResolveStateRoot("")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if got != envRoot {
		t.Errorf("env root lost: got %q want %q", got, envRoot)
	}
}

// Refuse-to-start, not silent per-user fallback: the silent fallback is the exact
// bug this package exists to fix.
func TestOpenRefusesUnwritableRootInsteadOfFallingBack(t *testing.T) {
	if os.Getenv("CI_SKIP_PERM") != "" {
		t.Skip("permission semantics not exercised here")
	}
	root := t.TempDir()
	blocker := filepath.Join(root, "gpu")
	// Make <root>/gpu a FILE, so MkdirAll and any write beneath it must fail.
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o666); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := Open(root)
	if err == nil {
		t.Fatal("Open succeeded on an unusable state root; it must refuse rather than degrade")
	}
	if !strings.Contains(err.Error(), "gpulease:") {
		t.Errorf("error not attributed to gpulease: %v", err)
	}
}

func TestUnknownClassRefused(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(Class("gpu-ish"), Options{}); err == nil {
		t.Fatal("unknown class accepted; a typo would create a third unarbitrated category")
	}
	if !ClassMedia.Valid() || !ClassText.Valid() || Class("").Valid() {
		t.Fatal("Class.Valid is wrong")
	}
}

// A failed stamp must not leave an unowned CLAIM behind that blocks everyone.
//
// The lease DIRECTORY legitimately survives — it is a container, not a claim, and
// another acquirer may be working inside it. The invariant is about meta.json, the
// single token both languages contend on. (An earlier version of this test asserted
// the directory was gone, which only held while Go used the directory itself as its
// atomic token — the very asymmetry that let Go and Node both hold the lease.)
func TestFailedStampLeavesNoOrphanClaim(t *testing.T) {
	m, _ := newTestManager(t)
	// Make the epoch path a DIRECTORY so bumpEpoch's write fails.
	if err := os.MkdirAll(m.epochPath(), 0o777); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{}); err == nil {
		t.Fatal("acquire succeeded despite an unwritable epoch counter")
	}
	if _, err := os.Stat(m.metaPath()); !os.IsNotExist(err) {
		t.Fatal("a claim survived a failed stamp; the GPU would be blocked by nobody")
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("the lease reads as HELD after a failed stamp: %+v", info)
	}
}

// realClockManager is a Manager on a temp root using the REAL clock. Tests that run
// acquirers concurrently need it: newTestManager's fake clock is a plain variable, and
// several goroutines reading and advancing it is a data race, not a test.
func realClockManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return &Manager{
		root:         root,
		heartbeatTTL: DefaultHeartbeatTTL,
		now:          time.Now,
		sleep:        time.Sleep,
		pollEvery:    2 * time.Millisecond,
		procStart:    func(int) (int64, bool) { return 4242, true },
		pid:          os.Getpid(),
	}
}

// A fencing token that two leases share is not a cosmetic defect. The token is threaded
// to child processes as GPU_LEASE_EPOCH, so a straggling child of the FIRST lease passes
// Check() against the SECOND, may unload models on top of it, and its release deletes
// the live holder's claim. Read-modify-write on the counter is what produced duplicates:
// two acquirers both read n and both wrote n+1, which tmp+rename cannot prevent because
// it makes each WRITE atomic, not the increment.
func TestConcurrentAcquirersNeverShareAFencingToken(t *testing.T) {
	m := realClockManager(t)
	const workers, each = 8, 15
	tokens := make(chan uint64, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				v, err := m.bumpEpoch()
				if err != nil {
					t.Errorf("bumpEpoch: %v", err)
					return
				}
				tokens <- v
			}
		}()
	}
	wg.Wait()
	close(tokens)

	seen := map[uint64]bool{}
	for v := range tokens {
		if seen[v] {
			t.Fatalf("token %d was issued twice; a straggler holding it would pass Check() against a later lease", v)
		}
		seen[v] = true
	}
	if len(seen) != workers*each {
		t.Fatalf("issued %d distinct tokens, want %d", len(seen), workers*each)
	}
	for i := uint64(1); i <= workers*each; i++ {
		if !seen[i] {
			t.Errorf("token %d was never issued; the counter did not advance one at a time", i)
		}
	}
}

// The end-to-end version of the same invariant: real acquire/release cycles under
// contention must never hand one epoch to two different leases.
func TestConcurrentAcquireReleaseCyclesNeverReuseAnEpoch(t *testing.T) {
	m := realClockManager(t)
	const workers, each = 6, 8
	var mu sync.Mutex
	seen := map[uint64]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				l, err := m.Acquire(ClassMedia, Options{Reason: "job", Wait: 30 * time.Second})
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				mu.Lock()
				seen[l.Epoch()]++
				mu.Unlock()
				if err := l.Release(); err != nil {
					t.Errorf("Release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for epoch, n := range seen {
		if n > 1 {
			t.Errorf("epoch %d was handed to %d different leases", epoch, n)
		}
	}
	if len(seen) != workers*each {
		t.Errorf("completed %d leases, want %d", len(seen), workers*each)
	}
}

// A busy card must QUEUE the job, not drop it: the lock this package replaced existed to
// serialize GPU work, and a render arriving mid-render should run afterwards.
// The holder is `media` on purpose: a media expiry is a timeout ceiling, so the waiter
// must actually wait it out. A `text` holder declaring a window past ours short-circuits
// instead — see TestALongTextReservationDefersImmediately.
func TestAcquireWaitsOutAHolderThenTakesTheCard(t *testing.T) {
	m, now := newTestManager(t)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "render", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	probes := 0
	m.sleep = func(d time.Duration) {
		probes++
		*now = now.Add(d)
		if probes == 2 {
			if rerr := holder.Release(); rerr != nil {
				t.Errorf("holder release: %v", rerr)
			}
		}
	}
	lease, err := m.Acquire(ClassMedia, Options{Reason: "video", Wait: 90 * time.Second})
	if err != nil {
		t.Fatalf("Acquire dropped a job it should have queued: %v", err)
	}
	if probes != 2 {
		t.Errorf("took the card after %d probes, want 2 (one per second until the holder released)", probes)
	}
	if lease.Class() != ClassMedia {
		t.Errorf("class = %q, want %q", lease.Class(), ClassMedia)
	}
}

// The wait is BOUNDED, and what comes back is an honest ETA rather than a bare failure:
// an agent caller can act on "held by text, 45m reservation", not on "error".
func TestAcquireGivesUpAfterItsWindowAndNamesTheHolder(t *testing.T) {
	m, now := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "long video", TTL: 45 * time.Minute}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	probes := 0
	m.sleep = func(d time.Duration) { probes++; *now = now.Add(d) }

	_, err := m.Acquire(ClassMedia, Options{Reason: "video", Wait: 5 * time.Second})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want *ErrHeld carrying the holder", err)
	}
	if held.Info.Class != ClassMedia || held.Info.Reason != "long video" {
		t.Errorf("holder detail lost: %+v", held.Info)
	}
	if probes != 5 {
		t.Errorf("probed %d times in a 5s window at 1s intervals, want 5", probes)
	}
}

// Waiting cannot fix an unusable lease location, so a configuration fault must come back
// immediately instead of burning the caller's whole window first.
func TestAcquireDoesNotWaitOutAConfigurationFault(t *testing.T) {
	m, _ := newTestManager(t)
	slept := 0
	m.sleep = func(time.Duration) { slept++ }

	if _, err := m.Acquire(Class("gpu"), Options{Wait: time.Hour}); err == nil {
		t.Fatal("an unknown class was accepted")
	}
	if slept != 0 {
		t.Errorf("waited %d times on a configuration fault; it can never clear", slept)
	}
}

// A waiter must be CHEAP. Issuing a fencing token takes a machine-wide lock and four
// file operations, and doing that on every probe churned the counter ~90 times per wait
// for claims that could not possibly succeed.
func TestAWaitingAcquirerDoesNotChurnTheFencingCounter(t *testing.T) {
	m, now := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "render", TTL: time.Hour}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	before, err := m.readEpoch()
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	probes := 0
	m.sleep = func(d time.Duration) { probes++; *now = now.Add(d) }

	if _, err := m.Acquire(ClassMedia, Options{Reason: "second", Wait: 10 * time.Second}); err == nil {
		t.Fatal("acquired a card that is held")
	}
	if probes != 10 {
		t.Fatalf("probed %d times in a 10s window, want 10", probes)
	}
	after, err := m.readEpoch()
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if after != before {
		t.Errorf("the fencing counter advanced %d times while merely WAITING (%d -> %d); "+
			"a waiter must not issue tokens it cannot use", after-before, before, after)
	}
}

// Waiting for something that cannot happen is pure friction. A `text` reservation is an
// operator's DECLARED duration, so one that outlasts the whole window is answered now.
func TestALongTextReservationDefersImmediately(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassText, Options{Reason: "45m bench", TTL: 45 * time.Minute}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	slept := 0
	m.sleep = func(time.Duration) { slept++ }

	_, err := m.Acquire(ClassMedia, Options{Reason: "video", Wait: 90 * time.Second})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want *ErrHeld", err)
	}
	if held.Info.Class != ClassText {
		t.Errorf("holder detail lost: %+v", held.Info)
	}
	if slept != 0 {
		t.Errorf("waited %d times for a reservation declared to outlast the window", slept)
	}
}

// The converse, so the short-circuit cannot quietly grow into "never wait": a media
// holder's expiry is a TIMEOUT CEILING, not a promise — a 45-minute video budget
// routinely finishes in three — so it must still be waited out.
func TestALongMediaHolderIsStillWaitedOut(t *testing.T) {
	m, now := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "long video", TTL: 45 * time.Minute}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	slept := 0
	m.sleep = func(d time.Duration) { slept++; *now = now.Add(d) }

	if _, err := m.Acquire(ClassMedia, Options{Reason: "second", Wait: 4 * time.Second}); err == nil {
		t.Fatal("acquired a held card")
	}
	if slept == 0 {
		t.Error("did not wait at all for a media holder; its expiry is a ceiling, not a promise")
	}
}

// A short text reservation may genuinely free inside the window, so it is waited out too.
func TestAShortTextReservationIsStillWaitedOut(t *testing.T) {
	m, now := newTestManager(t)
	if _, err := m.TryAcquire(ClassText, Options{Reason: "quick eval", TTL: 5 * time.Second}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	slept := 0
	m.sleep = func(d time.Duration) { slept++; *now = now.Add(d) }

	if _, err := m.Acquire(ClassMedia, Options{Reason: "video", Wait: 60 * time.Second}); err == nil {
		t.Fatal("acquired a held card")
	}
	if slept == 0 {
		t.Error("did not wait for a reservation that expires INSIDE the window")
	}
}

// Zero Wait is exactly the old single-try behaviour, so callers that genuinely want to
// fail fast keep it.
func TestAcquireWithoutAWaitIsASingleTry(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", TTL: time.Hour}); err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	slept := 0
	m.sleep = func(time.Duration) { slept++ }

	var held *ErrHeld
	if _, err := m.Acquire(ClassMedia, Options{Reason: "second"}); !errors.As(err, &held) {
		t.Fatalf("err = %v, want *ErrHeld", err)
	}
	if slept != 0 {
		t.Errorf("slept %d times with Wait unset", slept)
	}
}
