package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// TestHelperWriteLeaseEnv is a helper process: when asked, it writes the lease
// environment it was handed to a file, then stays alive for LO_HELPER_SLEEP_MS.
// The stay does not depend on the file: a test that only watches the lease from
// outside sets the sleep alone, and a helper that returned at once held the lease
// for the few milliseconds a process takes to start, which a 40 ms poll caught on
// a slow-spawning host and missed on a fast one (the Linux CI runner, every run).
func TestHelperWriteLeaseEnv(t *testing.T) {
	if out := os.Getenv("LO_HELPER_ENV_OUT"); out != "" {
		body := "devices=" + os.Getenv("GPU_LEASE_DEVICES") + "\nepoch=" + os.Getenv("GPU_LEASE_EPOCH") + "\n" +
			"cuda_visible=" + os.Getenv("CUDA_VISIBLE_DEVICES") + "\ncuda_order=" + os.Getenv("CUDA_DEVICE_ORDER") + "\n"
		_ = os.WriteFile(out, []byte(body), 0o644)
	}
	if ms := atoiOr(os.Getenv("LO_HELPER_SLEEP_MS")); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

func atoiOr(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// useCardTable swaps the live reads for a synthetic host: three cards (nvidia index 1 is
// the display card), no foreign processes, no resident seats, plenty of host RAM.
func useCardTable(t *testing.T, order string) {
	t.Helper()
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 16, FreeGiB: 16, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}
	oldCards, oldF, oldR, oldH := cardTableFn, foreignBusyFn, residentSeatsFn, hostMemoryFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		cards, warn := gpuprobe.BuildCards(devs, order)
		return cards, warn, nil
	}
	foreignBusyFn = func(context.Context, config.Config) map[string]string { return nil }
	residentSeatsFn = func(context.Context, config.Config, []gpuprobe.Card) map[string]gpulease.ResidentInfo { return nil }
	hostMemoryFn = func() (gpuprobe.HostMemory, bool) { return roomyTestHost, true }
	t.Cleanup(func() { cardTableFn, foreignBusyFn, residentSeatsFn, hostMemoryFn = oldCards, oldF, oldR, oldH })
}

func waitForLeases(t *testing.T, m *gpulease.Manager, n int) []gpulease.Info {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if l := m.Leases(); len(l) >= n {
			return l
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("never saw %d live leases (have %v)", n, m.Leases())
	return nil
}

func envHelperCmd(out string) []string {
	return []string{"--", os.Args[0], "-test.run=TestHelperWriteLeaseEnv", "-test.timeout=30s"}
}

// TWO RESERVATIONS, TWO CARDS, AT ONCE: the operator's order. Each holds its own card;
// neither fences the other, and the wrapped command is told which cards its lease holds.
func TestGPUReserveDevicesRunsTwoDisjointLeasesAtOnce(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "2500")

	done := make(chan error, 2)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", "0", "--reason", "card two"}, envHelperCmd(out)...))
	}()
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--devices", "GPU-aaaa", "--wait", "0", "--reason", "card zero"}, envHelperCmd(out)...))
	}()
	leases := waitForLeases(t, m, 2)
	got := map[string]bool{}
	for _, l := range leases {
		if len(l.Devices) != 1 {
			t.Fatalf("each lease holds exactly one card: %+v", l)
		}
		got[l.Devices[0]] = true
	}
	if !got["gpu-cccc0000-x"] || !got["gpu-aaaa0000-x"] {
		t.Fatalf("the two leases must hold cards 2 and 0, got %v", got)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "devices=gpu-") {
		t.Fatalf("the wrapped command must be handed GPU_LEASE_DEVICES: %q %v", b, err)
	}
}

// --devices naming a card that another lease holds QUEUES (FIFO) rather than failing.
func TestGPUReserveDevicesQueuesBehindTheSameCard(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "holder", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	go func() { time.Sleep(700 * time.Millisecond); _ = holder.Release() }()
	start := time.Now()
	args := append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", "20s", "--reason", "behind"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("a busy card is a place in line: %v", err)
	}
	if el := time.Since(start); el < 500*time.Millisecond {
		t.Errorf("ran after %s, before the holder released", el)
	}
}

func TestGPUReserveDevicesIsARefusalOnAFlagOffHost(t *testing.T) {
	cfg, _ := leaseFixture(t)
	useCardTable(t, "")
	err := runGPUReserve(append([]string{"--config", cfg, "--devices", "0", "--wait", "0"}, helperCmd()...))
	if !errors.Is(err, gpulease.ErrCardScopedOff) {
		t.Fatalf("want ErrCardScopedOff, got %v", err)
	}
}

