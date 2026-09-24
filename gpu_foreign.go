package main

// Foreign GPU memory holders (register D-1xx-4, 2026-09-23; R4/noise-repro on
// the OptiPlex): idle DaVinci Resolve held 1,450 MiB of an 8 GB card and
// nothing reported it. `gpuactivity.SampleProcesses` (nvidia-smi
// --query-compute-apps) already lists processes on the cards, but on Windows
// (WDDM) nvidia-smi's own per-process memory is `[N/A]` — see GPUProcess's
// doc comment in internal/gpuactivity/smi.go — so a Windows box had NO
// per-process VRAM source at all. The Windows perf counter
// `\GPU Process Memory(*)\Dedicated Usage`, read by pid (internal/fleetnode,
// already used there for the fleet-dispatcher footprint sampler) closes that
// gap; nvidia-smi's compute-apps query is kept as the Linux/NVIDIA path,
// where it DOES report real per-process memory.
//
// This is deliberately in package main, not internal/gpuactivity: fleetnode
// already imports gpuactivity (internal/fleetnode/server.go), so the reverse
// import would cycle. Package main already imports both.
//
// Follow-up (2026-09-24, operator-reported): the warning fired on the Qube
// for an ordinary desktop session — Code.exe, chrome.exe, SnippingTool.exe,
// explorer.exe and friends, all sitting on the Qube's DISPLAY card (nvidia-smi
// index 1, the 3-card law's own "never a work card"). The PDH source this
// file reads has no per-card identity (AllProcessDedicatedMiB sums/duplicates
// per pid, not per adapter), so two more signals are cross-referenced,
// best-effort, before a resident is reported:
//   - gpuprobe.DisplayCardUUIDs(...) — the SAME rule the 3-card law and the
//     placement guards already use for "which card is the operator's screen"
//     (nil on a single-GPU box, where that card IS the work card).
//   - gpuactivity.SampleProcesses (nvidia-smi --query-compute-apps) for a
//     PID -> GPU UUID correlation: its used_memory is [N/A] on WDDM (the
//     reason this file exists), but gpu_uuid is NOT — nvidia-smi types every
//     Windows desktop row C+G (see gpuprobe/display.go's package comment),
//     so an ordinary desktop app shows up here with its card identity even
//     though PDH is the only source with its real MiB figure.
//
// A resident whose every KNOWN GPU UUID is a display card is dropped — but
// only when the box has an eligible non-display card to score (that is what
// a nil DisplayCardUUIDs already encodes), which is also the single-card-box
// case where the lease has no other card to fence: there the display card
// IS the work card, so nothing is filtered and a Resolve-class hog still
// warns (foreignMinMiB_test.go's OptiPlex-shaped case). A PID absent from the
// compute-apps list (unknown attribution) is never filtered — absence of
// evidence is not evidence of "it's only on the display card".
//
// A per-process floor (foreignDefaultMinMiB, overridable via
// config.Config.ForeignGPUMinMiB) does the rest: an ordinary desktop
// resident sits well under it, so most of the incident's own list (PAIR.exe
// 366, chrome.exe 365, SnippingTool.exe 361, msedgewebview2.exe 238,
// explorer.exe 154, csrss.exe 119, opera.exe 65 MiB) never reaches the
// display-card check at all. Combined with foreignDenylist naming the known
// shell/desktop hosts by name (belt-and-suspenders for a process the
// threshold alone doesn't catch), only a real work-card hog like idle
// Resolve at 1,450 MiB clears every filter.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// printForeignGPUWarning writes the one-line warning to out (`gpu reserve`'s
// stderr) at lease acquire, e.g.:
//
//	gpu reserve: WARNING foreign GPU memory holders: Resolve pid 9416 1450 MiB
//
// Silent (writes nothing) when there are none, which is the common case —
// never a reason to refuse or delay the reservation itself: this is
// evidence, not a gate. Never kills or touches the processes it names. cfg
// supplies ForeignGPUMinMiB (0 = built-in default); the config value is read
// here, not baked in, so an operator can raise/lower the floor per machine.
func printForeignGPUWarning(out io.Writer, cfg config.Config) {
	if w := formatForeignWarning(foreignGPUHolders(context.Background(), cfg)); w != "" {
		fmt.Fprintf(out, "gpu reserve: %s\n", w)
	}
}

