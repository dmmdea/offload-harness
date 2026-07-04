package gpulock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLock creates a lock dir with a meta.json recording pid, exactly like
// gpu-lock.mjs acquireGpuLock does. Returns the lock path.
func writeLock(t *testing.T, pid int) string {
	t.Helper()
	lock := filepath.Join(t.TempDir(), "gpu.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"pid":%d,"startedAt":%d}`, pid, time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(lock, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return lock
}

// TestPathResolution: config override > GPU_LOCK env > tmpdir default — the
// same order the Node runners resolve (gpu-lock.mjs defaultLockPath).
func TestPathResolution(t *testing.T) {
	t.Setenv("GPU_LOCK", filepath.Join("env", "gpu.lock"))
	if got := Path(filepath.Join("cfg", "gpu.lock")); got != filepath.Join("cfg", "gpu.lock") {
		t.Errorf("override must win over env: got %q", got)
	}
	if got := Path(""); got != filepath.Join("env", "gpu.lock") {
		t.Errorf("env must win over default: got %q", got)
	}
	t.Setenv("GPU_LOCK", "")
	want := filepath.Join(os.TempDir(), DefaultLockName)
	if got := Path(""); got != want {
		t.Errorf("default = %q, want the .mjs tmpdir default %q", got, want)
	}
}

// TestInspectAbsent: no lock dir => free.
func TestInspectAbsent(t *testing.T) {
	if info := Inspect(filepath.Join(t.TempDir(), "no-such.lock")); info.Held {
		t.Fatal("absent lock dir must report not held")
	}
}

// TestInspectHeldByLiveHolder: a fresh lock recording OUR (alive) pid is held,
// with the holder pid surfaced and a sane age.
func TestInspectHeldByLiveHolder(t *testing.T) {
	lock := writeLock(t, os.Getpid())
	// Backdate meta.json so the age is measurably positive.
	old := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(filepath.Join(lock, "meta.json"), old, old); err != nil {
		t.Fatal(err)
	}
	info := Inspect(lock)
	if !info.Held {
		t.Fatal("live-holder lock must report held")
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.Age < 4*time.Second || info.Age > time.Minute {
		t.Errorf("Age = %v, want ~5s (from meta.json mtime)", info.Age)
	}
}

// TestInspectDeadHolderIsStale: same liveness rule as the .mjs — a lock whose
// recorded pid is dead is stale and reports NOT held (regardless of age).
func TestInspectDeadHolderIsStale(t *testing.T) {
	lock := writeLock(t, os.Getpid())
	orig := pidAlive
	pidAlive = func(int) bool { return false } // deterministic dead holder
	defer func() { pidAlive = orig }()
	if info := Inspect(lock); info.Held {
		t.Fatal("dead-holder lock must report not held (stale)")
	}
}

// TestInspectMissingMetaIsStale: a bare lock dir with no meta.json is stale
// (mjs: isStale(null) === true).
func TestInspectMissingMetaIsStale(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "gpu.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if info := Inspect(lock); info.Held {
		t.Fatal("meta-less lock dir must report not held (stale)")
	}
}

// TestInspectTTLCap: even a live-pid lock older than the TTL is stale — the
// age cap / pid-recycle backstop the .mjs keeps.
func TestInspectTTLCap(t *testing.T) {
	lock := writeLock(t, os.Getpid())
	if info := inspectAt(lock, time.Hour, time.Now().Add(2*time.Hour)); info.Held {
		t.Fatal("over-TTL lock must report not held (stale)")
	}
	if info := inspectAt(lock, time.Hour, time.Now()); !info.Held {
		t.Fatal("fresh live-pid lock must report held under the same TTL")
	}
}

// TestPidAliveSelf: the real liveness probe recognizes our own process.
func TestPidAliveSelf(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("pidAlive(self) must be true")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Fatal("pidAlive must reject non-positive pids")
	}
}

// TestWaitFreeBoundedWait: a held lock makes WaitFree poll for the FULL wait
// window and then report held (the caller defers).
func TestWaitFreeBoundedWait(t *testing.T) {
	lock := writeLock(t, os.Getpid())
	start := time.Now()
	info := WaitFree(context.Background(), lock, 120*time.Millisecond, 20*time.Millisecond)
	if !info.Held {
		t.Fatal("still-held lock must report held after the bounded wait")
	}
	if el := time.Since(start); el < 120*time.Millisecond {
		t.Errorf("WaitFree returned after %v, want >= the 120ms wait window", el)
	}
}

// TestWaitFreeReleasedMidWait: releasing the lock mid-wait lets WaitFree
// return free well before the deadline (the caller proceeds).
func TestWaitFreeReleasedMidWait(t *testing.T) {
	lock := writeLock(t, os.Getpid())
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = os.RemoveAll(lock)
	}()
	start := time.Now()
	info := WaitFree(context.Background(), lock, 5*time.Second, 10*time.Millisecond)
	if info.Held {
		t.Fatal("released lock must report not held")
	}
	if el := time.Since(start); el >= 5*time.Second {
		t.Errorf("WaitFree took the full window (%v) despite the release", el)
	}
}
