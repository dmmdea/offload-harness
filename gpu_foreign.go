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

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// printForeignGPUWarning writes the one-line warning to out (`gpu reserve`'s
// stderr) at lease acquire, e.g.:
//
//	gpu reserve: WARNING foreign GPU memory holders: Resolve pid 9416 1450 MiB
//
// Silent (writes nothing) when there are none, which is the common case —
// never a reason to refuse or delay the reservation itself: this is
// evidence, not a gate. Never kills or touches the processes it names.
func printForeignGPUWarning(out io.Writer) {
	if w := formatForeignWarning(foreignGPUHolders(context.Background())); w != "" {
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

// foreignMinMiB is the floor below which a resident is noise, not a finding:
// small allocations (icons, thumbnail codecs, the shell) are normal on any
// desktop box and would turn every `gpu reserve` into a wall of warnings.
// DaVinci Resolve's measured 1,450 MiB clears this by more than an order of
// magnitude.
const foreignMinMiB = 64

// foreignDenylist names processes this box's OWN harness/OS stack is
// expected to hold VRAM under — never "foreign" in the sense this exists to
// warn about. Matched case-insensitively against the executable's base name
// with any ".exe"/".EXE" suffix trimmed. BEST-EFFORT BY NAME, not a security
// boundary: a differently-named build of the same engine, or an unrelated
// process that happens to share one of these names, is a known, accepted
// imprecision (see the package comment on gpuactivity.ForeignGPUHolders'
// callers — "keep it best-effort, time-bounded, never fatal").
var foreignDenylist = map[string]bool{
	"dwm":              true, // Windows' own desktop compositor: always resident, never actionable
	"offload-harness":  true,
	"llama-server":     true,
	"llama-swap":       true,
	"sd-cli":           true,
	"sdcpp":            true,
	"whisper-server":   true,
	"ffmpeg":           true,
	"ffprobe":          true,
	"python":           true, // ComfyUI's interpreter on this fleet's boxes
	"pythonw":          true,
	"comfyui":          true,
	"paddleocr-server": true,
}

// foreignProbeTimeout bounds the whole read: never fatal, never worth
// delaying a lease acquire or a status call over a wedged driver or a slow
// PDH counter refresh. Shorter than gpuactivity's own smiTimeout (4s): that
// bound covers spawning and waiting on an nvidia-smi SUBPROCESS, while the
// Windows path here (the common case) is a handful of local PDH syscalls
// against counters vram_windows_test.go's live smoke measures in the low
// milliseconds — `gpu status` is otherwise a cheap, near-instant read-only
// call (a review finding, 2026-09-23), so this stays tight rather than
// matching the subprocess bound out of habit.
const foreignProbeTimeout = 1500 * time.Millisecond

// foreignGPUHolders lists non-harness processes holding at least
// foreignMinMiB of dedicated VRAM right now. Best-effort and time-bounded:
// any failure (no PDH counters, no nvidia-smi, a timeout) returns an empty
// list, never an error the caller must handle — this is advisory evidence
// for a warning line, not a gate on GPU work.
func foreignGPUHolders(ctx context.Context) []ForeignGPUHolder {
	type result struct {
		holders []ForeignGPUHolder
	}
	done := make(chan result, 1)
	go func() { done <- result{rawForeignGPUHolders()} }()
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
func rawForeignGPUHolders() []ForeignGPUHolder {
	if byPID, pdhErr := fleetnode.AllProcessDedicatedMiB(); pdhErr == nil {
		names, _ := fleetnode.ProcessNames() // best-effort; a nil map just means "no name known"
		return classifyForeign(byPID, names)
	}
	// Fall back to nvidia-smi's compute-apps query (Linux/NVIDIA: real
	// per-process memory; Windows without working PDH counters: UsedKnown is
	// false for every row and this contributes nothing — see GPUProcess's own
	// doc comment — so the caller correctly sees an empty list rather than a
	// crash or a guess).
	procs, serr := gpuactivity.SampleProcesses(context.Background())
	if serr != nil {
		return nil
	}
	var out []ForeignGPUHolder
	for _, p := range procs {
		if !p.UsedKnown {
			continue
		}
		if isHarnessOwned(p.Name) || p.UsedMiB < foreignMinMiB {
			continue
		}
		out = append(out, ForeignGPUHolder{Name: shortProcessName(p.Name), PID: p.PID, MiB: p.UsedMiB})
	}
	return sortForeign(out)
}

// classifyForeign turns raw pid->MiB PDH instances plus a pid->name map into
// the filtered, sorted foreign-holder list.
func classifyForeign(byPID []fleetnode.ProcessDedicated, names map[int]string) []ForeignGPUHolder {
	var out []ForeignGPUHolder
	for _, pd := range byPID {
		if pd.MiB < foreignMinMiB {
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