// ForeignGPUHolder is one non-harness process holding significant dedicated
// VRAM at the moment of the read.
type ForeignGPUHolder struct {
	Name string
	PID  int
	MiB  int
}

// foreignDefaultMinMiB is the floor below which a resident is noise, not a
// finding, when config.Config.ForeignGPUMinMiB is unset (0/negative): small
// allocations (icons, thumbnail codecs, the shell) are normal on any desktop
// box and would turn every `gpu reserve` into a wall of warnings. Raised
// from the original 64 MiB to 512 (2026-09-24, operator-reported): 64 MiB
// let ordinary desktop apps (PAIR.exe 366, chrome.exe 365, SnippingTool.exe
// 361, msedgewebview2.exe 238 MiB — all measured on the Qube's display card)
// through; DaVinci Resolve's measured 1,450 MiB idle-hog clears 512 by
// nearly 3x, so the headline case this file exists for is unaffected.
const foreignDefaultMinMiB = 512

// effectiveForeignMinMiB resolves the configured floor: cfg.ForeignGPUMinMiB
// when positive, else foreignDefaultMinMiB. A zero-value config.Config (a
// caller that never loaded one) is therefore identical to the built-in
// default, not an accidental "warn on everything".
func effectiveForeignMinMiB(cfg config.Config) int {
	if cfg.ForeignGPUMinMiB > 0 {
		return cfg.ForeignGPUMinMiB
	}
	return foreignDefaultMinMiB
}

// foreignDenylist names processes this box's OWN harness/OS stack is
// expected to hold VRAM under — never "foreign" in the sense this exists to
// warn about. Matched case-insensitively against the executable's base name
// with any ".exe"/".EXE" suffix trimmed. BEST-EFFORT BY NAME, not a security
// boundary: a differently-named build of the same engine, or an unrelated
// process that happens to share one of these names, is a known, accepted
// imprecision (see the package comment on gpuactivity.ForeignGPUHolders'
// callers — "keep it best-effort, time-bounded, never fatal").
//
// The Windows shell/desktop rows (csrss, explorer, and the shell-experience
// hosts below) were added 2026-09-24 alongside dwm: the per-process floor
// already clears them on the Qube's own measured numbers (explorer.exe 154,
// csrss.exe 119 MiB), but naming them explicitly means a busier desktop
// session that pushes one of them over the floor still doesn't warn — the
// operator's own OS chrome is never the "GPU hog" this file exists to catch.
var foreignDenylist = map[string]bool{
	"dwm":                     true, // Windows' own desktop compositor: always resident, never actionable
	"csrss":                   true, // Windows client/server runtime — OS-owned, never actionable
	"explorer":                true, // the desktop shell itself
	"sihost":                  true, // Shell Infrastructure Host
	"shellexperiencehost":     true,
	"startmenuexperiencehost": true,
	"searchhost":              true,
	"textinputhost":           true,
	"offload-harness":         true,
	"llama-server":            true,
	"llama-swap":              true,
	"sd-cli":                  true,
	"sdcpp":                   true,
	"whisper-server":          true,
	"ffmpeg":                  true,
	"ffprobe":                 true,
	"python":                  true, // ComfyUI's interpreter on this fleet's boxes
	"pythonw":                 true,
	"comfyui":                 true,
	"paddleocr-server":        true,
}

// foreignProbeTimeout bounds the whole read: never fatal, never worth
// delaying a lease acquire or a status call over a wedged driver or a slow
// PDH counter refresh. The Windows PDH path itself is a handful of local
// syscalls in the low milliseconds (vram_windows_test.go's live smoke), but
// the display-card cross-reference added 2026-09-24 spends up to
// foreignDisplayProbeTimeout on real nvidia-smi SUBPROCESS calls, so this
// bound grew from the original 1500ms to cover PDH (near-instant) plus that
// one bounded probe with headroom — still far under gpuactivity's own 4s
// smiTimeout, and still a best-effort return-nil-on-timeout, never a block
// on the lease itself.
const foreignProbeTimeout = 2500 * time.Millisecond

