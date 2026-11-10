package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// useCannedDisplayTable swaps the live reads for the reference three-card box: the display card (nvidia
// index 1) holds 2.6 GiB of 15.9, the pair seat is loaded on cards 0 and 2, and the desk reads idle. The
// display card sorts after the other two whatever they cost (operator rule 2026-11-07).
func useCannedDisplayTable(t *testing.T) {
	t.Helper()
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 15.9, FreeGiB: 15.9, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 15.9, FreeGiB: 13.3, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 15.9, FreeGiB: 15.9, UtilKnown: true},
	}
	oldCards, oldF, oldR, oldH, oldB := cardTableFn, foreignBusyFn, residentSeatsFn, hostMemoryFn, displayBusyFn
	displayBusyFn = func(config.Config, []gpuprobe.Card) string { return "" }
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		cards, warn := gpuprobe.BuildCards(devs, "")
		return cards, warn, nil
	}
	foreignBusyFn = func(context.Context, config.Config) map[string]string { return nil }
	residentSeatsFn = func(context.Context, config.Config, []gpuprobe.Card) map[string]gpulease.ResidentInfo {
		seat := gpulease.ResidentInfo{Seats: []string{"agent-pool"}, CostGiB: 15}
		return map[string]gpulease.ResidentInfo{"gpu-aaaa0000-x": seat, "gpu-cccc0000-x": seat}
	}
	hostMemoryFn = func() (gpuprobe.HostMemory, bool) { return roomyTestHost, true }
	t.Cleanup(func() {
		cardTableFn, foreignBusyFn, residentSeatsFn, hostMemoryFn, displayBusyFn = oldCards, oldF, oldR, oldH, oldB
	})
}

// The reserve verb's door onto the display card (operator rule 2026-11-07): `--cards 2` takes the two cards that
// are not the screen's; `--cards N --vram X` takes the screen's card only while it is idle and the VRAM in use plus
// X stays under gpu_display_cap_pct (60 percent of 15.9 GiB = 9.54, with 2.6 in use: up to 6.94 GiB), and a request
// over it, one that declares no --vram, a cap of 0, or the operator at the desk does not.
func TestGPUReserveCardsTakesTheDisplayCardWithinTheCapAndNotPastIt(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	useCannedDisplayTable(t)
	zero := 0
	pickRange := func(cfg config.Config, vram float64, lo, hi int) ([]string, error) {
		build := func() (gpulease.AllocInput, error) {
			return buildAllocInput(context.Background(), m, cfg, reserveDeviceFlags{vramGiB: vram})
		}
		var out bytes.Buffer
		ids, _, err := pickAutoCards(devicePlan{Auto: true, Min: lo, Max: hi}, 0, build, &out, func(time.Duration) {}, time.Now)
		return ids, err
	}
	pick := func(cfg config.Config, vram float64, n int) ([]string, error) { return pickRange(cfg, vram, n, n) }
	if ids, err := pick(config.Config{}, 6, 2); err != nil || strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") || len(ids) != 2 {
		t.Fatalf("a two-card job takes the two cards that are not the screen's, got %v %v", ids, err)
	}
	if ids, err := pick(config.Config{}, 6, 3); err != nil || !strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") {
		t.Fatalf("6 GiB on 2.6 in use of 15.9 is under the 60 percent cap and the desk is idle: the display card is allocatable, got %v %v", ids, err)
	}
	displayBusyFn = func(config.Config, []gpuprobe.Card) string { return "the operator is at the desk" }
	if ids, err := pickRange(config.Config{}, 6, 1, 3); err != nil || strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") {
		t.Fatalf("the operator at the desk closes the display card whatever the cap, got %v %v", ids, err)
	}
	displayBusyFn = func(config.Config, []gpuprobe.Card) string { return "" }
	if ids, err := pickRange(config.Config{}, 7.5, 1, 3); err != nil || strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") {
		t.Fatalf("7.5 GiB makes 10.1 GiB in use, over the cap: the display card must not be picked, got %v %v", ids, err)
	}
	_, err := pick(config.Config{}, 7.5, 3)
	if err == nil || !strings.Contains(err.Error(), "gpu_display_cap_pct") || !strings.Contains(err.Error(), "60 %") {
		t.Fatalf("three cards at 7.5 GiB each cannot be met, and the refusal states the cap: %v", err)
	}
	if ids, err := pickRange(config.Config{}, 0, 1, 3); err != nil || strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") {
		t.Fatalf("a request that declares no --vram cannot be shown to stay under the cap: %v %v", ids, err)
	}
	if ids, err := pickRange(config.Config{GPUDisplayCapPct: &zero}, 6, 1, 3); err != nil || strings.Contains(strings.Join(ids, ","), "gpu-bbbb0000-x") {
		t.Fatalf("gpu_display_cap_pct 0 is the operator's switch: %v %v", ids, err)
	}
}

// Naming the display card is not a way past its rule (incident: an agent's `gpu reserve --devices <display card>`
// rendered on the operator's screen while he was at the desk, to 83 % of its VRAM). The verb with --devices on the
// screen's card: refused (nothing held) while the desk is not idle and with no --vram declared; taken once the desk is
// idle and the declared footprint fits under the cap.
func TestGPUReserveDevicesOnTheDisplayCardIsHeldToItsRule(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCannedDisplayTable(t)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	reserve := func(extra ...string) error {
		args := append([]string{"--config", cfg, "--class", "media", "--devices", "1", "--wait", "0", "--ram", "0", "--reason", "display rule"}, extra...)
		return runGPUReserve(append(args, helperCmd()...))
	}
	displayBusyFn = func(config.Config, []gpuprobe.Card) string { return "the operator is at the desk" }
	if err := reserve("--vram", "4"); err == nil || !strings.Contains(err.Error(), "the operator is at the desk") {
		t.Fatalf("the operator at the desk closes the display card to a named request too: %v", err)
	}
	displayBusyFn = func(config.Config, []gpuprobe.Card) string { return "" }
	if err := reserve(); err == nil || !strings.Contains(err.Error(), "--vram") {
		t.Fatalf("a named request that declares no footprint cannot be shown to stay under the cap: %v", err)
	}
	if n := len(m.Leases()); n != 0 {
		t.Fatalf("a refused request holds nothing, found %d lease(s)", n)
	}
	if err := reserve("--vram", "4"); err != nil {
		t.Fatalf("the desk idle and 4 GiB on 2.6 in use of 15.9 is under the 60 percent cap: the named card is taken: %v", err)
	}
}
