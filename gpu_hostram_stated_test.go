package main

// `gpu reserve --ram N` alone states no commit, so it is held to the line it always was (committed + N against physical RAM less
// the headroom); `--ram N --ram-commit N` states one, even when it equals the working set, and is held to the commit limit.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A 100 GiB box with a 160 GiB commit limit, 70 GiB committed and 80 available: 30 GiB declared once is over the 92 GiB line
// (70 + 30) and well inside the 152 GiB the commit limit leaves.
var singleFigureHost = gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 70, CommitLimitGiB: 160}

func TestReserveWithRamAloneIsHeldToPhysicalRAM(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostMemAs(t, singleFigureHost)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--ram", "30"}))
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) {
		t.Fatalf("--ram 30 states no commit: want the host-RAM refusal on the physical line, got %T: %v", err, err)
	}
	if !short.CommitShort || short.TouchShort || !strings.Contains(err.Error(), "committed 70.0 of 100.0 GiB physical") {
		t.Fatalf("the refusal is on the commit charge against physical RAM: commitShort=%v touchShort=%v (%v)", short.CommitShort, short.TouchShort, err)
	}
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("a refused reserve holds nothing, got %+v", l)
	}
}

func TestReserveWithTheCommitStatedEqualToRamIsHeldToTheCommitLimit(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostMemAs(t, singleFigureHost)
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	flags := []string{"--class", "media", "--devices", "2", "--wait", "0", "--reason", "stated", "--ram", "30", "--ram-commit", "30"}
	done := make(chan error, 1)
	go func() { done <- runGPUReserve(reserveWithCmd(cfg, flags)) }()
	leases := waitForLeases(t, m, 1)
	if leases[0].HostRAMGiB != 30 || leases[0].HostRAMCommitGiB != 30 {
		t.Errorf("the lease declares touched %.1f commit %.1f, want 30 and 30", leases[0].HostRAMGiB, leases[0].HostRAMCommitGiB)
	}
	if err := <-done; err != nil {
		t.Fatalf("a stated commit of 30 fits the commit limit (70 + 30 <= 152): %v", err)
	}
}

// A commit equal to the working set still travels to the detached holder: left off, the holder would read it as unstated.
func TestHoldArgsCarryAStatedCommitEqualToTheTouchedFigure(t *testing.T) {
	has := func(args []string, a, b string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == a && args[i+1] == b {
				return true
			}
		}
		return false
	}
	named := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 30, HostRAMCommitGiB: 30}, nil, "")
	if !has(named, "--ram-commit", "30") {
		t.Errorf("a stated commit equal to --ram travels: %q", named)
	}
	deferred := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMWaitGiB: 30, HostRAMWaitCommitGiB: 30}, nil, "")
	if !has(deferred, "--ram-wait-commit", "30") {
		t.Errorf("a stated deferred commit equal to --ram-wait travels: %q", deferred)
	}
	auto := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMGiB: 30, HostRAMCommitGiB: 30}, &reserveDeviceFlags{cards: "1", vramGiB: 4, ramGiB: 30, ramCommitGiB: 30}, "")
	if !has(auto, "--ram-commit", "30") {
		t.Errorf("an allocated request carries a stated commit equal to --ram: %q", auto)
	}
}
