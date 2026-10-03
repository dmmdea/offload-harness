package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// scopedLeaseFixture is leaseFixture with the per-host switch on, through the same
// config key the CLI reads, on a host whose reader audit is green.
func scopedLeaseFixture(t *testing.T) (string, *gpulease.Manager) {
	t.Helper()
	return scopedLeaseFixtureAudit(t, true)
}

// scopedLeaseFixtureAudit is scopedLeaseFixture with the reader audit marker present (or
// not): the config key alone must never enable the writer.
func scopedLeaseFixtureAudit(t *testing.T, audited bool) (string, *gpulease.Manager) {
	t.Helper()
	root := t.TempDir()
	if audited {
		if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"state_dir": `+strconv.Quote(root)+`, "gpu_card_scoped_leases": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	m, err := openLease(fs)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, m
}

// openLease is what every gpu verb opens the lease with: the config switch must reach
// the Manager, and be off when the config does not set it.
func TestOpenLeaseHonoursTheCardScopedSwitch(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", Devices: []string{"gpu-test-0"}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("with gpu_card_scoped_leases on, a device lease must be grantable: %v", err)
	}
	_ = l.Release()

	_, off := leaseFixture(t)
	if _, err := off.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", Devices: []string{"gpu-test-0"}}); !errors.Is(err, gpulease.ErrCardScopedOff) {
		t.Fatalf("a config that does not set the key must not write device leases: %v", err)
	}
}

// Both callers compare the epoch THEY were told to act on with the live set, not with
// the lowest live epoch.
func TestGPUVerbsRecogniseAHigherEpochDeviceLease(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	low, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "low", Devices: []string{"gpu-test-0"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = low.Release() }()
	high, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "high", Devices: []string{"gpu-test-1"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = high.Release() }()

	cause := errors.New("drain failed")
	if got := detachedMaintainError(m, high.Epoch(), cause); !errors.Is(got, cause) || got.Error() == cause.Error() ||
		!containsAll(got.Error(), "the lease is still held") {
		t.Errorf("the higher-epoch lease is still held; got %v", got)
	}
	g := releaseWarmGuard(m, high.Epoch())
	if err := g.held(); err != nil {
		t.Errorf("releasing the higher epoch must not read as 'the lease has moved on': %v", err)
	}
	if err := releaseWarmGuard(m, high.Epoch()+50).held(); err == nil {
		t.Error("an epoch that is not live has moved on")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

// The config key alone does not enable the writer: without the green reader audit the
// CLI opens the lease with the writer OFF, so a device request is refused rather than
// written where an older reader on this host would read it as a free card.
func TestOpenLeaseKeepsTheWriterOffWithoutAGreenReaderAudit(t *testing.T) {
	_, m := scopedLeaseFixtureAudit(t, false)
	if _, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "x", Devices: []string{"gpu-test-0"}, TTL: time.Hour}); !errors.Is(err, gpulease.ErrCardScopedOff) {
		t.Fatalf("gpu_card_scoped_leases=true with no reader audit must not write a device lease: %v", err)
	}
}
