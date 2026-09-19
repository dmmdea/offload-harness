package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// Register C-50 (diagnosis S-31 / S-32): a failed drain must not leave the
// DRAINING stamp on a lease that stays held, and a run that heartbeats without
// progressing must not hold the drain for the whole queue budget.

// A busy seat whose state never changes trips the no-progress bound long
// before the overall deadline; a seat whose state keeps changing does not.
func TestDrainGivesUpOnAnUnchangedBusyStateBeforeTheDeadline(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	p := drainProbe{client: srv.Client(), endpoint: srv.URL, model: "seat", every: 2 * time.Millisecond, stuckAfter: 80 * time.Millisecond}
	start := time.Now()
	err := drainUntil(context.Background(), p, time.Now().Add(5*time.Second))
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "without progress") {
		t.Fatalf("an unchanged busy state must fail as stuck, got %v", err)
	}
	if took > 2*time.Second {
		t.Fatalf("the stuck bound must fire near stuckAfter, not at the deadline: took %s", took)
	}
	// Progress resets the bound: toggling the in-flight count keeps the drain
	// alive past several stuckAfter windows, until the overall deadline.
	f.inflight.Store(2)
	stop := make(chan struct{})
	go func() {
		n := int64(2)
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			n = 3 - n // 2 <-> 1, never zero
			f.inflight.Store(n)
		}
	}()
	start = time.Now()
	err = drainUntil(context.Background(), p, time.Now().Add(400*time.Millisecond))
	close(stop)
	took = time.Since(start)
	if err == nil || strings.Contains(err.Error(), "without progress") || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("a changing busy state must run to the overall deadline, got %v after %s", err, took)
	}
	if took < 350*time.Millisecond {
		t.Fatalf("the drain gave up early on a progressing seat: %s", took)
	}
}

// The no-progress bound is derived from the seat's own rate sample, never
// under the drain floor, and disabled when the seat has no sample.
func TestSeatStuckAfterFollowsTheSeatRate(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": "http://127.0.0.1:1", "agent_model": "seat", "agent_max_tokens": 2048}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadCfgPath(cfgPath)
	if got := seatStuckAfter(c, "seat"); got != 0 {
		t.Fatalf("no rate sample must disable the bound, got %s", got)
	}
	if err := writeSeatRate(t, root, "seat", 4.0, 100); err != nil {
		t.Fatal(err)
	}
	// 2 turns of 8192/4 = 2048 s each + 100 s cold = 4196 s.
	if got := seatStuckAfter(c, "seat"); got != 4196*time.Second {
		t.Fatalf("stuckAfter from the sample: got %s want 4196s", got)
	}
	if err := writeSeatRate(t, root, "fast", 1000.0, 0); err != nil {
		t.Fatal(err)
	}
	if got := seatStuckAfter(c, "fast"); got != drainFloor {
		t.Fatalf("a fast seat must still get the floor, got %s", got)
	}
}

// A drain that fails while the lease stays held (the detach form) clears the
// DRAINING stamp so the seat is not cordoned for the rest of the window.
func TestAFailedDrainClearsTheDrainingStamp(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "seat"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if info := m.Inspect(); !info.Held || !info.Draining {
		t.Fatalf("precondition: held and draining, got %+v", info)
	}
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	err = maintainSeat(loadCfgPath(cfgPath), holder.Restamp, true, time.Now().Add(60*time.Millisecond), true, true, nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("the drain must fail at its deadline, got %v", err)
	}
	info := m.Inspect()
	if !info.Held {
		t.Fatal("the lease must stay held after a failed drain")
	}
	if info.Draining {
		t.Fatal("a failed drain left the DRAINING stamp: the seat is cordoned for the whole window")
	}
	if info.Exclusive {
		t.Fatal("a failed drain must not turn the lease exclusive")
	}
	if f.unloads.Load() != 0 {
		t.Fatal("a failed drain must not unload the seat")
	}
}

// loadCfgPath loads a config file the way the verbs do, without a FlagSet.
func loadCfgPath(path string) config.Config {
	cfg, _ := config.LoadWithSource(path)
	return cfg
}

// writeSeatRate records one rate sample for seat in the store the drain reads.
func writeSeatRate(t *testing.T, root, seat string, tokS, coldSec float64) error {
	t.Helper()
	return seatrate.Update(seatrate.Path(root), func(s *seatrate.Store) {
		s.Observe(seat, tokS, coldSec, time.Now())
	})
}
