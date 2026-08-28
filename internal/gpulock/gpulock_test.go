package gpulock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// deadPID is a pid that will not exist. Liveness is no longer stubbable from this
// package — it delegates to gpulease.PIDAlive precisely so there cannot be a second,
// weaker definition here — so tests use real pids: our own (alive) and this one (dead).
const deadPID = 999999999

// writeLease writes the SHARED lease record (gpulease.Meta), which is what every
// participant now reads. The old helper wrote a legacy {pid,startedAt} shape that only
// this package's now-deleted local parser understood.
func writeLease(t *testing.T, pid int, mutate func(*gpulease.Meta)) string {
	t.Helper()
	lock := filepath.Join(t.TempDir(), "lease")
	if err := os.MkdirAll(lock, 0o777); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m := gpulease.Meta{
		Epoch:        1,
		Class:        gpulease.ClassMedia,
		Holder:       gpulease.Holder{PID: pid},
		AcquiredAtMs: now.UnixMilli(),
		RenewedAtMs:  now.UnixMilli(),
		ExpiresAtMs:  now.Add(time.Hour).UnixMilli(),
	}
	if mutate != nil {
		mutate(&m)
	}
	b, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	return lock
}

// TestPathResolution: config override > GPU_LOCK env > <state-root>/gpu/lease — the
// same order every other lease participant resolves.
func TestPathResolution(t *testing.T) {
	t.Setenv("GPU_LOCK", filepath.Join("env", "gpu.lock"))
	wantCfg, _ := filepath.Abs(filepath.Join("cfg", "gpu.lock"))
	if got := Path(filepath.Join("cfg", "gpu.lock"), ""); got != wantCfg {
		t.Errorf("override must win over env: got %q", got)
	}
	wantEnv, _ := filepath.Abs(filepath.Join("env", "gpu.lock"))
	if got := Path("", ""); got != wantEnv {
		t.Errorf("env must win over default: got %q", got)
	}
	t.Setenv("GPU_LOCK", "")
	stateDir := t.TempDir()
	want := filepath.Join(stateDir, "gpu", "lease")
	if got := Path("", stateDir); got != want {
		t.Errorf("default = %q, want the shared lease dir %q", got, want)
	}
}

// THE REGRESSION THIS PACKAGE SUFFERED, pinned: the read-only view must resolve to the
// exact directory the acquirers write. When the lease moved to a machine-wide root and
// this function was left pointing at the OS temp dir, WaitFree answered "free" forever
// and the vision gate stopped gating — the harness silently quit offloading.
func TestPathMatchesTheLeaseDirectoryAcquirersUse(t *testing.T) {
	t.Setenv("GPU_LOCK", "")
	stateDir := t.TempDir()
	m, err := gpulease.Open(stateDir)
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = lease.Release() }()

	if got := Path("", stateDir); got != lease.Dir() {
		t.Fatalf("gpulock watches %q but acquirers write %q — the gate is blind", got, lease.Dir())
	}
	info := Inspect(Path("", stateDir))
	if !info.Held {
		t.Fatal("gpulock reports a freshly acquired lease as FREE — vision calls would fire into a busy GPU")
	}
	if info.Class != string(gpulease.ClassMedia) {
		t.Errorf("Class = %q, want media; the caller should be able to say WHAT holds the card", info.Class)
	}
}

func TestInspectAbsent(t *testing.T) {
	if info := Inspect(filepath.Join(t.TempDir(), "no-such.lock")); info.Held {
		t.Fatal("absent lease must report not held")
	}
}

func TestInspectHeldByLiveHolder(t *testing.T) {
	lock := writeLease(t, os.Getpid(), func(m *gpulease.Meta) {
		m.AcquiredAtMs = time.Now().Add(-5 * time.Second).UnixMilli()
	})
	info := Inspect(lock)
	if !info.Held {
		t.Fatal("live-holder lease must report held")
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	// Age is time HELD, not time since the last heartbeat.
	if info.Age < 4*time.Second || info.Age > time.Minute {
		t.Errorf("Age = %v, want ~5s (time held, from the acquirer's stamp)", info.Age)
	}
}

func TestInspectDeadHolderIsStale(t *testing.T) {
	lock := writeLease(t, deadPID, nil)
	if info := Inspect(lock); info.Held {
		t.Fatal("dead-holder lease must report not held (stale)")
	}
}

func TestInspectMissingMetaIsStale(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "lease")
	if err := os.MkdirAll(lock, 0o777); err != nil {
		t.Fatal(err)
	}
	if info := Inspect(lock); info.Held {
		t.Fatal("a lease dir with no claim must report not held")
	}
}

