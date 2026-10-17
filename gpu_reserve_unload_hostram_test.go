package main

// F82: a lease that asks to have the agent seat unloaded is not held back by the host RAM that very unload
// frees.
//
// THE DEADLOCK. `gpu reserve --ram N --drain --unload-seat` admitted its host RAM inside Acquire, and the drain
// and the unload ran only after the lease was held. A film lease declared host RAM while the agent seat held 67 GiB
// of it (the 3-card vLLM seat's WSL VM), so the grant waited for commit the seat was holding and the seat was
// unloaded by the lease that was waiting to be granted: it waited about six hours. The same class as
// the DRAINING stamp (gpu_drain.go, 0.117.0): the gate at the front of the step that would clear it.
//
// THE ORDER NOW. The grant admits the cards, the drain and the unload run, and THEN the host-RAM check is put
// (gpulease.Manager.AwaitHostRAM), still holding the cards, with the same words and inside the same --wait. It is
// a reorder, not a looser check: nothing here admits more than the one rule admits.
//
// The host these tests stand in is the seat's memory made visible: the commit it reads is 95 of 100 GiB while
// the fake seat is loaded and a figure the test names once the seat is gone (the unload frees it, as the real one
// does when the VM gives the pages back).

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// useSeatBackedHost installs a 100 GiB host whose committed memory is withSeat while loaded() reports the fake
// seat resident and withoutSeat once it is not, on BOTH readings of it (the grant's and the card allocator's).
func useSeatBackedHost(t *testing.T, loaded func() bool, withSeat, withoutSeat float64) {
	t.Helper()
	read := func() (gpuprobe.HostMemory, bool) {
		c := withoutSeat
		if loaded() {
			c = withSeat
		}
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 100 - c/2, CommitUsedGiB: c, CommitLimitGiB: 100}, true
	}
	restore := gpuprobe.UseHostMemoryReader(read)
	old := hostMemoryFn
	hostMemoryFn = read
	t.Cleanup(func() { restore(); hostMemoryFn = old })
}

// shortFloor shortens the least a post-unload host-RAM wait runs for, so a test that expects the wait to end does
// not stand in line for the production floor.
func shortFloor(t *testing.T, d time.Duration) {
	t.Helper()
	old := drainFloor
	drainFloor = d
	t.Cleanup(func() { drainFloor = old })
}

// unloadSeatReserve is the wrapper form of the film lease: --ram, --drain --unload-seat, the helper that records the
// epoch it inherited (so a test can tell that the command ran, and under which lease).
func unloadSeatReserve(t *testing.T, cfgPath string, extra ...string) ([]string, string) {
	t.Helper()
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfgPath, "--reason", "film"}, extra...)
	return append(args, helperCmdFor("TestHelperRecordEpoch")...), epochFile
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// THE REPORTED SHAPE. The seat holds 95 of 100 GiB; the lease needs 30; 95 + 30 > 92, so the grant cannot be
// admitted while the seat is loaded, and only this lease's own --unload-seat takes it down. On main the reserve
// waited its --wait for the room and gave up with the seat still loaded; now it takes the cards, unloads the seat
// and, seeing 40 + 30 <= 92, runs.
func TestAnUnloadSeatLeaseIsNotHeldByTheHostRAMItsOwnUnloadFrees(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 40)
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "6s", "--drain", "--unload-seat", "--ram", "30")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("a lease that asks to unload the seat must not be held by the host RAM the unload frees: %v\nunloads=%d\n%s", err, f.unloads.Load(), stderr)
	}
	if n := f.unloads.Load(); n != 1 {
		t.Errorf("the seat must be unloaded once, unloads=%d\n%s", n, stderr)
	}
	if !fileExists(epochFile) {
		t.Errorf("the wrapped command never ran\n%s", stderr)
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the seat is warmed back once the command ends and the debt is paid: warms=%d owed=%q\n%s", n, m.SeatWarmOwed(), stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the reserve must release on exit; still held: %+v", info)
	}
}