// Flag off = no change: the command says --cuda-device, and the lease is still the
// whole node, with no card table read.
func TestGPUReserveFlagOffKeepsTheWholeNodeLeaseWhateverTheCommandSays(t *testing.T) {
	cfg, m := leaseFixture(t)
	oldCards := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		t.Error("a flag-off host must not read the card table")
		return nil, "", errors.New("must not be called")
	}
	t.Cleanup(func() { cardTableFn = oldCards })
	t.Setenv("COMFY_CUDA_DEVICE", "1")
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--wait", "0"}, helperCmd()...))
	}()
	l := waitForLeases(t, m, 1)
	if len(l[0].Devices) != 0 {
		t.Fatalf("a flag-off host holds the whole node: %+v", l[0])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// On an enabled host the wrapped command's own pin becomes the lease's card set.
func TestGPUReserveDerivesTheCardFromTheWrappedCommand(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "1,0,2") // ComfyUI order: nvidia 1, nvidia 0, nvidia 2
	t.Setenv("COMFY_CUDA_DEVICE", "2")
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--wait", "0"}, helperCmd()...))
	}()
	l := waitForLeases(t, m, 1)
	if len(l[0].Devices) != 1 || l[0].Devices[0] != "gpu-cccc0000-x" {
		t.Fatalf("COMFY_CUDA_DEVICE=2 in ComfyUI order is nvidia index 2: %+v", l[0])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// --cards N: the allocator picks free cards and never the display card.
func TestGPUReserveCardsTakesFreeNonDisplayCards(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--cards", "2", "--wait", "0"}, helperCmd()...))
	}()
	l := waitForLeases(t, m, 1)
	got := strings.Join(l[0].Devices, ",")
	if got != "gpu-aaaa0000-x,gpu-cccc0000-x" {
		t.Fatalf("two cards, never the display card: %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// pickAutoCards: queue on claimed cards, wait for a short supply, refuse only at the end.
func TestResolveAutoCardsQueuesOnClaimedCards(t *testing.T) {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	build := func() (gpulease.AllocInput, error) {
		return gpulease.AllocInput{Cards: cards, Claimed: map[string]bool{"gpu-aaaa0000-x": true, "gpu-cccc0000-x": true},
			HostMemOK: true, HostMem: roomyTestHost}, nil
	}
	var out bytes.Buffer
	ids, free, err := pickAutoCards(devicePlan{Auto: true, Min: 1, Max: 2}, time.Minute, build, &out, func(time.Duration) { t.Error("must queue, not poll") }, time.Now)
	if err != nil || len(ids) != 1 || free {
		t.Fatalf("both cards claimed: queue on one of them, FIFO (not a free set): %v free=%v %v", ids, free, err)
	}
	if !strings.Contains(out.String(), "queueing") {
		t.Errorf("say so: %q", out.String())
	}
}

func TestResolveAutoCardsWaitsForAShortSupplyThenRefusesAtTheDeadline(t *testing.T) {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	polls := 0
	build := func() (gpulease.AllocInput, error) {
		polls++
		// Host RAM is short for the first two reads (committed 99 of 100 GiB), then recovers.
		host := gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 1, CommitUsedGiB: 99, CommitLimitGiB: 160}
		if polls >= 3 {
			host = roomyTestHost
		}
		return gpulease.AllocInput{Cards: cards, HostMemOK: true, HostMem: host, HostNeedGiB: 8, HostHeadroomGiB: 4}, nil
	}
	clock := time.Unix(1000, 0)
	now := func() time.Time { return clock }
	sleep := func(d time.Duration) { clock = clock.Add(d) }
	var out bytes.Buffer
	ids, free, err := pickAutoCards(devicePlan{Auto: true, Min: 1, Max: 1}, time.Hour, build, &out, sleep, now)
	if err != nil || len(ids) != 1 || !free || polls < 3 {
		t.Fatalf("host RAM recovers on the third read, and the card is free to claim: %v free=%v %v polls=%d", ids, free, err, polls)
	}
	// Never enough: it waits the whole budget, then says why and what to change.
	polls = -1000
	_, _, err = pickAutoCards(devicePlan{Auto: true, Min: 1, Max: 1}, 10*time.Second, build, &out, sleep, now)
	var none *gpulease.NoCardsError
	if !errors.As(err, &none) || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("at the deadline: the reasons and the flag: %v", err)
	}
	// --wait 0 fails fast with the same reasons.
	if _, _, err = pickAutoCards(devicePlan{Auto: true, Min: 1, Max: 1}, 0, build, &out, sleep, now); err == nil {
		t.Fatal("--wait 0 must refuse at once when nothing qualifies")
	}
}
