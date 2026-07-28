package fleetnode

import (
	"fmt"
	"sync"
	"time"
)

// vram_reclaimable_gb answers the question a dispatcher actually has: how much
// VRAM can this node DELIVER for a job? Neither published number answers it.
// vram_free_gb is what is free right now — it under-counts a node whose own models
// are loaded and could be swapped out. vram_total_gb over-counts every node whose
// card is shared: the measured workstation's desktop holds ~6.5 GiB the harness
// cannot reclaim at any price, so dividing by total schedules jobs that then OOM.
//
// Two obvious mechanisms do not work here:
//
//   - per-process GPU memory: `nvidia-smi --query-compute-apps=...,used_memory`
//     returns [N/A] on Windows (WDDM), which is exactly the node with the shared
//     desktop, so a per-PID sum is unavailable where it matters most;
//   - the footprint store: it records what a RENDER task peaks at, not what the
//     llama-swap text tiers currently hold.
//
// So reclaimable is derived from a BASELINE instead: the used-VRAM observed while
// this harness has nothing of its own loaded. That baseline IS the unreclaimable
// part (desktop, other apps), measured rather than assumed, and it tracks the
// machine over time. Reclaimable is what sits above it.
//
// The rule is deliberately asymmetric, because over-promising costs a failed job
// while under-promising costs only a scheduling opportunity:
//
//   - nothing of ours loaded  -> reclaimable is 0, and this moment becomes the
//     new baseline;
//   - ours loaded, baseline known -> reclaimable = max(0, used - baseline);
//   - ours loaded, no baseline yet -> UNKNOWN. The field is omitted so the
//     dispatcher falls back to free rather than acting on a guess.
//
// It never claims reclaim capacity while holding nothing, so a third party that
// allocated VRAM after the baseline cannot be mistaken for our own model.

// ReclaimInputs is one evaluation's worth of facts.
type ReclaimInputs struct {
	UsedGiB float64
	// OursLoaded is true when llama-swap has any model resident or this node holds
	// the GPU lease — i.e. when there is something we could actually free.
	OursLoaded bool
	// BaselineGiB is the last used-VRAM observed with nothing of ours loaded.
	BaselineGiB  float64
	HaveBaseline bool
}

// ReclaimVerdict is the answer plus how it was reached, so a dispatcher (or an
// operator reading /fleet/health) can tell a measured zero from an unknown.
type ReclaimVerdict struct {
	ReclaimableGiB float64
	Known          bool
	Source         string
}

// Reclaimable applies the rule. Pure.
func Reclaimable(in ReclaimInputs) ReclaimVerdict {
	if !in.OursLoaded {
		return ReclaimVerdict{ReclaimableGiB: 0, Known: true,
			Source: fmt.Sprintf("nothing loaded by this harness; %.1f GiB in use is the unreclaimable baseline", in.UsedGiB)}
	}
	if !in.HaveBaseline {
		return ReclaimVerdict{Known: false,
			Source: "unknown: models are loaded but no idle baseline has been observed yet, so the unreclaimable share is unmeasured"}
	}
	got := in.UsedGiB - in.BaselineGiB
	if got < 0 {
		// Below the baseline: something outside the harness released memory. Report
		// zero rather than a negative, and say so.
		return ReclaimVerdict{ReclaimableGiB: 0, Known: true,
			Source: fmt.Sprintf("in use (%.1f GiB) is below the observed baseline (%.1f GiB); treating as nothing reclaimable", in.UsedGiB, in.BaselineGiB)}
	}
	return ReclaimVerdict{ReclaimableGiB: got, Known: true,
		Source: fmt.Sprintf("%.1f GiB in use minus a %.1f GiB idle baseline", in.UsedGiB, in.BaselineGiB)}
}

// BaselineTracker records the idle baseline over time. Safe for concurrent use:
// the sampler writes it while /fleet/health reads it.
type BaselineTracker struct {
	mu    sync.Mutex
	gib   float64
	at    time.Time
	valid bool
}

// Observe feeds one sample. Only idle moments (nothing of ours loaded) move the
// baseline, so a busy node keeps the last honest measurement instead of drifting
// upward as our own models load.
func (b *BaselineTracker) Observe(usedGiB float64, oursLoaded bool, now time.Time) {
	if oursLoaded || usedGiB < 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gib, b.at, b.valid = usedGiB, now, true
}

// Baseline returns the last idle observation.
func (b *BaselineTracker) Baseline() (gib float64, at time.Time, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gib, b.at, b.valid
}

// Verdict evaluates the current sample against the tracked baseline.
func (b *BaselineTracker) Verdict(usedGiB float64, oursLoaded bool) ReclaimVerdict {
	base, _, ok := b.Baseline()
	return Reclaimable(ReclaimInputs{
		UsedGiB: usedGiB, OursLoaded: oursLoaded, BaselineGiB: base, HaveBaseline: ok,
	})
}
