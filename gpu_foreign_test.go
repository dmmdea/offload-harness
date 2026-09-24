package main

// Foreign GPU memory holders (register D-1xx-4, 2026-09-23). These tests pin
// the pure classification/formatting logic (denylist, MiB floor, sort,
// message shape) with synthetic PDH-shaped input — the live PDH/toolhelp
// reads and the nvidia-smi fallback are best-effort by design (see
// gpu_foreign.go's package comment) and are not reproducible on a CI box
// without a GPU, so they are exercised through this pure seam instead.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// THE HEADLINE CASE, shaped exactly like the incident: DaVinci Resolve at
// 1,450 MiB must be reported; dwm.exe (the OS compositor) and llama-server.exe
// (the agent seat, harness-owned) must not, whatever their own MiB; a
// resident under the noise floor must not, however it is named. No
// displayOnly set here (nil) — this is the plain classification path.
func TestClassifyForeignFiltersHarnessOwnedAndSmallResidents(t *testing.T) {
	byPID := []fleetnode.ProcessDedicated{
		{PID: 9416, MiB: 1450}, // Resolve — must be reported
		{PID: 100, MiB: 320},   // dwm — OS compositor, never a finding
		{PID: 200, MiB: 6176},  // llama-server — the agent seat itself
		{PID: 300, MiB: 10},    // trivial — below the noise floor
		{PID: 400, MiB: 600},   // unnamed process, above the floor — must be reported
	}
	names := map[int]string{
		9416: `C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe`,
		100:  "dwm.exe",
		200:  "llama-server.exe",
		300:  "SomeTinyThing.exe",
		// 400 deliberately has no entry: an unresolved name must still surface.
	}

	got := classifyForeign(byPID, names, foreignDefaultMinMiB, nil)
	if len(got) != 2 {
		t.Fatalf("classifyForeign = %+v, want exactly 2 entries (Resolve and the unnamed pid 400)", got)
	}
	// Sorted MiB descending: Resolve first.
	if got[0].PID != 9416 || got[0].MiB != 1450 || got[0].Name != "Resolve.exe" {
		t.Fatalf("first entry = %+v, want Resolve pid 9416 1450 MiB", got[0])
	}
	if got[1].PID != 400 || got[1].MiB != 600 {
		t.Fatalf("second entry = %+v, want the unnamed pid 400 at 600 MiB", got[1])
	}
	for _, h := range got {
		if h.PID == 100 || h.PID == 200 || h.PID == 300 {
			t.Fatalf("a harness/OS-owned or sub-floor resident leaked into the foreign list: %+v", h)
		}
	}
}

