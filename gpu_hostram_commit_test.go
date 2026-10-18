package main

// `gpu reserve --ram <GiB> --ram-commit <GiB>`: the two quantities of the host-RAM declaration (internal/gpulease/hostram.go,
// internal/gpuprobe/hostmemory.go). --ram is the working set the job touches (admitted against the memory that is available),
// --ram-commit the commit it is charged (admitted against the OS commit limit); unset, the commit is the --ram figure.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// hostMemAs installs this reading for the length of the test.
func hostMemAs(t *testing.T, mem gpuprobe.HostMemory) {
	t.Helper()
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return mem, true })
	t.Cleanup(restore)
}

// A 100 GiB box with a 160 GiB commit limit: 80 GiB available, 130 GiB committed. 10 GiB touched fits what is available; 60 GiB
// of commit does not fit the commit limit less the 8 GiB headroom (130 + 60 > 152).
var commitShortHost = gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 130, CommitLimitGiB: 160}
var roomyForCommit = gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 40, CommitLimitGiB: 160}

func TestReserveRefusesOnTheCommitAlthoughTheTouchedSetFits(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostMemAs(t, commitShortHost)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--ram", "10", "--ram-commit", "60"}))
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) {
		t.Fatalf("want the host-RAM refusal, got %T: %v", err, err)
	}
	if !short.CommitShort || short.TouchShort {
		t.Fatalf("the refusal is on the commit alone: commitShort=%v touchShort=%v (%v)", short.CommitShort, short.TouchShort, err)
	}
	for _, want := range []string{"waiting for host RAM: needs 60.0 GiB of commit, committed 130.0 of 160.0 GiB commit limit, 8.0 GiB headroom", "--wait", "--ram-commit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must contain %q: %v", want, err)
		}
	}
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("a refused reserve holds nothing, got %+v", l)
	}
}

// The lease record carries both declarations, and a commit left unset is the --ram figure.
func TestReserveDeclaresBothFigures(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		flags                   []string
		wantTouched, wantCommit float64
	}{
		{"both given", []string{"--ram", "10", "--ram-commit", "60"}, 10, 60},
		{"commit left unset", []string{"--ram", "12"}, 12, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			hostMemAs(t, roomyForCommit)
			t.Setenv("LO_HELPER_SLEEP_MS", "1500")
			t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
			flags := append([]string{"--class", "media", "--devices", "2", "--wait", "0", "--reason", "coder arm"}, tc.flags...)
			done := make(chan error, 1)
			go func() { done <- runGPUReserve(reserveWithCmd(cfg, flags)) }()
			leases := waitForLeases(t, m, 1)
			if leases[0].HostRAMGiB != tc.wantTouched || leases[0].HostRAMCommitGiB != tc.wantCommit {
				t.Errorf("the lease declares touched %.1f commit %.1f, want %.1f and %.1f", leases[0].HostRAMGiB, leases[0].HostRAMCommitGiB, tc.wantTouched, tc.wantCommit)
			}
			if err := <-done; err != nil {
				t.Fatalf("it fits and runs: %v", err)
			}
		})
	}
}

// A commit is declared beside the working set it goes with: it needs a --ram (given or estimated) above 0, and is itself above 0.
func TestReserveRejectsAnIncoherentCommit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"commit with no host RAM declared", []string{"--ram", "0", "--ram-commit", "20"}, "--ram-commit"},
		{"a zero commit", []string{"--ram", "10", "--ram-commit", "0"}, "--ram-commit"},
		{"a negative commit", []string{"--ram", "10", "--ram-commit", "-1"}, "--ram-commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			hostMemAs(t, roomyForCommit)
			flags := append([]string{"--class", "media", "--devices", "2", "--wait", "0"}, tc.flags...)
			err := runGPUReserve(reserveWithCmd(cfg, flags))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %s, got %v", tc.want, err)
			}
			if l := m.Leases(); len(l) != 0 {
				t.Fatalf("an incoherent declaration holds nothing, got %+v", l)
			}
		})
	}
}

// A commit no state of the box admits is refused at once, before anything is taken.
func TestReserveRefusesAnImpossibleCommitAtOnce(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostMemAs(t, roomyForCommit)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--ram", "10", "--ram-commit", "200"}))
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) || !short.Impossible || !short.CommitShort {
		t.Fatalf("200 GiB of commit can never fit a 160 GiB limit: %v", err)
	}
	if !strings.Contains(err.Error(), "--ram-commit") || len(m.Leases()) != 0 {
		t.Fatalf("it says what to change and holds nothing: %v %+v", err, m.Leases())
	}
}

