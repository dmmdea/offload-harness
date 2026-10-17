package gpucards

import (
	"fmt"
	"math"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// HostView is the host's memory as `gpu status`, `gpu status --json` and offload_status show it: the
// reading, the headroom, what the live leases declared (the working set they touch and the commit they
// are charged) and what of that is still to load, and ONE word for how it stands (OK / NEAR / OVER). All
// three surfaces build it here, the way they share the card rows, so a session reading one cannot be told
// something the other contradicts.
//
// The word judges the two lines the grant applies (gpuprobe.HostMemory.Verdict): the available physical
// memory, and the commit limit where the OS enforces one. NEAR is inside the headroom of either, so a lease
// that declares any host RAM waits; OVER is one of them gone (nothing available, or the commit charge at the
// limit). It is a statement about room, not about paging: whether the box is paging now needs the page-file
// growth or the pages-out rate, which no surface here reads, so none of them says it. The grant's own rule
// (gpuprobe.HostRAMAdmitsLoad) is stricter for the load it is asked to admit (it holds a new declaring lease while
// its need plus what running leases have yet to load would pass either line less the headroom), but it
// compares a declaration, so a box can still read NEAR or OVER after a lane it admitted committed more than it
// declared (measured: gpu-lease.md, "Known limits") as well as after something the grant never saw (a process
// outside every lease).
type HostView struct {
	Read bool
	Mem  gpuprobe.HostMemory
	// HeadroomGiB is the configured headroom (gpu_host_ram_headroom_gib).
	HeadroomGiB float64
	// Declared is the sum of the host memory the live leases declared; Pending the part of it their
	// processes have not loaded yet, per quantity (TouchGiB the working set, CommitGiB the commit).
	Declared, Pending gpuprobe.HostLoad
	Verdict           gpuprobe.HostVerdict
}

// NewHostView judges a reading. A reading that could not be taken is verdict "unknown".
func NewHostView(mem gpuprobe.HostMemory, ok bool, headroomGiB float64, declared, pending gpuprobe.HostLoad) HostView {
	v := HostView{Read: ok, Mem: mem, HeadroomGiB: headroomGiB, Declared: declared, Pending: pending, Verdict: gpuprobe.HostUnknown}
	if ok {
		v.Verdict = mem.Verdict(headroomGiB)
	}
	return v
}

// commitEnforced says the reading's commit limit is one the OS enforces (the commit condition is applied to it).
func (v HostView) commitEnforced() bool { return !v.Mem.CommitLimitSoft && v.Mem.CommitLimitGiB > 0 }

// Map is the JSON block (`host_memory`). Only keys are ever added to it.
func (v HostView) Map() map[string]any {
	out := map[string]any{
		"read":                     v.Read,
		"verdict":                  string(v.Verdict),
		"headroom_gib":             round1(v.HeadroomGiB),
		"declared_live_gib":        round1(v.Declared.TouchGiB),
		"declared_live_commit_gib": round1(v.Declared.CommitGiB),
		"pending_gib":              round1(v.Pending.TouchGiB),
		"pending_commit_gib":       round1(v.Pending.CommitGiB),
	}
	if v.Read {
		out["physical_gib"] = round1(v.Mem.PhysicalGiB)
		out["available_gib"] = round1(v.Mem.AvailableGiB)
		out["commit_used_gib"] = round1(v.Mem.CommitUsedGiB)
		out["commit_limit_gib"] = round1(v.Mem.CommitLimitGiB)
		// The most a touched set could ever be admitted at (physical RAM less the headroom), and what a new lease could
		// touch now: the available memory less what running leases have yet to touch and the headroom.
		out["admits_up_to_gib"] = round1(max(v.Mem.PhysicalGiB-v.HeadroomGiB, 0))
		out["touch_room_gib"] = round1(max(v.Mem.AvailableGiB-v.Pending.TouchGiB-v.HeadroomGiB, 0))
		// Whether the commit condition is applied to this host, and the commit a new lease could be charged now.
		out["commit_limit_enforced"] = v.commitEnforced()
		if v.commitEnforced() {
			out["commit_room_gib"] = round1(max(v.Mem.CommitLimitGiB-v.Mem.CommitUsedGiB-v.Pending.CommitGiB-v.HeadroomGiB, 0))
		}
	}
	if why := v.Why(); why != "" {
		out["verdict_why"] = why
	}
	if v.Verdict == gpuprobe.HostOver {
		out["note"] = v.overNote()
	}
	return out
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// Why names the line a NEAR or OVER verdict rests on, with its numbers ("" for any other verdict).
func (v HostView) Why() string {
	if v.Verdict != gpuprobe.HostNear && v.Verdict != gpuprobe.HostOver {
		return ""
	}
	var parts []string
	if v.Mem.AvailableGiB < v.HeadroomGiB {
		parts = append(parts, fmt.Sprintf("%.1f of %.1f GiB physical RAM is available", v.Mem.AvailableGiB, v.Mem.PhysicalGiB))
	}
	if v.commitEnforced() && v.Mem.CommitUsedGiB > v.Mem.CommitLimitGiB-v.HeadroomGiB {
		parts = append(parts, fmt.Sprintf("committed %.1f of the %.1f GiB commit limit", v.Mem.CommitUsedGiB, v.Mem.CommitLimitGiB))
	}
	return fmt.Sprintf("%s, %.1f GiB headroom", strings.Join(parts, "; "), v.HeadroomGiB)
}

func (v HostView) overNote() string {
	return fmt.Sprintf("%s. This reading cannot say whether the box is paging now; "+
		"a lease that declares host RAM waits until there is room again (the available RAM less its need and the %.1f GiB headroom, "+
		"and where the OS enforces a commit limit the commit charge under that limit less the headroom); "+
		"end a lease or stop a kept ComfyUI instance to free it", v.Why(), v.HeadroomGiB)
}

// Line is the terminal line `gpu status` prints.
func (v HostView) Line() string {
	switch v.Verdict {
	case gpuprobe.HostUnknown:
		return "host memory: unknown — it could not be read here (this platform has no reader, or the read failed), so a lease's host-RAM need cannot be checked"
	}
	commit := fmt.Sprintf("commit %.1f of %.1f GiB", v.Mem.CommitUsedGiB, v.Mem.CommitLimitGiB)
	if !v.commitEnforced() {
		commit += " (not a limit this OS enforces: the commit condition is not applied)"
	}
	line := fmt.Sprintf("host memory: %s — %.1f GiB physical, %.1f GiB available, %s", v.Verdict, v.Mem.PhysicalGiB, v.Mem.AvailableGiB, commit)
	switch {
	case v.Declared.Declares():
		line += fmt.Sprintf("; live leases declared %.1f GiB touched, %.1f GiB commit (%.1f GiB touched, %.1f GiB commit still to load)",
			v.Declared.TouchGiB, v.Declared.CommitGiB, v.Pending.TouchGiB, v.Pending.CommitGiB)
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
		return fmt.Sprintf("HOST RAM OVER (%s)", v.Why()), true
	case gpuprobe.HostNear:
		return fmt.Sprintf("host RAM NEAR the limit (%s)", v.Why()), false
	}
	return "", false
}
