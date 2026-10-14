package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// DefaultGPUHostRAMHoldMaxSec is how long a lease that unloaded the seat may hold its cards waiting for the host to have the
// room for its declared RAM, when gpu_hostram_hold_max_sec is unset. A chosen figure, not a measurement: ten minutes is
// several times the minutes a seat's memory takes to come back after its unload, and short enough that a lease whose figure
// the host can never meet frees the cards for the queue behind it.
const DefaultGPUHostRAMHoldMaxSec = 600

// GPUHostRAMHoldMax is gpu_hostram_hold_max_sec as a duration. SEAM: the accessor is not written yet.
func (c Config) GPUHostRAMHoldMax() time.Duration {
	return 0
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

// WithSource returns c recording the file its values were read from, so LiveGPUDisplayCap can follow that file.
// Load records it; a caller that builds a Config by hand and wants the switch to follow a file does it here.
func (c Config) WithSource(path string) Config {
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	c.sourcePath = path
	return c
}

// LiveGPUDisplayCap is gpu_display_cap_pct as the file this Config was read from says NOW, for the readers
// that must see the operator's "I need the card" switch (0) while the process runs: the card allocator's input
// (gpualloc.BuildInput), `gpu status` and `gpu cards` (displayCapNow in gpu_cards.go) and the offload_status
// brief (internal/mcpserver/status_section.go). A process never reloads its config, so GPUDisplayCap, the value at
// load, would leave the switch dead for a `gpu reserve --wait 8h` already in line, for a detached holder, and for the long-lived
// MCP server that places media calls. The read is one small file and one key: a plain read on every call, no
// memo, because a memo keyed on the file's timestamp can miss a rewrite that keeps both timestamp and size.
//
// A file that cannot be read or decoded at this moment (an editor part-way through a save, a lock) keeps the
// last value read from it, and before any read the value this Config holds, so a torn write never reopens a card
// the operator has just closed. A key removed reads as the default (60), a value that is not a share as closed
// (GPUDisplayCap). A Config with no source file answers with its own value.
func (c Config) LiveGPUDisplayCap() int {
	if c.sourcePath == "" {
		return c.GPUDisplayCap()
	}
	if v, ok := readDisplayCap(c.sourcePath); ok {
		lastDisplayCap.Store(c.sourcePath, v)
		return v
	}
	if v, ok := lastDisplayCap.Load(c.sourcePath); ok {
		return v.(int)
	}
	return c.GPUDisplayCap()
}

// lastDisplayCap is the cap last read from each config file, so a read that fails keeps it (LiveGPUDisplayCap).
var lastDisplayCap sync.Map

// readDisplayCap reads gpu_display_cap_pct from the config file at path, with Load's own decoding (a BOM is
// stripped) and GPUDisplayCap's own meaning of the value. ok is false when the file cannot be read or decoded.
func readDisplayCap(path string) (share int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var key struct {
		Cap *int `json:"gpu_display_cap_pct"`
	}
	if err := json.Unmarshal(StripBOM(b), &key); err != nil {
		return 0, false
	}
	return Config{GPUDisplayCapPct: key.Cap}.GPUDisplayCap(), true
}
