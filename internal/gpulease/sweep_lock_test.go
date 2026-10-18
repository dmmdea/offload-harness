package gpulease

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The sweep of the staged files left by writers that hold the epoch lock (sweepStagedLeftoversLocked) is safe only because the lock is
// held: a staged file found under it is a dead writer's, and one found without it may be the file a live restamp has staged and is about
// to rename. The tests that the sweep removes the files do not say where the sweep runs, so these hold the lock in another party's name,
// plant the leftover, and ask a grant to run: it must wait for the lock before it removes anything.

// holdTheEpochLock takes the lock file as another process would, and returns the function that lets it go.
func holdTheEpochLock(t *testing.T, m *Manager) (release func()) {
	t.Helper()
	lock := m.epochLockPath()
	if err := os.MkdirAll(filepath.Dir(lock), 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	released := false
	release = func() {
		if !released {
			released = true
			_ = os.Remove(lock)
		}
	}
	t.Cleanup(release)
	return release
}

func TestAGrantSweepsTheLeftoversOfTheWritersThatHoldTheLockOnlyWhileItHoldsTheLock(t *testing.T) {
	for _, c := range []struct {
		name   string
		devs   []string
		staged func(m *Manager) string
	}{
		{"a whole-node grant and the staged whole-node record", nil, func(m *Manager) string { return stagingPath(m.metaPath()) }},
		{"a card grant and a staged card record", []string{card0}, func(m *Manager) string { return stagingPath(epochRecordPath(m.leaseDir(), 99)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := scopedRealClock(t)
			staged := c.staged(m)
			if err := os.MkdirAll(filepath.Dir(staged), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(staged, []byte("{}"), 0o666); err != nil {
				t.Fatal(err)
			}
			release := holdTheEpochLock(t, m)

			done := make(chan error, 1)
			go func() {
				l, err := m.TryAcquire(ClassMedia, Options{Reason: "grant", Devices: c.devs, TTL: time.Hour})
				if err == nil {
					_ = l.Release()
				}
				done <- err
			}()

			time.Sleep(500 * time.Millisecond) // the grant is spinning for the lock
			if _, err := os.Stat(staged); err != nil {
				t.Errorf("the staged file was removed by a grant that did not hold the epoch lock (another party did): %v", err)
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the grant after the lock was let go = %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the grant did not finish after the lock was let go")
			}
			if _, err := os.Stat(staged); err == nil {
				t.Errorf("the staged file is still there after a grant that held the lock: the sweep did not run")
			}
		})
	}
}