// NOT LOOSER. The unload frees only 5 of the 95 GiB (90 still committed): 90 + 30 > 92 is the same refusal the
// grant would have given, so the lease holds the cards, waits inside its --wait, and ends with the refusal when the
// host never gives the room. The command never runs; the seat was unloaded, so its warm-back is owed (the warm is a
// load too, and 90 + 21 > 92 refuses it as well).
func TestAnUnloadSeatLeaseStillWaitsWhenTheUnloadDoesNotFreeEnough(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 90)
	shortFloor(t, 400*time.Millisecond)
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "4s", "--drain", "--unload-seat", "--ram", "30")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if n := f.unloads.Load(); n != 1 {
		t.Fatalf("the lease takes the cards and unloads the seat BEFORE it asks the host: unloads=%d err=%v\n%s", n, err, stderr)
	}
	if err == nil {
		t.Fatalf("the host still reads 90 committed: the lease must end with the host-RAM refusal\n%s", stderr)
	}
	for _, want := range []string{"waiting for host RAM: needs 30.0 GiB of commit, committed 90.0 of 100.0 GiB commit limit, 8.0 GiB headroom", "--wait"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say %q: %v", want, err)
		}
	}
	if fileExists(epochFile) {
		t.Errorf("the command ran although the host did not admit its RAM\n%s", stderr)
	}
	if n := strings.Count(stderr, "waiting for host RAM"); n < 1 {
		t.Errorf("the wait is told (once) while the cards are held:\n%s", stderr)
	}
	if owed := m.SeatWarmOwed(); owed != "seat" {
		t.Errorf("the seat was unloaded and the host refused its warm-back too: the warm stays owed, owed=%q\n%s", owed, stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("a lease that ends without running is released; still held: %+v", info)
	}
}

// A lease WITHOUT --unload-seat behaves exactly as before: the grant admits its RAM, a loaded seat's commit keeps it
// out, and nothing is drained or unloaded on its behalf. --drain alone frees no memory (it only waits for work), so it
// is the same: the seat is not cordoned for a wait it cannot shorten.
func TestALeaseWithoutUnloadSeatKeepsTheOldOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"neither flag", nil},
		{"--drain alone", []string{"--drain"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 40)
			args, epochFile := unloadSeatReserve(t, cfgPath, append([]string{"--wait", "2s", "--ram", "30"}, tc.args...)...)
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if err == nil || !strings.Contains(err.Error(), "waiting for host RAM: needs 30.0 GiB of commit, committed 95.0 of 100.0 GiB commit limit") {
				t.Fatalf("the grant admits the RAM as before and refuses it: %v\n%s", err, stderr)
			}
			if n := f.unloads.Load(); n != 0 {
				t.Errorf("nothing is unloaded on behalf of a lease that has not been granted: unloads=%d", n)
			}
			if fileExists(epochFile) {
				t.Error("the command ran although the grant was refused")
			}
			if info := m.Inspect(); info.Held {
				t.Errorf("a refused reserve holds nothing, got %+v", info)
			}
		})
	}
}

// A need no state of the box can admit (more than physical RAM less the headroom) is refused at once, as it always was,
// and BEFORE the seat is touched: draining and unloading a seat for a lease that can never run is a seat cycle bought
// for nothing.
func TestAnImpossibleNeedWithUnloadSeatIsRefusedBeforeTheSeatIsTouched(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 40)
	args, _ := unloadSeatReserve(t, cfgPath, "--wait", "5m", "--drain", "--unload-seat", "--ram", "200")
	start := time.Now()
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "can never admit") || !strings.Contains(err.Error(), "--ram") {
		t.Fatalf("want the impossible refusal naming --ram, got: %v\n%s", err, stderr)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("an impossible need waited %s", time.Since(start))
	}
	if n := f.unloads.Load(); n != 0 {
		t.Errorf("the seat was unloaded for a lease that can never run: unloads=%d", n)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("a refused reserve holds nothing, got %+v", info)
	}
}

