package config

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// DefaultGPUHostRAMHeadroomGiB is the host RAM every GPU lease grant, and the card allocator, keep
// uncommitted beyond what a lease declares, when gpu_host_ram_headroom_gib is unset. It is the
// probe's own default (8 GiB, gpuprobe.DefaultHostRAMHeadroomGiB: it was 4 while only the
// `--cards` path read it, and the paging incident of 2026-10-09 showed 4 is inside the noise of the
// desktop's own growth). A floor chosen to keep declared loads away from paging, not a measurement of any one job.
const DefaultGPUHostRAMHeadroomGiB = gpuprobe.DefaultHostRAMHeadroomGiB

// GPUHostRAMHeadroom is gpu_host_ram_headroom_gib with the default applied.
func (c Config) GPUHostRAMHeadroom() float64 {
	if c.GPUHostRAMHeadroomGiB > 0 {
		return c.GPUHostRAMHeadroomGiB
	}
	return DefaultGPUHostRAMHeadroomGiB
}

// DefaultGPUDisplayCapPct is the share of the display card's VRAM the card allocator may fill when
// gpu_display_cap_pct is unset: the operator's rule of 2026-10-11 ("the display card may be used up to 60 percent
// at all times unless I say I need it"). It is the operator's figure, not a measurement.
const DefaultGPUDisplayCapPct = 60

// GPUDisplayCap is gpu_display_cap_pct with the default applied: 60 when unset, and 0 (the card closed to the
// allocator) only when the operator wrote 0. A value that is not a share (below 0 or above 100) reads as 0, closed,
// never as the whole card: Load refuses it by name, but it returns the Config beside that error and the one-shot
// verbs (gpu reserve among them) go on with the value, so a typo of 600 for 60 must not open the desktop's memory
// to a job. A Config set by hand takes the same rule.
func (c Config) GPUDisplayCap() int {
	if c.GPUDisplayCapPct == nil {
		return DefaultGPUDisplayCapPct
	}
	if v := *c.GPUDisplayCapPct; v >= 0 && v <= 100 {
		return v
	}
	return 0
}

// validateGPUDisplayCap refuses a gpu_display_cap_pct that is not a share of the card, by name: a typo of 600 for
// 60 would otherwise hand the desktop's memory to a job without a word.
func validateGPUDisplayCap(c Config) error {
	if c.GPUDisplayCapPct == nil {
		return nil
	}
	if v := *c.GPUDisplayCapPct; v < 0 || v > 100 {
		return fmt.Errorf("gpu_display_cap_pct: %d is not a share of the display card (0 closes it, 100 is all of it)", v)
	}
	return nil
}