// The detached holder is told both declarations, and the figures it waits to have admitted.
func TestHoldArgsCarryTheCommitDeclarations(t *testing.T) {
	has := func(args []string, seq ...string) bool {
		for i := 0; i+len(seq) <= len(args); i++ {
			ok := true
			for j := range seq {
				if args[i+j] != seq[j] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	named := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 30, HostRAMCommitGiB: 60}, nil, "")
	if !has(named, "--ram", "30", "--ram-commit", "60") {
		t.Errorf("a declared commit travels beside --ram: %q", named)
	}
	deferred := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMWaitGiB: 30, HostRAMWaitCommitGiB: 60}, nil, "")
	if !has(deferred, "--ram-wait", "30", "--ram-wait-commit", "60") {
		t.Errorf("a deferred commit travels beside --ram-wait: %q", deferred)
	}
	auto := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMGiB: 30, HostRAMCommitGiB: 60}, &reserveDeviceFlags{cards: "1", vramGiB: 4, ramGiB: 30, ramCommitGiB: 60}, "")
	if !has(auto, "--ram", "30", "--ram-commit", "60") {
		t.Errorf("an allocated request carries the commit in its own flags: %q", auto)
	}
	plain := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 30}, nil, "")
	for _, a := range plain {
		if a == "--ram-commit" || a == "--ram-wait-commit" {
			t.Errorf("a lease that declared one figure passes no commit flag: %q", plain)
		}
	}
}

// The hidden holder takes the lease with the declarations it was passed.
func TestTheHolderTakesTheLeaseWithBothDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts gpulease.Options
		want func(gpulease.Info) bool
	}{
		{"declared", gpulease.Options{HostRAMGiB: 10, HostRAMCommitGiB: 60}, func(i gpulease.Info) bool { return i.HostRAMGiB == 10 && i.HostRAMCommitGiB == 60 }},
		{"deferred", gpulease.Options{HostRAMWaitGiB: 10, HostRAMWaitCommitGiB: 60}, func(i gpulease.Info) bool {
			return i.HostRAMGiB == 0 && i.HostRAMWaitGiB == 10 && i.HostRAMWaitCommitGiB == 60
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			hostMemAs(t, roomyForCommit)
			opts := tc.opts
			opts.Reason, opts.Devices = "coder arm", []string{"gpu-aaaa0000-x"}
			args := holdArgs("media", 3*time.Second, 0, opts, nil, cfg)
			done := make(chan error, 1)
			go func() { done <- runGPUHold(args[2:]) }()
			ls := waitForLeases(t, m, 1)
			if !tc.want(ls[0]) {
				t.Errorf("the holder's lease did not take the declarations: %+v", ls[0])
			}
			releaseAll(t, m)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The split of a deferred reserve moves both quantities together.
func TestHostRAMLoadSplitMovesBothQuantities(t *testing.T) {
	load := gpuprobe.HostLoad{TouchGiB: 28, CommitGiB: 58, CommitStated: true} // the flag is part of the figure: a split that rebuilds the struct without it must not pass
	gate, later := hostRAMLoadSplit(false, load)
	if gate != load || later != (gpuprobe.HostLoad{}) {
		t.Errorf("a lease that unloads nothing compares both in the grant: gate %+v later %+v", gate, later)
	}
	gate, later = hostRAMLoadSplit(true, load)
	if gate != (gpuprobe.HostLoad{}) || later != load {
		t.Errorf("a lease that unloads the seat asks the host for both after it: gate %+v later %+v", gate, later)
	}
	gate, later = hostRAMLoadSplit(true, gpuprobe.HostLoad{})
	if gate != (gpuprobe.HostLoad{}) || later != (gpuprobe.HostLoad{}) {
		t.Errorf("nothing declared, nothing to ask: gate %+v later %+v", gate, later)
	}
}

// `gpu status` shows both declarations of a live lease, in the text and in --json.
func TestGPUStatusShowsBothDeclarations(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	hostMemAs(t, roomyForCommit)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "coder arm", TTL: time.Hour, Devices: []string{"gpu-cccc0000-x"}, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	doc := statusJSON(t, cfg)
	leases, _ := doc["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("one lease: %v", doc["leases"])
	}
	row := leases[0].(map[string]any)
	if row["host_ram_gib"] != 10.0 || row["host_ram_commit_gib"] != 60.0 {
		t.Errorf("the lease row carries both declarations: %v", row)
	}
	hm, _ := doc["host_memory"].(map[string]any)
	for k, want := range map[string]any{"declared_live_gib": 10.0, "declared_live_commit_gib": 60.0, "pending_gib": 10.0, "pending_commit_gib": 60.0, "commit_limit_enforced": true} {
		if hm[k] != want {
			t.Errorf("host_memory.%s = %v, want %v", k, hm[k], want)
		}
	}
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"host RAM 10.0 GiB touched, 60.0 GiB commit", "live leases declared 10.0 GiB touched, 60.0 GiB commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the text status lacks %q:\n%s", want, out)
		}
	}
	if _, err := json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
}