// Qube-shaped: 3 cards, index 1 is the display (nvidia-smi's own
// display_active), the operator's ordinary desktop session sits ON it
// (Code.exe, PAIR.exe, chrome.exe, SnippingTool.exe, msedgewebview2.exe,
// explorer.exe, csrss.exe, opera.exe — the exact incident list, PDH-measured
// MiB), and idle DaVinci Resolve sits on a WORK card (index 0). Only Resolve
// must be reported: every desktop app is either display-card-only (Code.exe,
// the one entry above the floor) or under the floor to begin with — the
// double coverage is intentional, this is the regression the incident
// actually hit.
func TestClassifyForeignIgnoresDisplayCardDesktopOnMultiCardBox(t *testing.T) {
	devices := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-work-0", DisplayActive: false},
		{Index: 1, UUID: "GPU-display-1", DisplayActive: true},
		{Index: 2, UUID: "GPU-work-2", DisplayActive: false},
	}
	displayUUIDs := gpuprobe.DisplayCardUUIDs(devices)
	procs := []gpuactivity.GPUProcess{
		{PID: 37504, GPUUUID: "GPU-display-1", Name: "Code.exe"},
		{PID: 40001, GPUUUID: "GPU-display-1", Name: "PAIR.exe"},
		{PID: 40002, GPUUUID: "GPU-display-1", Name: "chrome.exe"},
		{PID: 40003, GPUUUID: "GPU-display-1", Name: "SnippingTool.exe"},
		{PID: 40004, GPUUUID: "GPU-display-1", Name: "msedgewebview2.exe"},
		{PID: 40005, GPUUUID: "GPU-display-1", Name: "explorer.exe"},
		{PID: 40006, GPUUUID: "GPU-display-1", Name: "csrss.exe"},
		{PID: 40007, GPUUUID: "GPU-display-1", Name: "opera.exe"},
		// Resolve never shows up as a "compute app" in this synthetic set —
		// exactly the unknown-attribution case: it must NOT be treated as
		// display-only just because it's absent, and its real card (0) is
		// what the PDH byPID data below says anyway.
	}
	displayOnly := displayOnlyPIDs(procs, displayUUIDs)

	byPID := []fleetnode.ProcessDedicated{
		{PID: 9416, MiB: 1450}, // Resolve, on work card 0 — must be reported
		{PID: 37504, MiB: 625}, // Code.exe, display card only — must NOT
		{PID: 40001, MiB: 366}, // PAIR.exe — under the floor anyway
		{PID: 40002, MiB: 365}, // chrome.exe
		{PID: 40003, MiB: 361}, // SnippingTool.exe
		{PID: 40004, MiB: 238}, // msedgewebview2.exe
		{PID: 40005, MiB: 154}, // explorer.exe
		{PID: 40006, MiB: 119}, // csrss.exe
		{PID: 40007, MiB: 65},  // opera.exe
	}
	names := map[int]string{
		9416: "Resolve.exe", 37504: "Code.exe", 40001: "PAIR.exe", 40002: "chrome.exe",
		40003: "SnippingTool.exe", 40004: "msedgewebview2.exe", 40005: "explorer.exe",
		40006: "csrss.exe", 40007: "opera.exe",
	}

	got := classifyForeign(byPID, names, foreignDefaultMinMiB, displayOnly)
	if len(got) != 1 || got[0].PID != 9416 || got[0].MiB != 1450 {
		t.Fatalf("classifyForeign on the Qube-shaped desktop session = %+v, want exactly [Resolve pid 9416 1450 MiB]", got)
	}
}

// OptiPlex-shaped: ONE card, which is also the display (a single-GPU
// desktop). gpuprobe.DisplayCardUUIDs returns nil here BY DESIGN (a
// display-card-only card is only ever excluded when a non-display card
// exists to score instead) — so displayOnlyPIDs must also see nil
// displayUUIDs and exclude nothing: idle Resolve at 1,450 MiB on the box's
// only card still warns, and ordinary desktop residents below the floor
// still don't.
func TestClassifyForeignStillWarnsOnSingleCardBox(t *testing.T) {
	devices := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-onlycard-0", DisplayActive: true},
	}
	displayUUIDs := gpuprobe.DisplayCardUUIDs(devices) // must be nil/empty: no eligible non-display card
	if len(displayUUIDs) != 0 {
		t.Fatalf("a single-card box's own display card must not be excluded (nothing left to score), got display set %v", displayUUIDs)
	}
	procs := []gpuactivity.GPUProcess{
		{PID: 9416, GPUUUID: "GPU-onlycard-0", Name: "Resolve.exe"},
		{PID: 500, GPUUUID: "GPU-onlycard-0", Name: "explorer.exe"},
	}
	displayOnly := displayOnlyPIDs(procs, displayUUIDs)
	if displayOnly != nil {
		t.Fatalf("displayOnlyPIDs with an empty display set must return nil, got %v", displayOnly)
	}

	byPID := []fleetnode.ProcessDedicated{
		{PID: 9416, MiB: 1450}, // Resolve — the box's only card is still a work card
		{PID: 500, MiB: 154},   // explorer.exe — under the floor
	}
	names := map[int]string{9416: "Resolve.exe", 500: "explorer.exe"}

	got := classifyForeign(byPID, names, foreignDefaultMinMiB, displayOnly)
	if len(got) != 1 || got[0].PID != 9416 || got[0].MiB != 1450 {
		t.Fatalf("classifyForeign on the OptiPlex-shaped single-card box = %+v, want exactly [Resolve pid 9416 1450 MiB]", got)
	}
}

