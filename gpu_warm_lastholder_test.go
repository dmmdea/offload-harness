package main

import (
	"bytes"
	"context"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// With card-scoped leases several holders share a box. The seat a lease unloaded is owed a
// warm-back, and the warm loads it on ALL its cards: the first lease to finish must not
// load the seat over cards another live lease is still using (three per-card film renders
// on the 3-card box, 2026-10-03: the first card to finish would have loaded the 3-card agent
// seat over the two cards still rendering). The warm stays owed; the last lease on the
// seat's cards pays it.

// warmCardFixture: a host with card-scoped leases on, a green reader audit and a fake
// llama-swap that counts warm requests for the seat.
func warmCardFixture(t *testing.T) (cfgPath string, m *gpulease.Manager, warms *atomic.Int32) {
	t.Helper()
	warms = &atomic.Int32{}
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 10 * time.Millisecond}
	f.onWarm = func() { warms.Add(1) }
	srv := httptest.NewServer(f.handler("seat"))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) +
		`, "agent_model": "seat", "gpu_card_scoped_leases": true}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	m, err := openLease(fs)
	if err != nil {
		t.Fatal(err)
	}
	f.holder = func() string { return "" }
	return cfgPath, m, warms
}

func acquireCard(t *testing.T, m *gpulease.Manager, reason, card string) *gpulease.Lease {
	t.Helper()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: reason, Devices: []string{card}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire %s on %s: %v", reason, card, err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

func TestAWarmBackWaitsForTheLastLeaseOnTheSeatsCards(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	a := acquireCard(t, m, "film card 0", "gpu-aaaa0000-x")
	b := acquireCard(t, m, "film card 2", "gpu-cccc0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// The seat declares no cards here: unknown is every card, so b sits on it.
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
	_ = a.Release()
	if n := warms.Load(); n != 0 {
		t.Fatalf("the first lease to finish loaded the seat over a card lease b still holds (%d warm(s)): %s", n, out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Fatalf("a skipped warm stays owed, owed=%q", m.SeatWarmOwed())
	}
	if !strings.Contains(out.String(), "NOT warming seat back yet") || !strings.Contains(out.String(), "epoch "+strconv.FormatUint(b.Epoch(), 10)) {
		t.Errorf("the skip must name the lease still on the seat's cards: %s", out.String())
	}
	out.Reset()
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, b), &out)
	_ = b.Release()
	if n := warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Fatalf("the last lease on the seat's cards pays the warm: warms=%d owed=%q: %s", n, m.SeatWarmOwed(), out.String())
	}
}

// The rule is about the SEAT'S cards, not about any lease anywhere: a lease on a card the
// seat does not use leaves the warm alone.
func TestAWarmBackIgnoresALeaseOffTheSeatsCards(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	a := acquireCard(t, m, "seat card", "gpu-aaaa0000-x")
	acquireCard(t, m, "other card", "gpu-cccc0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	cfg := loadCfgPath(cfgPath) // arms the seat pins from the config; the test's pins go on after it
	modelaffinity.SetSeatPins(func(model string) ([]string, bool) {
		if model == "seat" {
			return []string{"gpu-aaaa0000"}, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return statusCards(), "", nil
	}))
	var out bytes.Buffer
	warmBackGuarded(cfg, leaseWarmGuard(m, a), &out)
	if n := warms.Load(); n != 1 {
		t.Fatalf("a lease on a card the seat does not use must not hold the warm (%d warm(s)): %s", n, out.String())
	}
}

// `gpu release --warm-seat` with no epoch is "whatever is held", which the release accepts
// only when one lease is live: that lease is the one being released, never an "other".
func TestReleaseWarmSeatWithNoEpochWarmsOverTheOneLiveLease(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	acquireCard(t, m, "detached card 0", "gpu-aaaa0000-x")
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, 0), &out)
	if n := warms.Load(); n != 1 {
		t.Fatalf("release --warm-seat over the one live lease must warm (%d warm(s)): %s", n, out.String())
	}
}

// A release whose lease is already gone leaves a free card: the warm the operator asked
// for runs (nothing held is nobody else's lease).
func TestReleaseWarmSeatAfterTheLeaseEndedWarmsOnTheFreeCard(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	a := acquireCard(t, m, "ended", "gpu-aaaa0000-x")
	_ = a.Release()
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, a.Epoch()), &out)
	if n := warms.Load(); n != 1 {
		t.Fatalf("a warm on a free card must run (%d warm(s)): %s", n, out.String())
	}
}

// `gpu release --warm-seat` takes the same rule: releasing one card lease while another sits
// on the seat's cards leaves the warm owed.
func TestReleaseWarmSeatWaitsForTheOtherLeaseOnTheSeatsCards(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	a := acquireCard(t, m, "detached card 0", "gpu-aaaa0000-x")
	acquireCard(t, m, "film card 2", "gpu-cccc0000-x")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), releaseWarmGuard(m, a.Epoch()), &out)
	if n := warms.Load(); n != 0 || m.SeatWarmOwed() != "seat" {
		t.Fatalf("release --warm-seat warmed over another live lease: warms=%d owed=%q: %s", n, m.SeatWarmOwed(), out.String())
	}
}