// useSeatBackedMem installs a host whose reading is withSeat while loaded() reports the fake seat resident and withoutSeat
// once it is not, on both readings of it (the grant's and the card allocator's).
func useSeatBackedMem(t *testing.T, loaded func() bool, withSeat, withoutSeat gpuprobe.HostMemory) {
	t.Helper()
	read := func() (gpuprobe.HostMemory, bool) {
		if loaded() {
			return withSeat, true
		}
		return withoutSeat, true
	}
	restore := gpuprobe.UseHostMemoryReader(read)
	old := hostMemoryFn
	hostMemoryFn = read
	t.Cleanup(func() { restore(); hostMemoryFn = old })
}

// THE DEFERRED LEASE CARRIES ITS COMMIT. The seat's weights hold 140 GiB of a 160 GiB commit limit; the lease touches 10 GiB
// and is charged 30. 140 + 30 is over the bound of 152, so the grant would refuse it, and only its own --unload-seat frees
// the commit: it takes the cards, unloads the seat, and then, seeing 40 + 30 <= 152, runs.
func TestAnUnloadSeatLeaseIsNotHeldByTheCommitItsOwnUnloadFrees(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedMem(t, f.loaded.Load,
		gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 140, CommitLimitGiB: 160},
		gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 40, CommitLimitGiB: 160})
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "6s", "--drain", "--unload-seat", "--ram", "10", "--ram-commit", "30")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("a lease that unloads the seat is not held by the commit the unload frees: %v\nunloads=%d\n%s", err, f.unloads.Load(), stderr)
	}
	if n := f.unloads.Load(); n != 1 || !fileExists(epochFile) {
		t.Errorf("the seat is unloaded once and the command runs: unloads=%d ran=%v\n%s", n, fileExists(epochFile), stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the reserve must release on exit; still held: %+v", info)
	}
}

// NOT LOOSER, on commit. The unload frees only 10 GiB of commit (130 stay): 130 + 30 > 152, while the touched set alone (10 of
// 80 available) and the commit taken as the touched figure (130 + 10) would both fit. The wait after the unload ends with
// the commit sentence, and the command never runs: a build that dropped the commit between the verb and the await would run it.
func TestAnUnloadSeatLeaseStillWaitsOnItsCommitWhenTheUnloadDoesNotFreeEnough(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedMem(t, f.loaded.Load,
		gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 140, CommitLimitGiB: 160},
		gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 80, CommitUsedGiB: 130, CommitLimitGiB: 160})
	shortFloor(t, 400*time.Millisecond)
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "4s", "--drain", "--unload-seat", "--ram", "10", "--ram-commit", "30")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if n := f.unloads.Load(); n != 1 {
		t.Fatalf("the lease takes the cards and unloads the seat BEFORE it asks the host: unloads=%d err=%v\n%s", n, err, stderr)
	}
	if err == nil {
		t.Fatalf("the host still reads 130 committed: the lease must end with the commit refusal\n%s", stderr)
	}
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) || !short.CommitShort || short.TouchShort {
		t.Fatalf("the refusal is on the commit alone: %v", err)
	}
	if !strings.Contains(err.Error(), "needs 30.0 GiB of commit, committed 130.0 of 160.0 GiB commit limit, 8.0 GiB headroom") {
		t.Errorf("the refusal carries the commit declared on the command line: %v", err)
	}
	if fileExists(epochFile) {
		t.Errorf("the command ran although the host did not admit its commit\n%s", stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the lease is released: %+v", info)
	}
}
