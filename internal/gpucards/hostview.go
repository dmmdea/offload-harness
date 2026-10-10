package gpucards

import (
	"fmt"
	"math"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// HostView is the host's memory as `gpu status`, `gpu status --json` and offload_status show it: the
// reading, the headroom, what the live leases declared and what of that is still to load, and ONE word
// for how it stands (OK / NEAR / OVER). All three surfaces build it here, the way they share the card
// rows, so a session reading one cannot be told something the other contradicts.
//
// OVER is committed memory above physical RAM. That is a statement about what the OS has promised, not
// about paging: whether the box is paging now needs the page-file growth or the pages-out rate, which no
// surface here reads, so none of them says it. The grant's own rule (gpuprobe.HostRAMAdmits) is stricter
// than OVER for the load it is asked to admit (it holds a new declaring lease while commit + the lease's
// DECLARED need + pending would pass physical RAM less the headroom), but it compares a declaration, so a box
// can still read OVER after a lane it admitted committed more than it declared (measured: gpu-lease.md,
// "Known limits") as well as after something the grant never saw (a process outside every lease).
type HostView struct {
	Read bool
	Mem  gpuprobe.HostMemory
	// HeadroomGiB is the configured headroom (gpu_host_ram_headroom_gib).
	HeadroomGiB float64
	// DeclaredGiB is the sum of the host RAM the live leases declared; PendingGiB the part of it their
	// processes have not loaded yet.
	DeclaredGiB, PendingGiB float64
	Verdict                 gpuprobe.HostVerdict
}

// NewHostView judges a reading. A reading that could not be taken is verdict "unknown".
func NewHostView(mem gpuprobe.HostMemory, ok bool, headroomGiB, declaredGiB, pendingGiB float64) HostView {
	v := HostView{Read: ok, Mem: mem, HeadroomGiB: headroomGiB, DeclaredGiB: declaredGiB, PendingGiB: pendingGiB, Verdict: gpuprobe.HostUnknown}
	if ok {
		v.Verdict = mem.Verdict(headroomGiB)
	}
	return v
}

// Map is the JSON block (`host_memory`). Only keys are ever added to it.
func (v HostView) Map() map[string]any {
	out := map[string]any{
		"read":              v.Read,
		"verdict":           string(v.Verdict),
		"headroom_gib":      round1(v.HeadroomGiB),
		"declared_live_gib": round1(v.DeclaredGiB),
		"pending_gib":       round1(v.PendingGiB),
	}
	if v.Read {
		out["physical_gib"] = round1(v.Mem.PhysicalGiB)
		out["available_gib"] = round1(v.Mem.AvailableGiB)
		out["commit_used_gib"] = round1(v.Mem.CommitUsedGiB)
		out["commit_limit_gib"] = round1(v.Mem.CommitLimitGiB)
		out["admits_up_to_gib"] = round1(max(v.Mem.PhysicalGiB-v.HeadroomGiB, 0))
	}
	if v.Verdict == gpuprobe.HostOver {
		out["note"] = v.overNote()
	}
	return out
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func (v HostView) overNote() string {
	return fmt.Sprintf("committed memory (%.1f GiB) exceeds the %.1f GiB of physical RAM. This reading cannot say whether the box is paging now; "+
		"a lease that declares host RAM waits until commit is back under %.1f GiB (physical RAM less the %.1f GiB headroom); "+
		"end a lease or stop a kept ComfyUI instance to free it",
		v.Mem.CommitUsedGiB, v.Mem.PhysicalGiB, max(v.Mem.PhysicalGiB-v.HeadroomGiB, 0), v.HeadroomGiB)
}

// Line is the terminal line `gpu status` prints.
func (v HostView) Line() string {
	switch v.Verdict {
	case gpuprobe.HostUnknown:
		return "host memory: unknown — it could not be read here (this platform has no reader, or the read failed), so a lease's host-RAM need cannot be checked"
	}
	line := fmt.Sprintf("host memory: %s — %.1f GiB physical, %.1f GiB available, commit %.1f of %.1f GiB",
		v.Verdict, v.Mem.PhysicalGiB, v.Mem.AvailableGiB, v.Mem.CommitUsedGiB, v.Mem.CommitLimitGiB)
	switch {
	case v.DeclaredGiB > 0:
		line += fmt.Sprintf("; live leases declared %.1f GiB (%.1f GiB still to load)", v.DeclaredGiB, v.PendingGiB)
	default:
		line += "; no live lease declared host RAM"
	}
	line += fmt.Sprintf("; headroom %.1f GiB", v.HeadroomGiB)
	if v.Verdict == gpuprobe.HostOver {
		line += "\n  " + v.overNote()
	}
	return line
}

// Lead is the clause that leads offload_status's one-line gpu_lease verdict when the host is over
// ("HOST RAM OVER ..."), or trails it when the host is near; "" otherwise. OVER is stated loudly and
// first because a session reading "free" at the head of that line would conclude the box has room.
func (v HostView) Lead() (lead string, loud bool) {
	switch v.Verdict {
	case gpuprobe.HostOver:
		return fmt.Sprintf("HOST RAM OVER (committed memory %.1f GiB exceeds the %.1f GiB of physical RAM)", v.Mem.CommitUsedGiB, v.Mem.PhysicalGiB), true
	case gpuprobe.HostNear:
		return fmt.Sprintf("host RAM NEAR the limit (committed %.1f of %.1f GiB physical, %.1f GiB headroom)", v.Mem.CommitUsedGiB, v.Mem.PhysicalGiB, v.HeadroomGiB), false
	}
	return "", false
}
