package eval

import (
	"math/rand"
	"sort"
)

// BootstrapDeltaMean is a PAIRED bootstrap of the mean difference (candidate −
// incumbent) over aligned per-item scores — the P6 flywheel's adoption metric for a
// candidate planner prompt (e.g. goal-reached 0/1 per trajectory). It returns the
// mean delta and its 95% CI. The SAME fail-closed rule as the AURC gate applies:
// ADOPT only when lo>0 (candidate strictly better), BLOCK on hi<0, else
// inconclusive. n<2 or B<1 => zeros (nothing to conclude).
func BootstrapDeltaMean(inc, cand []float64, B int, seed int64) (delta, lo, hi float64) {
	n := len(inc)
	if n != len(cand) || n < 2 || B < 1 {
		return 0, 0, 0
	}
	rng := rand.New(rand.NewSource(seed))
	reps := make([]float64, B)
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
	pick := func(q float64) float64 {
		idx := int(q * float64(B))
		if idx < 0 {
			idx = 0
		}
		if idx >= B {
			idx = B - 1
		}
		return reps[idx]
	}
	return delta, pick(0.025), pick(0.975)
}
