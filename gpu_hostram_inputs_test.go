package main

// The inputs of the host-memory declaration on the `gpu reserve` path, pinned one by one: a commit below the working set
// (mapped files count in the working set and not in the commit) travels from the flag to the grant, to the detached holder and
// to the deferred pre-screen as stated.

import (
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// 60 GiB touched of 90 available, 5 GiB committed on 140 of a 152 bound fit both conditions; the commit read as 60 (the larger
// figure) would be refused. The lease records what was stated.
func TestReserveTakesACommitBelowTheWorkingSetAsStated(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostMemAs(t, gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 90, CommitUsedGiB: 140, CommitLimitGiB: 160})
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	flags := []string{"--class", "media", "--devices", "2", "--wait", "0", "--reason", "mapped", "--ram", "60", "--ram-commit", "5"}
	done := make(chan error, 1)
	go func() { done <- runGPUReserve(reserveWithCmd(cfg, flags)) }()
	leases := waitForLeases(t, m, 1)
	if leases[0].HostRAMGiB != 60 || leases[0].HostRAMCommitGiB != 5 {
		t.Errorf("the lease declares touched %.1f commit %.1f, want 60 and 5", leases[0].HostRAMGiB, leases[0].HostRAMCommitGiB)
	}
	if err := <-done; err != nil {
		t.Fatalf("it fits both conditions and runs: %v", err)
	}
}

// The detached holder is told a commit that is smaller than the working set too: left out of its arguments, it would declare
// the working set for both quantities.
func TestHoldArgsCarryACommitBelowTheWorkingSet(t *testing.T) {
	args := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 60, HostRAMCommitGiB: 5}, nil, "")
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--ram-commit" && args[i+1] == "5" {
			return
		}
	}
	t.Errorf("a declared commit of 5 beside --ram 60 must travel to the holder: %q", args)
}

// A commit no state of the box admits is refused before the seat is touched, as a working set that can never fit always was:
// 200 GiB of commit can never fit a 160 GiB limit, so the seat is not unloaded, nothing runs and nothing is held.
func TestAnImpossibleCommitIsRefusedBeforeTheSeatIsUnloaded(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	mem := gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 40, CommitLimitGiB: 160}
	useSeatBackedMem(t, f.loaded.Load, mem, mem)
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "4s", "--drain", "--unload-seat", "--ram", "10", "--ram-commit", "200")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) || !short.Impossible || !short.CommitShort {
		t.Fatalf("200 GiB of commit can never fit a 160 GiB limit: %v\n%s", err, stderr)
	}
	if n := f.unloads.Load(); n != 0 {
		t.Errorf("the seat was unloaded %d time(s) for a lease that could never be admitted\n%s", n, stderr)
	}
	if fileExists(epochFile) || m.Inspect().Held {
		t.Errorf("nothing ran and nothing is held")
	}
}
