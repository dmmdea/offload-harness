package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The warm-back's heartbeat is the same loop shape as the drain's, and had the
// same defect: it ended on the FIRST failed Renew, so one heartbeat write that
// failed with the lease still ours cancelled the warm ("LEASE LOST while warming
// ... abandoning the warm") and left the seat cold with the warm still owed.
// Only a lease that is actually gone abandons the warm; a failed write is
// reported once and retried.
func TestTheWarmBacksHeartbeatSurvivesATransientWriteFailure(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 600 * time.Millisecond}
	cfgPath, m := warmOrderFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 50 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	leaseDir, err := gpulease.LeaseDir("", m.Root())
	if err != nil {
		t.Fatal(err)
	}
	// While the warm runs, every heartbeat write fails (a directory in the
	// scratch name) and the record stays ours; the writes work again before the
	// warm ends.
	blocker := filepath.Join(leaseDir, "hb."+strconv.FormatUint(holder.Epoch(), 10)+".tmp")
	f.onWarm = func() {
		_ = os.Mkdir(blocker, 0o777)
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = os.Remove(blocker)
		}()
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if strings.Contains(out.String(), "LEASE LOST") {
		t.Errorf("one failed heartbeat write is not a lost lease: %s", out.String())
	}
	if m.SeatWarmOwed() != "" || !strings.Contains(out.String(), "warmed back") {
		t.Fatalf("the warm must finish and clear the owed marker (owed=%q): %s", m.SeatWarmOwed(), out.String())
	}
}

// The other half: a lease that is actually gone mid-warm still abandons the
// warm — loudly, and with the warm left owed for whoever holds the card next.
func TestAWarmIsAbandonedWhenTheLeaseIsActuallyLostMidWarm(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 600 * time.Millisecond}
	cfgPath, m := warmOrderFixture(t, f)
	old := drainRenewEvery
	drainRenewEvery = 50 * time.Millisecond
	t.Cleanup(func() { drainRenewEvery = old })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "warming", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	f.onWarm = func() {
		_, _ = m.ReleaseByEpoch(holder.Epoch())
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, holder), &out)
	_ = holder.Release()
	if !strings.Contains(out.String(), "LEASE LOST while warming") || !strings.Contains(out.String(), "warm-back of seat failed") {
		t.Errorf("a lease lost mid-warm must abandon the warm and say so: %s", out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Errorf("an abandoned warm stays owed: owed=%q", m.SeatWarmOwed())
	}
}
