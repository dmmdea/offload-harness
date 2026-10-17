package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A ComfyUI launch marker is the one thing that ties an instance outside a dead wrapper's job to its lease (tree.go). Node restamps a
// marker through a staged .tmp file and a rename over the live name, so a reader can meet the instant the marker is being installed.
// A marker that cannot be told is UNKNOWN, and unknown is not "no instance": reading it as that had a dead wrapper's lease reclaimed at
// once while its instance still ran on the card. The lease is held for as long as the marker is being installed (the staged file is
// fresh for 3 s at most), and is reclaimed as before once it is not.

// deadWrapperWithMarker is a card lease whose wrapper is gone and whose recorded tree is gone, and a launch marker that names the lease
// and a live instance: the lease is held by the marker alone.
func deadWrapperWithMarker(t *testing.T) (m *Manager, l *Lease, marker string) {
	t.Helper()
	m, now := scopedManager(t)
	l = mustDevices(t, m, card0)
	if err := l.RecordTree([]TreeMember{{PID: 9001, StartMs: 4242}}); err != nil {
		t.Fatal(err)
	}
	markerFixture(t, ".offload-launch-p8189.json", map[string]any{
		"startedAt": now.UnixMilli(), "pid": 9100, "ownerPid": 9001, "args": []string{"main.py", "--port", "8189"},
		"key": "p8189", "port": 8189, "leaseEpoch": l.Epoch(),
	})
	m.procStart = func(pid int) (int64, bool) { return now.UnixMilli() - 5000, true } // began before its marker
	liveSet(t, 9100, 5000)                                                            // the launcher and the wrapper are gone, the instance lives
	return m, l, filepath.Join(ComfyDir(), ".offload-launch-p8189.json")
}

func TestALaunchMarkerBeingInstalledHoldsTheLeaseOfADeadWrapper(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, marker string)
	}{
		{"the name is absent and a fresh staged file stands beside it", func(t *testing.T, marker string) {
			if err := os.Rename(marker, marker+".tmp"); err != nil {
				t.Fatal(err)
			}
			fresh := time.Now()
			if err := os.Chtimes(marker+".tmp", fresh, fresh); err != nil {
				t.Fatal(err)
			}
		}},
		{"the name was listed and its read ended as a rename in flight", func(t *testing.T, marker string) {
			old := openSharedFile
			openSharedFile = func(p string) (*os.File, error) {
				if p == marker {
					return nil, &fs.PathError{Op: "open", Path: p, Err: errRenameInFlight}
				}
				return old(p)
			}
			t.Cleanup(func() { openSharedFile = old })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _, marker := deadWrapperWithMarker(t)
			c.arrange(t, marker)
			_, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
			var held *ErrHeld
			if !errors.As(err, &held) {
				if err == nil {
					t.Fatalf("the lease of a dead wrapper was granted to a second job although its launch marker was being installed (%s)", c.name)
				}
				t.Fatalf("want *ErrHeld while the launch marker is being installed (%s), got %v", c.name, err)
			}
			if info := m.InspectFor([]string{card0}); !info.Held {
				t.Errorf("the lease reads free while its launch marker is being installed (%s)", c.name)
			}
		})
	}
}

// What stays: once nothing is being installed the marker is read as before, so a lease whose instance is gone, and one with no marker
// at all, is reclaimed at once.
func TestALaunchMarkerThatIsNotBeingInstalledStillDecidesAsBefore(t *testing.T) {
	t.Run("a staged leftover older than a write can take", func(t *testing.T) {
		m, _, marker := deadWrapperWithMarker(t)
		if err := os.Rename(marker, marker+".tmp"); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(marker+".tmp", old, old); err != nil {
			t.Fatal(err)
		}
		next, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
		if err != nil {
			t.Fatalf("a staged leftover that is a minute old is a writer that died, not a rename in flight: %v", err)
		}
		_ = next.Release()
	})
	t.Run("no marker at all", func(t *testing.T) {
		m, _, marker := deadWrapperWithMarker(t)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		next, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
		if err != nil {
			t.Fatalf("a dead wrapper with no live instance is reclaimed at once: %v", err)
		}
		_ = next.Release()
	})
	t.Run("the instance of the marker is gone", func(t *testing.T) {
		m, _, _ := deadWrapperWithMarker(t)
		liveSet(t, 5000)
		next, err := asPID(m, 5000).TryAcquire(ClassMedia, Options{Devices: []string{card0}, TTL: time.Hour})
		if err != nil {
			t.Fatalf("a marker whose process is gone holds nothing: %v", err)
		}
		_ = next.Release()
	})
}