// foreignDisplayProbeTimeout bounds the display-card cross-reference alone
// (one gpuprobe.Read plus, on a multi-card box, one gpuactivity.SampleProcesses
// call — both share this single deadline, so the pair never exceeds it). A
// timeout here degrades to "exclude nothing" (nil displayUUIDs / nil
// displayOnly), the same safe default as no display information at all —
// never a reason to lose the MiB numbers PDH already has in hand.
const foreignDisplayProbeTimeout = 1200 * time.Millisecond

// foreignGPUHolders lists non-harness processes holding at least the
// configured floor (effectiveForeignMinMiB) of dedicated VRAM right now,
// excluding a resident confined to a display card on a box that has a
// non-display card to actually score. Best-effort and time-bounded: any
// failure (no PDH counters, no nvidia-smi, a timeout) returns an empty list,
// never an error the caller must handle — this is advisory evidence for a
// warning line, not a gate on GPU work.
func foreignGPUHolders(ctx context.Context, cfg config.Config) []ForeignGPUHolder {
	type result struct {
		holders []ForeignGPUHolder
	}
	done := make(chan result, 1)
	go func() { done <- result{rawForeignGPUHolders(ctx, cfg)} }()
	select {
	case r := <-done:
		return r.holders
	case <-time.After(foreignProbeTimeout):
		return nil
	case <-ctx.Done():
		return nil
	}
}

// rawForeignGPUHolders does the actual read: the Windows PDH per-process
// source first (the only source with real per-process memory on WDDM), and
// nvidia-smi's compute-apps query as the Linux/NVIDIA fallback (which does
// NOT need this fallback on Windows — its own per-process memory there is
// always [N/A], so falling back to it there would report nothing new).
//
// Both branches cross-reference gpuprobe.DisplayCardUUIDs before reporting a
// resident — see the package comment for why that read is safe to skip
// (nil = exclude nothing) and why it never has to be exact.
func rawForeignGPUHolders(ctx context.Context, cfg config.Config) []ForeignGPUHolder {
	minMiB := effectiveForeignMinMiB(cfg)
	dctx, cancel := context.WithTimeout(ctx, foreignDisplayProbeTimeout)
	defer cancel()
	displayUUIDs := gpuprobe.DisplayCardUUIDs(readDevicesBestEffort(dctx))

	if byPID, pdhErr := fleetnode.AllProcessDedicatedMiB(); pdhErr == nil {
		names, _ := fleetnode.ProcessNames() // best-effort; a nil map just means "no name known"
		var displayOnly map[int]bool
		// Only worth the extra nvidia-smi call when there is a display card to
		// check against — the common single-card box already got its answer
		// (nil) from the cheap DisplayCardUUIDs call above.
		if len(displayUUIDs) > 0 {
			if procs, perr := gpuactivity.SampleProcesses(dctx); perr == nil {
				displayOnly = displayOnlyPIDs(procs, displayUUIDs)
			}
		}
		return classifyForeign(byPID, names, minMiB, displayOnly)
	}
	// Fall back to nvidia-smi's compute-apps query (Linux/NVIDIA: real
	// per-process memory AND gpu_uuid in the same row, so the display-card
	// check needs no separate correlation; Windows without working PDH
	// counters: UsedKnown is false for every row and this contributes
	// nothing — see GPUProcess's own doc comment — so the caller correctly
	// sees an empty list rather than a crash or a guess).
	procs, serr := gpuactivity.SampleProcesses(ctx)
	if serr != nil {
		return nil
	}
	var out []ForeignGPUHolder
	for _, p := range procs {
		if !p.UsedKnown {
			continue
		}
		if isHarnessOwned(p.Name) || p.UsedMiB < minMiB {
			continue
		}
		if len(displayUUIDs) > 0 && displayUUIDs[p.GPUUUID] {
			continue
		}
		out = append(out, ForeignGPUHolder{Name: shortProcessName(p.Name), PID: p.PID, MiB: p.UsedMiB})
	}
	return sortForeign(out)
}

