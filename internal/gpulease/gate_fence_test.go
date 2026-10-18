package gpulease

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The fence reads the record of the lease it holds. A record whose rename never ends cannot be read, and that is not the lease being
// gone: Check answers RecordUnreadableError, which a caller that would let go of its lease on a failed fence can tell from "gone"
// (confirmFence retries it; "gone" it takes as the verdict). Before the gate, the read answered ENOENT and Check said "lease is gone".
func TestTheFenceCallsARecordWhoseRenameNeverEndsUnreadableAndNotGone(t *testing.T) {
	for _, c := range []struct {
		name string
		devs []string
	}{{"card lease", []string{card0}}, {"whole-node lease", nil}} {
		t.Run(c.name, func(t *testing.T) {
			clk := onGateClock(t)
			m := scopedRealClock(t)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "test", Devices: c.devs, TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = os.Remove(stagingPath(m.metaPath()))
				_ = os.Remove(stagingPath(epochRecordPath(m.leaseDir(), l.Epoch())))
				_ = l.Release()
			}()
			record := m.metaPath()
			if len(c.devs) > 0 {
				record = epochRecordPath(m.leaseDir(), l.Epoch())
			}
			// the rename has started and does not end: the live name is gone, the staged file is there and young
			if err := os.Rename(record, stagingPath(record)); err != nil {
				t.Fatal(err)
			}
			at := clk.now()
			if err := os.Chtimes(stagingPath(record), at, at); err != nil {
				t.Fatal(err)
			}
			err = l.Check()
			var unreadable *RecordUnreadableError
			if !errors.As(err, &unreadable) {
				t.Fatalf("Check = %v while the rename of its record never ends, want a RecordUnreadableError", err)
			}
			if strings.Contains(err.Error(), "lease is gone") {
				t.Errorf("Check = %v: the lease is not gone, its record could not be read", err)
			}
			// the rename ends: the same lease checks out
			if err := os.Rename(stagingPath(record), record); err != nil {
				t.Fatal(err)
			}
			if err := l.Check(); err != nil {
				t.Errorf("Check after the rename ended = %v, want nil", err)
			}
		})
	}
}