// The denylist match is case-insensitive and tolerates the ".exe" suffix
// either way, since PDH's process name and Windows' own casing conventions
// are not guaranteed uniform.
func TestIsHarnessOwnedMatchesCaseInsensitivelyWithOrWithoutExeSuffix(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"dwm.exe", true},
		{"DWM.EXE", true},
		{`C:\Windows\System32\dwm.exe`, true},
		{"llama-server.exe", true},
		{"offload-harness.exe", true},
		{"Resolve.exe", false},
		{"chrome.exe", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isHarnessOwned(c.name); got != c.want {
			t.Errorf("isHarnessOwned(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The message shape is pinned exactly, including the "WARNING foreign GPU
// memory holders:" prefix and the "<name> pid <pid> <mib> MiB" per-entry
// form (the task's own worked example), and an empty list must render
// nothing at all — never a "no foreign holders" line nobody asked for.
func TestFormatForeignWarningShape(t *testing.T) {
	if got := formatForeignWarning(nil); got != "" {
		t.Fatalf("formatForeignWarning(nil) = %q, want empty", got)
	}
	got := formatForeignWarning([]ForeignGPUHolder{
		{Name: "Resolve.exe", PID: 9416, MiB: 1450},
		{Name: "chrome.exe", PID: 21044, MiB: 210},
	})
	want := "WARNING foreign GPU memory holders: Resolve.exe pid 9416 1450 MiB, chrome.exe pid 21044 210 MiB"
	if got != want {
		t.Fatalf("formatForeignWarning = %q, want %q", got, want)
	}
	if !strings.Contains(got, "pid 9416") || !strings.Contains(got, "1450 MiB") {
		t.Fatalf("formatForeignWarning dropped the pid/MiB detail an operator reads: %q", got)
	}
}

// classifyForeign must never touch anything below the noise floor — a small
// desktop resident (an icon renderer, a thumbnail codec) is normal on every
// box and would turn every `gpu reserve` into a wall of warnings nobody
// reads.
func TestClassifyForeignRespectsTheNoiseFloor(t *testing.T) {
	byPID := []fleetnode.ProcessDedicated{{PID: 1, MiB: foreignDefaultMinMiB - 1}}
	if got := classifyForeign(byPID, map[int]string{1: "tiny.exe"}, foreignDefaultMinMiB, nil); len(got) != 0 {
		t.Fatalf("a resident 1 MiB under the floor was reported: %+v", got)
	}
	byPID = []fleetnode.ProcessDedicated{{PID: 1, MiB: foreignDefaultMinMiB}}
	if got := classifyForeign(byPID, map[int]string{1: "atfloor.exe"}, foreignDefaultMinMiB, nil); len(got) != 1 {
		t.Fatalf("a resident exactly AT the floor must be reported: %+v", got)
	}
}

// effectiveForeignMinMiB: an explicit positive override wins; zero/negative
// falls back to the built-in default. This is the "configurable" half of the
// fix — an operator can raise or lower the floor per machine via
// config.Config.ForeignGPUMinMiB without touching code.
func TestEffectiveForeignMinMiB(t *testing.T) {
	if got := effectiveForeignMinMiB(config.Config{}); got != foreignDefaultMinMiB {
		t.Fatalf("effectiveForeignMinMiB(zero-value config) = %d, want the built-in default %d", got, foreignDefaultMinMiB)
	}
	if got := effectiveForeignMinMiB(config.Config{ForeignGPUMinMiB: 1024}); got != 1024 {
		t.Fatalf("effectiveForeignMinMiB(1024 override) = %d, want 1024", got)
	}
	if got := effectiveForeignMinMiB(config.Config{ForeignGPUMinMiB: -5}); got != foreignDefaultMinMiB {
		t.Fatalf("effectiveForeignMinMiB(negative) = %d, want the built-in default %d (never treated as 'warn on everything')", got, foreignDefaultMinMiB)
	}
}