// classifyForeign turns raw pid->MiB PDH instances plus a pid->name map into
// the filtered, sorted foreign-holder list. minMiB is the resolved floor
// (effectiveForeignMinMiB); displayOnly is the set of pids whose every known
// GPU is a display card (displayOnlyPIDs) — nil (the safe default) excludes
// nothing.
func classifyForeign(byPID []fleetnode.ProcessDedicated, names map[int]string, minMiB int, displayOnly map[int]bool) []ForeignGPUHolder {
	var out []ForeignGPUHolder
	for _, pd := range byPID {
		if pd.MiB < minMiB {
			continue
		}
		if displayOnly[pd.PID] {
			continue
		}
		name := names[pd.PID]
		if isHarnessOwned(name) {
			continue
		}
		if name == "" {
			name = fmt.Sprintf("pid %d", pd.PID)
		}
		out = append(out, ForeignGPUHolder{Name: shortProcessName(name), PID: pd.PID, MiB: pd.MiB})
	}
	return sortForeign(out)
}

// displayOnlyPIDs correlates nvidia-smi's compute-apps process list (which
// carries a real gpu_uuid per row even where used_memory is [N/A] on WDDM —
// see the package comment) against displayUUIDs (gpuprobe.DisplayCardUUIDs)
// to find pids whose EVERY known GPU is a display card. displayUUIDs empty
// (single-card box, or a driver that never reports display_active) returns
// nil — exclude nothing, the safe default. A pid absent from procs entirely
// is never included: unknown attribution must never read as "confined to
// the display card" and silently drop a real work-card hog.
func displayOnlyPIDs(procs []gpuactivity.GPUProcess, displayUUIDs map[string]bool) map[int]bool {
	if len(displayUUIDs) == 0 {
		return nil
	}
	known := map[int]bool{}
	touchedNonDisplay := map[int]bool{}
	for _, p := range procs {
		if p.GPUUUID == "" {
			continue
		}
		known[p.PID] = true
		if !displayUUIDs[p.GPUUUID] {
			touchedNonDisplay[p.PID] = true
		}
	}
	out := map[int]bool{}
	for pid := range known {
		if !touchedNonDisplay[pid] {
			out[pid] = true
		}
	}
	return out
}

// readDevicesBestEffort runs the per-device nvidia-smi query for the
// display-card check alone: an error (no nvidia-smi, a timeout, a wedged
// driver) returns nil, which DisplayCardUUIDs already treats as "exclude
// nothing" — never a reason to lose the PDH-sourced MiB numbers this file's
// whole warning exists to report.
func readDevicesBestEffort(ctx context.Context) []gpuprobe.Device {
	devices, err := gpuprobe.Read(ctx)
	if err != nil {
		return nil
	}
	return devices
}

func sortForeign(out []ForeignGPUHolder) []ForeignGPUHolder {
	sort.Slice(out, func(i, j int) bool { return out[i].MiB > out[j].MiB })
	return out
}

func isHarnessOwned(name string) bool {
	base := strings.ToLower(shortProcessName(name))
	base = strings.TrimSuffix(base, ".exe")
	return foreignDenylist[base]
}

func shortProcessName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// formatForeignWarning renders holders as the one-line warning `gpu reserve`
// and `gpu status` both print, e.g.:
//
//	WARNING foreign GPU memory holders: Resolve pid 9416 1450 MiB, chrome pid 21044 210 MiB
//
// "" when holders is empty — nothing is printed for the common case.
func formatForeignWarning(holders []ForeignGPUHolder) string {
	if len(holders) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, fmt.Sprintf("%s pid %d %d MiB", h.Name, h.PID, h.MiB))
	}
	return "WARNING foreign GPU memory holders: " + strings.Join(parts, ", ")
}
