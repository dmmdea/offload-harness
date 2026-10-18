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
// The word judges the lines the grant applies (gpuprobe.HostMemory.Verdict): the available physical
// memory, the commit charge against physical RAM (the line a commit nobody stated is held to, unless an enforced commit limit
// sits below it), and the commit limit where the OS enforces one. NEAR is inside the headroom of any of them, so a lease judged on that line waits: one that touches
// host RAM when the available memory is, one that states no commit when the room under physical RAM is, one that states its commit
// when the room under the commit limit is (a host that reads NEAR on the physical line alone still admits a lease that states its
// commit); OVER is one of them gone (nothing available, the commit charge above physical RAM, or at the limit), and when it rests on
// the physical line alone the lead says what a lease that states its commit is judged on instead (StatedCommitClause). It is a
// statement about room, not about paging: whether the box is paging now needs the page-file
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

// commitEnforced says the reading's commit limit is one the OS enforces, so a commit a lease states is judged against it. A commit
// nobody stated is held to physical RAM less the headroom on every reading, enforced limit or not, and to the enforced limit less
// the headroom when that sits below physical RAM (unstatedCommitLine).
func (v HostView) commitEnforced() bool { return !v.Mem.CommitLimitSoft && v.Mem.CommitLimitGiB > 0 }

// unstatedCommitLine is the line a commit nobody stated is held to, before the headroom: physical RAM, or the enforced commit
// limit when that sits nearer (the same choice gpuprobe.HostRAMAdmitsLoad makes).
func (v HostView) unstatedCommitLine() float64 {
	if v.commitEnforced() && v.Mem.CommitLimitGiB < v.Mem.PhysicalGiB {
		return v.Mem.CommitLimitGiB
	}
	return v.Mem.PhysicalGiB
}

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
		// The room under the line a lease that states no commit is held to (physical RAM, or the enforced commit limit when that
		// sits below it, with what running leases have yet to touch), on every reading: the figure `gpu reserve --ram N` alone has
		// to fit, beside touch_room_gib.
		out["unstated_commit_room_gib"] = round1(max(v.unstatedCommitLine()-v.Mem.CommitUsedGiB-v.Pending.TouchGiB-v.HeadroomGiB, 0))
		// Whether a commit a lease STATES is judged on this host, and the commit it could state and be charged now.
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
	if clause := v.StatedCommitClause(); clause != "" {
		out["stated_commit_note"] = clause
	}
	return out
}

// StatedCommitClause is what the loud OVER lead adds when the verdict rests on the commit charge above physical RAM: the line a lease
// that states NO commit is held to. A lease that states its commit is not judged on that line (on the commit limit instead, or not at
// all where the OS enforces none), and the rule admits it, so a session reading the lead alone would hold off work the rule admits.
// It is "" for any other verdict, for an OVER that rests on something else (nothing available), and for a reading at or near an
// enforced limit, where a stated commit waits as well.
func (v HostView) StatedCommitClause() string {
	if v.Verdict != gpuprobe.HostOver || !v.Read || v.Mem.CommitUsedGiB <= v.Mem.PhysicalGiB {
		return ""
	}
	if !v.commitEnforced() {
		return "a lease that states its commit is not judged on this line"
	}
	if v.Mem.CommitUsedGiB >= v.Mem.CommitLimitGiB-v.HeadroomGiB {
		return ""
	}
	return fmt.Sprintf("a lease that states its commit is judged on the %.1f GiB commit limit instead", v.Mem.CommitLimitGiB)
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
	switch {
	case v.commitEnforced() && v.Mem.CommitUsedGiB > v.Mem.CommitLimitGiB-v.HeadroomGiB:
		parts = append(parts, fmt.Sprintf("committed %.1f of the %.1f GiB commit limit", v.Mem.CommitUsedGiB, v.Mem.CommitLimitGiB))
	case v.Mem.CommitUsedGiB > v.Mem.PhysicalGiB-v.HeadroomGiB:
		parts = append(parts, fmt.Sprintf("committed %.1f of %.1f GiB physical RAM", v.Mem.CommitUsedGiB, v.Mem.PhysicalGiB))
	}
	return fmt.Sprintf("%s, %.1f GiB headroom", strings.Join(parts, "; "), v.HeadroomGiB)
}

func (v HostView) overNote() string {
	return fmt.Sprintf("%s. This reading cannot say whether the box is paging now; "+
		"a lease that declares host RAM waits until there is room again (the available RAM less its need and the %.1f GiB headroom, "+
		"and the commit charge plus its commit under physical RAM (or an enforced commit limit that sits below it) less the headroom, or, for a commit it states, under the commit limit less the headroom where the OS enforces one); "+
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
		commit += " (not a limit this OS enforces: a commit a lease states is not judged on it; one it does not state is still held to physical RAM)"
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

// withStatedCommitClause is the OVER lead with the clause behind it, " — " apart ("" adds nothing).
func withStatedCommitClause(lead, clause string) string {
	if clause == "" {
		return lead
	}
	return lead + " — " + clause
}

// Lead is the clause that leads offload_status's one-line gpu_lease verdict when the host is over
// ("HOST RAM OVER ..."), or trails it when the host is near; "" otherwise. OVER is stated loudly and
// first because a session reading "free" at the head of that line would conclude the box has room.
func (v HostView) Lead() (lead string, loud bool) {
	switch v.Verdict {
	case gpuprobe.HostOver:
		return withStatedCommitClause(fmt.Sprintf("HOST RAM OVER (%s)", v.Why()), v.StatedCommitClause()), true
	case gpuprobe.HostNear:
		return fmt.Sprintf("host RAM NEAR the limit (%s)", v.Why()), false
	}
	return "", false
}
