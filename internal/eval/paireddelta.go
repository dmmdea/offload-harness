package eval

import (
	"math/rand"
	"sort"
)

// bootstrapReps is the core of the paired bootstrap: B replicates of the mean difference
// (candidate - incumbent) over aligned per-item scores, both systems resampled at the SAME index.
// It returns the replicates sorted, and delta, their mean (summed before sorting, so the float
// result does not depend on the sort). ok is false when there is nothing to conclude: unaligned
// input, fewer than two items, or B < 1.
func bootstrapReps(inc, cand []float64, B int, seed int64) (reps []float64, delta float64, ok bool) {
	n := len(inc)
	if n != len(cand) || n < 2 || B < 1 {
		return nil, 0, false
	}
	rng := rand.New(rand.NewSource(seed))
	reps = make([]float64, B)
	for b := 0; b < B; b++ {
		var si, sc float64
		for i := 0; i < n; i++ {
			idx := rng.Intn(n) // paired: both systems resampled at the SAME index
			si += inc[idx]
			sc += cand[idx]
		}
		reps[b] = (sc - si) / float64(n)
	}
	for _, v := range reps {
		delta += v
	}
	delta /= float64(B)
	sort.Float64s(reps)
	return reps, delta, true
}

// percentile reads the q point of sorted replicates (index int(q*B), clamped to the ends).
func percentile(reps []float64, q float64) float64 {
	B := len(reps)
	idx := int(q * float64(B))
	if idx < 0 {
		idx = 0
	}
	if idx >= B {
		idx = B - 1
	}
	return reps[idx]
}

// BootstrapDeltaMean is a PAIRED bootstrap of the mean difference (candidate −
// incumbent) over aligned per-item scores — the P6 flywheel's adoption metric for a
// candidate planner prompt (e.g. goal-reached 0/1 per trajectory). It returns the
// mean delta and its 95% CI. The SAME fail-closed rule as the AURC gate applies:
// ADOPT only when lo>0 (candidate strictly better), BLOCK on hi<0, else
// inconclusive. n<2 or B<1 => zeros (nothing to conclude).
//
// Its results are pinned by a test: the measurement gate reads other percentiles of the same
// replicates through bootstrapReps, and this function must not move for its other callers.
func BootstrapDeltaMean(inc, cand []float64, B int, seed int64) (delta, lo, hi float64) {
	reps, delta, ok := bootstrapReps(inc, cand, B, seed)
	if !ok {
		return 0, 0, 0
	}
	return delta, percentile(reps, 0.025), percentile(reps, 0.975)
}