// --detach: the hidden holder takes the cards (it never runs the comparison for a lease that unloads the seat, so it
// cannot wait for the memory the unload frees), and the parent, which runs the drain and the unload, puts the host-RAM
// check afterwards. The holder is in-process here (detachHolderFn), taking the lease with the options the parent hands it.
func TestADetachedUnloadSeatLeaseIsNotHeldByTheHostRAMItsOwnUnloadFrees(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 40)
	var held *gpulease.Lease
	oldFn := detachHolderFn
	detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		l, err := m.TryAcquire(gpulease.Class(class), opts)
		if err != nil {
			return 0, err
		}
		held = l
		return l.Epoch(), nil
	}
	t.Cleanup(func() {
		detachHolderFn = oldFn
		if held != nil {
			_ = held.Release()
		}
	})
	args := []string{"--config", cfgPath, "--detach", "--for", "1h", "--wait", "6s", "--drain", "--unload-seat", "--ram", "30", "--reason", "film"}
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("a detached lease that unloads the seat must not be held by the host RAM the unload frees: %v\nunloads=%d\n%s", err, f.unloads.Load(), stderr)
	}
	if n := f.unloads.Load(); n != 1 {
		t.Errorf("the seat must be unloaded once, unloads=%d\n%s", n, stderr)
	}
	info := m.Inspect()
	if !info.Held || info.HostRAMGiB != 30 {
		t.Errorf("once the host admitted it the lease declares its 30 GiB: %+v", info)
	}
	if owed := m.SeatWarmOwed(); owed != "seat" {
		t.Errorf("the detached form leaves the warm-back to `gpu release --warm-seat`, owed=%q", owed)
	}
}

// The card-scoped and the allocated paths. The seat is the multi-seat stand-in, and the host reads the agent seat's
// commit while it is resident.
func cardSeatHost(t *testing.T, f *multiSeatSwap, seat string, withSeat, withoutSeat float64) {
	t.Helper()
	useSeatBackedHost(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.loaded[seat]
	}, withSeat, withoutSeat)
}

func TestACardLeaseWithUnloadSeatIsNotHeldByTheHostRAMItsOwnUnloadFrees(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b")
	f.agentSeat = "agent-pool"
	srv := swapWithRoster(f, "agent-pool", "qwen3-vl-8b")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")
	cardSeatHost(t, f, "agent-pool", 95, 40)
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfgPath, "--wait", "6s", "--class", "media", "--devices", "2", "--drain", "--unload-seat", "--ram", "30", "--reason", "card 2 film"},
		helperCmdFor("TestHelperRecordEpoch")...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("a card lease that unloads the seat must not be held by the host RAM the unload frees: %v\n%s", err, stderr)
	}
	if !fileExists(epochFile) {
		t.Errorf("the wrapped command never ran\n%s", stderr)
	}
	if got := sortedCalls(f); !strings.Contains(strings.Join(got, ","), "agent-pool") {
		t.Errorf("the agent seat must have been unloaded, got %v", got)
	}
}

func TestACardsRequestWithUnloadSeatIsNotHeldByTheHostRAMItsOwnUnloadFrees(t *testing.T) {
	f := newMultiSeatSwap("agent-pool")
	f.agentSeat = "agent-pool"
	srv := swapWithRoster(f, "agent-pool")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	setConfigKey(t, cfg, "endpoint", srv.URL)
	setConfigKey(t, cfg, "agent_model", "agent-pool")
	cardSeatHost(t, f, "agent-pool", 95, 40)
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfg, "--wait", "6s", "--class", "media", "--cards", "1", "--drain", "--unload-seat", "--ram", "30", "--reason", "allocated film"},
		helperCmdFor("TestHelperRecordEpoch")...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("an allocated lease that unloads the seat must not be held by the host RAM the unload frees (the allocator's host term included): %v\n%s", err, stderr)
	}
	if !fileExists(epochFile) {
		t.Errorf("the wrapped command never ran\n%s", stderr)
	}
}

