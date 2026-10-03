package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestHoldChildCarriesCardsAndOwner: the real runGPUHold, fed the argv holdArgs builds from
// options that carry BOTH the card flags and the owner/contract flags, takes a lease that
// carries both (the P8 x P4 merge gave the hidden holder one argv builder).
func TestHoldChildCarriesCardsAndOwner(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "work", "log.jsonl")
	pid := os.Getpid()
	start, _ := gpulease.ProcessStart(pid)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-child-env")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	base := gpulease.Options{
		Reason: "film", Origin: "me", Group: "grp-1", Exclusive: false,
		Owner:      gpulease.Owner{Session: "sess-parent", PID: pid, StartMs: start},
		Unattended: true, ProgressFile: prog, Stall: 2 * time.Hour, YieldGrace: 5 * time.Minute, OnYield: "touch STOP",
	}
	assert := func(t *testing.T, got gpulease.Info, wantDevs int) {
		t.Helper()
		if got.Legacy {
			t.Errorf("hold child's record reads Legacy")
		}
		if len(got.Devices) != wantDevs {
			t.Errorf("devices = %v, want %d", got.Devices, wantDevs)
		}
		if got.Group != "grp-1" {
			t.Errorf("group lost: %q", got.Group)
		}
		if got.Owner == nil || got.Owner.Session != "sess-parent" || got.Owner.PID != pid || got.Owner.StartMs != start {
			t.Errorf("owner lost or replaced by the child's env: %+v", got.Owner)
		}
		if !got.Unattended || got.Progress == nil || got.Progress.File != prog || got.Progress.StallMs != (2*time.Hour).Milliseconds() {
			t.Errorf("contract lost: unattended=%v progress=%+v", got.Unattended, got.Progress)
		}
		if got.YieldGraceMs != (5*time.Minute).Milliseconds() || got.OnYield != "touch STOP" {
			t.Errorf("yield terms lost: grace=%d on=%q", got.YieldGraceMs, got.OnYield)
		}
		if got.WrapperVersion == "" {
			t.Errorf("wrapper version not stamped")
		}
	}

	t.Run("named devices", func(t *testing.T) {
		cfg, m := scopedLeaseFixture(t)
		useCardTable(t, "")
		o := base
		o.Devices = []string{"gpu-aaaa0000-x"}
		args := holdArgs("media", 3*time.Second, 0, o, nil, cfg)
		t.Logf("argv: %q", args)
		done := make(chan error, 1)
		go func() { done <- runGPUHold(args[2:]) }()
		ls := waitForLeases(t, m, 1)
		assert(t, ls[0], 1)
		releaseAll(t, m) // a holder no longer releases at its deadline (plan P9); a release is what ends it
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("allocated cards", func(t *testing.T) {
		cfg, m := scopedLeaseFixture(t)
		useCardTable(t, "")
		args := holdArgs("media", 3*time.Second, 0, base, &reserveDeviceFlags{cards: "1", vramGiB: 4, ramGiB: 8}, cfg)
		t.Logf("argv: %q", args)
		done := make(chan error, 1)
		go func() { done <- runGPUHold(args[2:]) }()
		ls := waitForLeases(t, m, 1)
		assert(t, ls[0], 1)
		releaseAll(t, m) // a holder no longer releases at its deadline (plan P9); a release is what ends it
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("whole node (no card flags)", func(t *testing.T) {
		cfg, m := scopedLeaseFixture(t)
		args := holdArgs("media", 3*time.Second, 0, base, nil, cfg)
		done := make(chan error, 1)
		go func() { done <- runGPUHold(args[2:]) }()
		ls := waitForLeases(t, m, 1)
		assert(t, ls[0], 0)
		releaseAll(t, m)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