// THE THIRD RULE IS GONE — this pins that this package now applies gpulease's rule and
// not its own. The old local rule was (pid dead) OR (mtime older than 1h), so a holder
// that had legitimately held the card for over an hour read as STALE here while gpulease
// considered it live. Under the shared rule, a live heartbeating holder stays held no
// matter how long it has run, and reclaim needs BOTH a stale heartbeat and an expired
// window.
func TestUsesTheSharedStalenessRuleNotAnMtimeTTL(t *testing.T) {
	// Held for 3 hours, still heartbeating, window still open: the OLD rule called this
	// stale at the 1h mark; the shared rule calls it held.
	longRunning := writeLease(t, os.Getpid(), func(m *gpulease.Meta) {
		now := time.Now()
		m.AcquiredAtMs = now.Add(-3 * time.Hour).UnixMilli()
		m.RenewedAtMs = now.UnixMilli()
		m.ExpiresAtMs = now.Add(time.Hour).UnixMilli()
	})
	if info := Inspect(longRunning); !info.Held {
		t.Fatal("a live, heartbeating 3-hour holder reported as FREE — the vision gate would " +
			"fire into a busy GPU (this is the old mtime-TTL rule leaking back in)")
	}

	// Stale heartbeat AND expired window: reclaimable under the shared rule.
	abandoned := writeLease(t, os.Getpid(), func(m *gpulease.Meta) {
		now := time.Now()
		m.AcquiredAtMs = now.Add(-3 * time.Hour).UnixMilli()
		m.RenewedAtMs = now.Add(-1 * time.Hour).UnixMilli()
		m.ExpiresAtMs = now.Add(-30 * time.Minute).UnixMilli()
	})
	if info := Inspect(abandoned); info.Held {
		t.Fatal("an abandoned lease (stale heartbeat AND expired window) still reported held")
	}
}

// Liveness is shared, not reimplemented here.
func TestLivenessIsShared(t *testing.T) {
	if !gpulease.PIDAlive(os.Getpid()) {
		t.Fatal("our own process read as dead")
	}
	if gpulease.PIDAlive(0) || gpulease.PIDAlive(-1) {
		t.Fatal("non-positive pids must never read as alive")
	}
}

func TestWaitFreeBoundedWait(t *testing.T) {
	lock := writeLease(t, os.Getpid(), nil)
	start := time.Now()
	info := WaitFree(context.Background(), lock, 120*time.Millisecond, 20*time.Millisecond)
	if !info.Held {
		t.Fatal("still-held lease must report held after the bounded wait")
	}
	// 15ms epsilon: the Windows system timer ticks at ~15.6ms, so a sleep-based
	// wait can legitimately return a hair under its nominal window.
	if el := time.Since(start); el < 105*time.Millisecond {
		t.Errorf("WaitFree returned after %v, want >= the 120ms wait window (minus timer granularity)", el)
	}
}

func TestWaitFreeReleasedMidWait(t *testing.T) {
	lock := writeLease(t, os.Getpid(), nil)
	released := make(chan error, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		// Retry the release: under full-tree parallel load on Windows a
		// concurrent WaitFree poll holding meta.json open can make a single
		// os.Remove fail with a sharing violation — the old one-shot remove
		// swallowed that error and the test then "flaked" by honestly
		// reporting a lease nothing had released (issue #81).
		var err error
		for i := 0; i < 200; i++ {
			err = os.Remove(filepath.Join(lock, "meta.json"))
			if err == nil || os.IsNotExist(err) {
				err = nil
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		released <- err
	}()
	defer func() {
		if err := <-released; err != nil {
			t.Fatalf("test harness never managed to release the lease: %v", err)
		}
	}()
	start := time.Now()
	info := WaitFree(context.Background(), lock, 5*time.Second, 10*time.Millisecond)
	if info.Held {
		t.Fatal("released lease must report not held")
	}
	if el := time.Since(start); el >= 5*time.Second {
		t.Errorf("WaitFree took the full window (%v) despite the release", el)
	}
}