// WHEN THE ORDER APPLIES. The deferral exists for the host memory this lease's own unload frees. A lease whose unload
// reaches no model at all (the agent seat sits on cards the lease does not hold, and so does everything else the
// roster lists) frees nothing for it, so it is compared to the host in the grant, like a lease that unloads nothing:
// queued, the cards free, the seat untouched. Deferring it would take its cards, stamp them exclusive and hold them
// for the whole --wait for a shortage the unload cannot touch.
func TestACardLeaseWhoseUnloadReachesNothingIsComparedToTheHostInTheGrant(t *testing.T) {
	f := newMultiSeatSwap("gemma-4-26b-agent", "embeddinggemma")
	f.agentSeat = "gemma-4-26b-agent" // placed on card 0 by the fixture's layers; the lease holds card 2
	srv := swapWithRoster(f, "gemma-4-26b-agent", "embeddinggemma")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "gemma-4-26b-agent")
	cardSeatHost(t, f, "gemma-4-26b-agent", 95, 40) // the host is short because of something the lease does not unload
	shortFloor(t, 400*time.Millisecond)
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfgPath, "--wait", "2s", "--class", "media", "--devices", "2", "--drain", "--unload-seat", "--ram", "30", "--reason", "card 2 film"},
		helperCmdFor("TestHelperRecordEpoch")...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "waiting for host RAM: needs 30.0 GiB of commit, committed 95.0 of 100.0 GiB commit limit") {
		t.Fatalf("the grant refuses the figure, as it does for a lease that unloads nothing: %v\n%s", err, stderr)
	}
	for _, held := range []string{"neither drained nor unloaded", "lease stamped exclusive", "the cards are held"} {
		if strings.Contains(stderr, held) {
			t.Errorf("the lease took its cards although its unload reaches nothing (%q in the output):\n%s", held, stderr)
		}
	}
	if !strings.Contains(stderr, "compared in the grant") {
		t.Errorf("the reserve says why it keeps the comparison in the grant:\n%s", stderr)
	}
	if got := sortedCalls(f); len(got) != 0 {
		t.Errorf("nothing is unloaded for a lease that was never granted, got %v", got)
	}
	if fileExists(epochFile) {
		t.Error("the command ran although the grant was refused")
	}
}

// And the other half of the rule: the agent seat is on another card, but a model the roster lists IS placed on the leased
// card, and the lease's unload takes it down (unloadOthers), so the host memory that model holds is the lease's own to
// free. That lease keeps the order.
func TestACardLeaseWhoseUnloadReachesAnotherResidentStillDefers(t *testing.T) {
	f := newMultiSeatSwap("gemma-4-26b-agent", "qwen3-vl-8b", "embeddinggemma")
	f.agentSeat = "gemma-4-26b-agent"
	srv := swapWithRoster(f, "gemma-4-26b-agent", "qwen3-vl-8b", "embeddinggemma")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "gemma-4-26b-agent")
	cardSeatHost(t, f, "qwen3-vl-8b", 95, 40) // the vision seat on card 2 holds the memory
	epochFile := filepath.Join(t.TempDir(), "epoch")
	t.Setenv("LO_HELPER_EPOCH_FILE", epochFile)
	args := append([]string{"--config", cfgPath, "--wait", "6s", "--class", "media", "--devices", "2", "--drain", "--unload-seat", "--ram", "30", "--reason", "card 2 film"},
		helperCmdFor("TestHelperRecordEpoch")...)
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err != nil {
		t.Fatalf("a lease whose unload takes down the resident that holds the memory is not held by it: %v\n%s", err, stderr)
	}
	if !fileExists(epochFile) {
		t.Errorf("the wrapped command never ran\n%s", stderr)
	}
	if got := strings.Join(sortedCalls(f), ","); got != "qwen3-vl-8b" {
		t.Errorf("the lease unloads the model on its card and leaves the seat on card 0, got %q", got)
	}
}

// unloadReachesLease follows the unload's own rule of doubt (whole-node behaviour): a lease with no named cards, a roster
// that cannot be read and a config without a seat all keep the order, because the unload may reach anything.
func TestUnloadReachesLeaseFollowsTheUnloadsOwnRuleOfDoubt(t *testing.T) {
	f := newMultiSeatSwap("gemma-4-26b-agent", "embeddinggemma")
	srv := swapWithRoster(f, "gemma-4-26b-agent", "embeddinggemma")
	defer srv.Close()
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "gemma-4-26b-agent")
	cfg := loadCfgPath(cfgPath)
	card2 := []string{"gpu-cccc0000-x"}
	ctx := context.Background()

	if unloadReachesLease(ctx, cfg, card2, nil) {
		t.Error("the seat is on card 0 and nothing else is listed for card 2: the unload reaches nothing")
	}
	if !unloadReachesLease(ctx, cfg, []string{"gpu-aaaa0000-x"}, nil) {
		t.Error("the seat is on card 0: a lease on card 0 reaches it")
	}
	if !unloadReachesLease(ctx, cfg, nil, nil) {
		t.Error("a lease that names no card is the whole node (or an allocation not made yet): the unload reaches the seat")
	}
	old := rosterIDsFn
	rosterIDsFn = func(context.Context, string) ([]string, error) { return nil, errors.New("roster down") }
	t.Cleanup(func() { rosterIDsFn = old })
	if !unloadReachesLease(ctx, cfg, card2, nil) {
		t.Error("a roster that cannot be read says nothing about what is resident: the doubt keeps the order")
	}
	rosterIDsFn = old
	noSeat := cfg
	noSeat.Endpoint = ""
	if !unloadReachesLease(ctx, noSeat, card2, nil) {
		t.Error("a config without an endpoint makes the unload refuse at once; nothing waits, so the order is left as it was")
	}
}

// useUnreadableHost installs a host whose memory counters cannot be read, on both readings of it.
func useUnreadableHost(t *testing.T) {
	t.Helper()
	read := func() (gpuprobe.HostMemory, bool) { return gpuprobe.HostMemory{}, false }
	restore := gpuprobe.UseHostMemoryReader(read)
	old := hostMemoryFn
	hostMemoryFn = read
	t.Cleanup(func() { restore(); hostMemoryFn = old })
}

// A host whose memory cannot be read cannot show that the unload makes room (the look after the unload would refuse for the
// same reason until its deadline), so the seat is not cycled for it: the figure is compared in the grant, which waits for a
// readable host exactly as it did before F82, with the seat untouched and the cards free.
func TestAnUnreadableHostKeepsTheGrantsComparisonForALeaseThatUnloadsTheSeat(t *testing.T) {
	if !gpuprobe.HostMemorySupported {
		t.Skip("this platform has no host memory reader, so no reading is ever refused as unreadable")
	}
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useUnreadableHost(t)
	shortFloor(t, 400*time.Millisecond)
	args, epochFile := unloadSeatReserve(t, cfgPath, "--wait", "2s", "--drain", "--unload-seat", "--ram", "30")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "memory cannot be read") {
		t.Fatalf("the grant refuses a figure it cannot show to fit: %v\n%s", err, stderr)
	}
	if n := f.unloads.Load(); n != 0 {
		t.Errorf("the seat was cycled for a lease the host could not be asked about: unloads=%d\n%s", n, stderr)
	}
	if fileExists(epochFile) {
		t.Error("the command ran although the grant was refused")
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("a refused reserve holds nothing, got %+v", info)
	}
}
